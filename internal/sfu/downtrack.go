package sfu

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mini-sfu/internal/buffer"
	"mini-sfu/internal/log"

	"github.com/pion/rtcp"
	"github.com/pion/transport/v2/packetio"
	"github.com/pion/webrtc/v3"
)

// DownTrackType 决定了track的类型
type DownTrackType int

const (
	SimpleDownTrack DownTrackType = iota + 1
	SimulcastDownTrack
)

/*
DownTrack 实现TrackLocal，是用于将数据包写入SFU Subscriber的Track，
该Track处理simple、simulcast Publisher 的数据包。

最新的Pion中，移除了track，而是用TrackRemote和TrackLocal替代。
TrackRemote是在OnTrack时接收远端的track，
TrackLocal用来发送本地的Track，
在sfu中，接收远端track后，要转换成本地track再发送给其他Peer，
downTrack就是实现一个TrackLocal，代码结构上主要参考pion中的track_loacal_static.go
*/
type DownTrack struct {
	id          string
	peerID      string
	bound       atomicBool
	mine        string
	ssrc        uint32
	streamID    string
	rid         string
	payloadType uint8

	sequencer *sequencer
	trackType DownTrackType
	skipFB    int64
	payload   []byte

	spatialLayer int32

	enabled  atomicBool
	reSync   atomicBool
	snOffset uint16
	tsOffset uint32
	lastSSRC uint32
	lastSN   uint16
	lastTS   uint32

	simulcast       simulcastTrackHelpers
	maxSpatialLayer int64

	codec          webrtc.RTPCodecCapability
	transceiver    *webrtc.RTPTransceiver
	receiver       Receiver
	writeStream    webrtc.TrackLocalWriter
	onCloseHandler func()
	onBind         func()
	closeOnce      sync.Once

	// Report helpers
	octetCount   uint32
	packetCount  uint32
	maxPacketTs  uint32
	lastPacketMs int64
}

// NewDownTrack 需要实现 TrackLocal的方法，包括Bind，Unbind，ID，RID，StreamID， Kind
func NewDownTrack(c webrtc.RTPCodecCapability, r Receiver, peerID string) (*DownTrack, error) {
	return &DownTrack{
		id:       r.TrackID(),
		peerID:   peerID,
		streamID: r.StreamID(),
		receiver: r,
		codec:    c,
	}, nil
}

// Bind 协商完成后，PeerConnection调用Bind，表明请求的编码参数被RemotePeer支持。
// 因此在Bind中需要设定ssrc,payloadType等
func (d *DownTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	// 有关协商编解码器的信息，在SDP中，对应a=rtpmap,a=rtcp-fb,a=fmtp这几行
	parameters := webrtc.RTPCodecParameters{RTPCodecCapability: d.codec}
	// 协商结束后，双方PeerConnection都支持的编码器信息
	haystack := t.CodecParameters()
	if codec, err := codecParametersFuzzySearch(parameters, haystack); err == nil {
		// 将一些编解码器参数直接存到downTrack里面，方便后续发包
		d.mine = strings.ToLower(codec.MimeType)
		// 同时要把ssrc 和 payloadType和该track绑定，这也是Bind函数的作用之一
		d.ssrc = uint32(t.SSRC())
		d.payloadType = uint8(codec.PayloadType)
		// 真正的发包要用到 writeStream.WriteRTP(&pkt.Header, pkt.Payload)，所以在这里保存一下writeStream
		d.writeStream = t.WriteStream()

		d.reSync.set(true)
		d.enabled.set(true)
		// 注册rtcp包的处理函数
		if rr := bufferFactory.GetOrNew(packetio.RTCPBufferPacket, uint32(t.SSRC())).(*buffer.RTCPReader); rr != nil {
			rr.OnPacket(func(pkt []byte) {
				d.handleRTCP(pkt)
			})
		}

		if strings.HasPrefix(d.codec.MimeType, "video/") {
			d.sequencer = newSequencer()
		}
		// onBind的功能是在协商成功时，给对端发送源描述rtcp报文的。(真正的逻辑在router中)
		d.onBind()
		d.bound.set(true)
		return codec, nil
	}
	// unable to start track, codec of the track is not supported by remote
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

// Unbind 当track停止发送时，会调用Unbind
func (d *DownTrack) Unbind(_ webrtc.TrackLocalContext) error {
	d.bound.set(false)
	return nil
}

// ID Track唯一标识符
func (d *DownTrack) ID() string {
	return d.id
}

func (d *DownTrack) RID() string {
	return ""
}

// Codec 返回当前Track的编解码能力
func (d *DownTrack) Codec() webrtc.RTPCodecCapability {
	return d.codec
}

// StreamID 是track所属的组
func (d *DownTrack) StreamID() string {
	return d.streamID
}

// Kind controls if this TrackLocal is audio or video
func (d *DownTrack) Kind() webrtc.RTPCodecType {
	switch {
	case strings.HasPrefix(d.codec.MimeType, "audio/"):
		return webrtc.RTPCodecTypeAudio
	case strings.HasPrefix(d.codec.MimeType, "video/"):
		return webrtc.RTPCodecTypeVideo
	default:
		return webrtc.RTPCodecType(0)
	}
}

func (d *DownTrack) OnCloseHandler(f func()) {
	d.onCloseHandler = f
}

func (d *DownTrack) OnBind(f func()) {
	d.onBind = f
}

func (d *DownTrack) Close() {
	d.closeOnce.Do(func() {
		log.Debugf("Closing sender %s", d.peerID)
		if d.payload != nil {
			packetFactory.Put(d.payload)
		}
		if d.onCloseHandler != nil {
			d.onCloseHandler()
		}
	})
}

func (d *DownTrack) CreateSenderReport() *rtcp.SenderReport {
	if !d.bound.get() {
		return nil
	}
	now := time.Now().UnixNano()
	nowNTP := timeToNtp(now)
	//最后一次发送包的时间
	lastPktMs := atomic.LoadInt64(&d.lastPacketMs)
	// 这里的timestamp是偏移后的ts
	maxPktTs := atomic.LoadUint32(&d.lastTS)
	diffTs := uint32((now/1e6)-lastPktMs) * d.codec.ClockRate / 1000
	octets := atomic.LoadUint32(&d.octetCount)
	packets := atomic.LoadUint32(&d.packetCount)
	//log.Debugf("diffTs:%d, octets:%d, packets:%d", diffTs, octets, packets)
	return &rtcp.SenderReport{
		SSRC:        d.ssrc,
		NTPTime:     nowNTP,
		RTPTime:     maxPktTs + diffTs,
		PacketCount: packets,
		OctetCount:  octets,
	}
}

func (d *DownTrack) CreateSourceDescriptionChunks() []rtcp.SourceDescriptionChunk {
	if !d.bound.get() {
		return nil
	}
	return []rtcp.SourceDescriptionChunk{
		{
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESCNAME,
				Text: d.streamID,
			}},
		}, {
			Source: d.ssrc,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESType(15),
				Text: d.transceiver.Mid(),
			}},
		},
	}
}

func (d *DownTrack) SetInitialLayers(spatialLayer int64) {
	// 低16位存当前layer，高16位存目标layer
	atomic.StoreInt32(&d.spatialLayer, int32(spatialLayer<<16)|int32(spatialLayer))
}

// WriteRTP 发包，分为简单模式和大小流模式
func (d *DownTrack) WriteRTP(p buffer.ExtPacket) error {
	if !d.enabled.get() || !d.bound.get() {
		return nil
	}
	switch d.trackType {
	case SimpleDownTrack:
		return d.writeSimpleRTP(p)
	case SimulcastDownTrack:
		return d.writeSimulcastRTP(p)
	}
	return nil
}

func (d *DownTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	d.transceiver = transceiver
}

func (d *DownTrack) writeSimpleRTP(extPkt buffer.ExtPacket) error {
	//是否需要重新同步新源，第一次发包肯定是需要同步的
	if d.reSync.get() {
		if d.Kind() == webrtc.RTPCodecTypeVideo {
			//再次同步新源时，第一帧需要是个关键帧
			if !extPkt.KeyFrame {
				d.receiver.SendRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{SenderSSRC: d.ssrc, MediaSSRC: extPkt.Packet.SSRC},
				})
				return nil
			}
		}

		if d.lastSN != 0 {
			// 发包的包序号应该是上一个包+1，由于中间可能有一段时间没有转发包，实际的包序号是不连续的。
			// reSync的作用就是为了让包序号连续。
			// 因此需要计算当再次同步时，这次转发的包和上一次转发包的包序号偏移量。
			d.snOffset = extPkt.Packet.SequenceNumber - d.lastSN - 1
			d.tsOffset = extPkt.Packet.Timestamp - d.lastTS - 1
		}

		atomic.StoreUint32(&d.lastSSRC, extPkt.Packet.SSRC)
		d.reSync.set(false)
	}
	atomic.AddUint32(&d.octetCount, uint32(len(extPkt.Packet.Payload)))
	atomic.AddUint32(&d.packetCount, 1)
	//如果是第一次同步源，d.snOffset为0
	newSN := extPkt.Packet.SequenceNumber - d.snOffset
	newTS := extPkt.Packet.Timestamp - d.tsOffset
	if d.sequencer != nil {
		d.sequencer.push(extPkt.Packet.SequenceNumber, newSN, newTS, 0, extPkt.Head)
	}
	if extPkt.Head {
		d.lastSN = newSN
		d.lastTS = newTS
		d.lastPacketMs = extPkt.Arrival / 1e6
	}
	// 修改rtp报头的timestamp，sequenceNumber和ssrc
	hdr := extPkt.Packet.Header
	hdr.PayloadType = d.payloadType
	hdr.Timestamp = newTS
	hdr.SequenceNumber = newSN
	hdr.SSRC = d.ssrc

	_, err := d.writeStream.WriteRTP(&hdr, extPkt.Packet.Payload)
	return err
}

func (d *DownTrack) writeSimulcastRTP(extPkt buffer.ExtPacket) error {
	// 先检查数据包SSRC是否与之前不同 如果不同，说明视频源已经改变
	reSync := d.reSync.get()
	if d.lastSSRC != extPkt.Packet.SSRC || reSync {
		layer := atomic.LoadInt32(&d.spatialLayer)
		currentLayer := uint16(layer)
		targetLayer := uint16(layer >> 16)

		//如果层没有变化，但ssrc和之前不同，这种情况不应该存在
		if currentLayer == targetLayer && d.lastSSRC != 0 && !reSync {
			return nil
		}

		//如果是重新同步该层的视频源，记录该数据包到达时间
		if reSync && d.simulcast.lastTSCalc != 0 {
			d.simulcast.lastTSCalc = extPkt.Arrival
		}

		//等待关键帧同步新源
		if !extPkt.KeyFrame {
			// 数据包不是关键帧，丢弃它，并发送PLI请求IDR帧
			d.receiver.SendRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{SenderSSRC: d.ssrc, MediaSSRC: extPkt.Packet.SSRC},
			})
			return nil
		}

		// 如果切换层，在receiver中移除掉当前layer的对应的downTrack
		if currentLayer != targetLayer {
			d.receiver.DeleteDownTrack(int(currentLayer), d.peerID)
		}
		//更新当前层
		atomic.StoreInt32(&d.spatialLayer, int32(targetLayer)|int32(targetLayer))
		d.reSync.set(false)
	}
	//  切换到新流时，可能会发生此关键帧的时间戳小于已经发送到远端的最大时间戳。
	//	如果是这样，应该用一个额外的偏移量来“修复”这个流
	//  计算旧数据包和当前数据包之间经过的时间
	if d.simulcast.lastTSCalc != 0 && d.lastSSRC != extPkt.Packet.SSRC {
		tDiff := (extPkt.Arrival - d.simulcast.lastTSCalc) / 1e6
		td := uint32((tDiff * 90) / 1000)
		if td == 0 {
			td = 1
		}
		d.tsOffset = extPkt.Packet.Timestamp - (d.lastTS + td) //tsOffset越小，newTS越大
		d.snOffset = extPkt.Packet.SequenceNumber - d.lastSN - 1
	} else if d.simulcast.lastTSCalc == 0 {
		d.lastTS = extPkt.Packet.Timestamp
		d.lastSN = extPkt.Packet.SequenceNumber
	}

	newSN := extPkt.Packet.SequenceNumber - d.snOffset
	newTS := extPkt.Packet.Timestamp - d.tsOffset

	if d.sequencer != nil {
		layer := atomic.LoadInt32(&d.spatialLayer)
		d.sequencer.push(extPkt.Packet.SequenceNumber, newSN, newTS, uint8(layer), extPkt.Head)
	}
	atomic.AddUint32(&d.octetCount, uint32(len(extPkt.Packet.Payload)))
	atomic.AddUint32(&d.packetCount, 1)
	if extPkt.Head {
		d.lastSN = newSN
		d.lastTS = newTS
		atomic.StoreInt64(&d.lastPacketMs, time.Now().UnixNano()/1e6)
		atomic.StoreUint32(&d.lastTS, newTS)
	}
	d.simulcast.lastTSCalc = extPkt.Arrival
	d.lastSSRC = extPkt.Packet.SSRC

	extPkt.Packet.SequenceNumber = newSN
	extPkt.Packet.Timestamp = newTS
	extPkt.Packet.Header.SSRC = d.ssrc
	extPkt.Packet.Header.PayloadType = d.payloadType

	_, err := d.writeStream.WriteRTP(&extPkt.Packet.Header, extPkt.Packet.Payload)
	return err
}

// handleRTCP 处理来自sub.pc的各种RTCP报文
func (d *DownTrack) handleRTCP(bytes []byte) {
	if !d.enabled.get() {
		return
	}

	pkts, err := rtcp.Unmarshal(bytes)
	if err != nil {
		log.Errorf("Unmarshal rtcp receiver packets err: %v", err)
	}
	// 每次收到解析的报文，只允许发送一次pli和fir，避免造成资源的浪费
	pliOnce := true
	firOnce := true
	var (
		maxRatePacketLoss  uint8
		expectedMinBitrate uint64
	)
	var fwdPkts []rtcp.Packet
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			//PLI报文，转发给发送端
			if pliOnce {
				log.Debugf("recv PLI...")
				p.MediaSSRC = d.lastSSRC
				p.SenderSSRC = d.lastSSRC
				fwdPkts = append(fwdPkts, p)
				pliOnce = false
			}
			log.Debugf("PLI Packet Forward")
		case *rtcp.FullIntraRequest:
			log.Debugf("recv FIR...")
			//PIR报文，转发给发送端
			if firOnce {
				p.MediaSSRC = d.lastSSRC
				p.SenderSSRC = d.ssrc
				fwdPkts = append(fwdPkts, p)
				firOnce = false
			}
		case *rtcp.ReceiverEstimatedMaximumBitrate:
			//log.Infof("down bitrate %d", uint64(p.Bitrate))
			//REMB报文，反馈接收端的带宽情况，记录该带宽值
			if expectedMinBitrate == 0 || expectedMinBitrate > uint64(p.Bitrate) {
				expectedMinBitrate = uint64(p.Bitrate)
			}
		case *rtcp.ReceiverReport:
			//log.Debugf("recv RR...")
			//RR报文，记录所有ReceptionReport里面的最大丢包率
			for _, r := range p.Reports {
				if maxRatePacketLoss == 0 || maxRatePacketLoss < r.FractionLost {
					maxRatePacketLoss = r.FractionLost
				}
			}
		case *rtcp.TransportLayerNack:
			log.Debugf("recv nack...")
			if d.sequencer != nil {
				var nackedPackets []packetMeta
				for _, pair := range p.Nacks {
					nackedPackets = append(nackedPackets, d.sequencer.getSeqNoPairs(pair.PacketList())...)
				}
				if err = d.receiver.RetransmitPackets(d, nackedPackets); err != nil {
					return
				}
			}
		case *rtcp.TransportLayerCC:
			log.Infof("twcc")
		}

		// 获取接收端带宽值和丢包率情况后，进入simulcast切换逻辑
		if d.trackType == SimulcastDownTrack && (maxRatePacketLoss != 0 || expectedMinBitrate != 0) {
			d.handlerLayerChange(maxRatePacketLoss, expectedMinBitrate)
		}

		if len(fwdPkts) > 0 {
			d.receiver.SendRTCP(fwdPkts)
		}
	}
}

// handlerLayerChange 核心的切换逻辑，在何种情况下切换到哪一层
func (d *DownTrack) handlerLayerChange(maxRatePacketLoss uint8, expectedMinBitrate uint64) {
	spatialLayer := atomic.LoadInt32(&d.spatialLayer)
	currentLayer := int64(spatialLayer & 0x0f)
	targetLayer := int64(spatialLayer >> 16)
	log.Infof("switch layer, currentLayer %d, targetLayer %d", currentLayer, targetLayer)
	if targetLayer == currentLayer {
		if time.Now().After(d.simulcast.switchDelay) {
			brs := d.receiver.GetBitrate()
			cbr := brs[currentLayer]

			if maxRatePacketLoss <= 5 {
				// 如果下行带宽是上行带宽是1.5倍，且当前层的质量不是最佳的
				if expectedMinBitrate >= 3*cbr/2 && currentLayer+1 <= d.maxSpatialLayer && currentLayer+1 <= 2 {
					d.SwitchSpatialLayer(currentLayer + 1)
				}
				//5s内不能再切换
				d.simulcast.switchDelay = time.Now().Add(5 * time.Second)
			}

			if maxRatePacketLoss >= 25 {
				// 如果下行带宽时上行带宽的5/8，且当前层不是质量最差的
				if expectedMinBitrate <= 5*cbr/8 && currentLayer > 0 && brs[currentLayer-1] != 0 {
					d.SwitchSpatialLayer(currentLayer - 1)
				}
				//5s内不能再切换
				d.simulcast.switchDelay = time.Now().Add(5 * time.Second)
			}
		}
	}
}

// SwitchSpatialLayer 负责将downTrack添加到目标层，删除当前层的downTrack是在writeSimulcastRTP中进行的
func (d *DownTrack) SwitchSpatialLayer(targetLayer int64) {
	if d.trackType == SimulcastDownTrack {
		layer := atomic.LoadInt32(&d.spatialLayer)
		currentLayer := uint16(layer)
		currentTargetLayer := uint16(layer >> 16)

		//在上一次切换完成之前，或者当前层已经是目标层，不能切换
		if currentLayer != currentTargetLayer || currentLayer == uint16(targetLayer) {
			log.Infof("switching or layer has changed, can not switch now")
			return
		}
		// 将downTrack 挂载到receiver的layer层
		err := d.receiver.SubDownTrack(d, int(targetLayer))
		if err != nil {
			log.Errorf("switch spatial layer failed")
			return
		}
		atomic.StoreInt32(&d.spatialLayer, int32(targetLayer<<16)|int32(currentLayer))
		return
	}
}
