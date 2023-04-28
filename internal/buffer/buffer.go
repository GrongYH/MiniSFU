package buffer

import (
	"encoding/binary"
	"io"
	"mini-sfu/internal/log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	maxSN = 1 << 16

	reportDelta = 1e9
)

type pendingPackets struct {
	arrivalTime int64
	packet      []byte
}

type ExtPacket struct {
	Head     bool //标识该报文是不是队列头的第一个报文
	Cycle    uint32
	Arrival  int64
	Packet   rtp.Packet
	Payload  interface{}
	KeyFrame bool
}

type Buffer struct {
	sync.Mutex
	bucket     *Bucket
	codecType  webrtc.RTPCodecType
	videoPool  *sync.Pool
	audioPool  *sync.Pool
	packetChan chan ExtPacket
	pPackets   []pendingPackets
	closeOnce  sync.Once

	mediaSSRC  uint32
	clockRate  uint32
	maxBitrate uint64
	lastReport int64
	twccExt    uint8
	audioExt   uint8
	bound      bool
	closed     atomicBool
	mime       string

	// 支持的反馈方式
	remb       bool
	nack       bool
	twcc       bool
	audioLevel bool

	minPacketProbe   int
	lastPacketRead   int
	maxTemporalLayer int64
	bitrate          uint64
	bitrateHelper    uint64 //帮助计算传输码率
	lastSRNTPTime    uint64
	lastSRRTPTime    uint32
	lastSRRecv       int64 // 上一次收到SR的时间（ns）
	baseSN           uint16
	maxSeqNo         uint16 //RTP数据包中接收到的最大序列号
	cycles           uint32
	lastTransit      uint32

	stats Stats

	latestTimestamp     uint32 // 数据包上最新收到的 RTP 时间戳
	latestTimestampTime int64  // 最新时间戳的时间(ns， 10-9s)

	// 回调
	onClose      func()
	onAudioLevel func(level uint8)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)
}

type Stats struct {
	LastExpected uint32
	LastReceived uint32
	LostRate     float32
	PacketCount  uint32
	Jitter       float64 //RTP数据包到达SFU的时间间隔的统计方差估算
	TotalByte    uint64
}

// Options buffer 配置选项
type Options struct {
	MaxBitRate uint64
}

func NewBuffer(ssrc uint32, vp, ap *sync.Pool) *Buffer {
	b := &Buffer{
		mediaSSRC:  ssrc,
		videoPool:  vp,
		audioPool:  ap,
		packetChan: make(chan ExtPacket, 100),
	}
	return b
}

func (b *Buffer) PacketChan() chan ExtPacket {
	return b.packetChan
}

// Bind 将buffer和receiver的媒体编码与传输参数的配置绑定
func (b *Buffer) Bind(params webrtc.RTPParameters, o Options) {
	b.Lock()
	defer b.Unlock()
	codec := params.Codecs[0]
	b.clockRate = codec.ClockRate
	b.maxBitrate = o.MaxBitRate
	b.mime = strings.ToLower(codec.MimeType)

	//从buffer池中获取缓冲区
	switch {
	case strings.HasPrefix(b.mime, "audio/"):
		b.codecType = webrtc.RTPCodecTypeAudio
		b.bucket = NewBucket(b.audioPool.Get().([]byte), false)
	case strings.HasPrefix(b.mime, "video/"):
		b.codecType = webrtc.RTPCodecTypeVideo
		b.bucket = NewBucket(b.videoPool.Get().([]byte), true)
	default:
		b.codecType = webrtc.RTPCodecType(0)
	}

	for _, ext := range params.HeaderExtensions {
		if ext.URI == sdp.TransportCCURI {
			b.twccExt = uint8(ext.ID)
			break
		}
	}

	// 绑定该buffer支持的反馈方式
	if b.codecType == webrtc.RTPCodecTypeVideo {
		for _, fb := range codec.RTCPFeedback {
			switch fb.Type {
			case webrtc.TypeRTCPFBGoogREMB:
				log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBGoogREMB)
				b.remb = true
			case webrtc.TypeRTCPFBTransportCC:
				log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBTransportCC)
				b.twcc = true
			case webrtc.TypeRTCPFBNACK:
				log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBNACK)
				b.nack = true
			}
		}
	} else if b.codecType == webrtc.RTPCodecTypeAudio {
		for _, h := range params.HeaderExtensions {
			if h.URI == sdp.AudioLevelURI {
				b.audioLevel = true
				b.audioExt = uint8(h.ID)
			}
		}
	}

	// bucket回调
	b.bucket.onLost = func(nacks []rtcp.NackPair, askKeyframe bool) {
		pkts := []rtcp.Packet{&rtcp.TransportLayerNack{
			MediaSSRC: b.mediaSSRC,
			Nacks:     nacks,
		}}

		if askKeyframe {
			pkts = append(pkts, &rtcp.PictureLossIndication{
				MediaSSRC: b.mediaSSRC,
			})
		}

		b.feedbackCB(pkts)
	}

	for _, pp := range b.pPackets {
		b.calc(pp.packet, pp.arrivalTime)
	}
	b.pPackets = nil
	b.bound = true
	log.Debugf("NewBuffer BufferOptions=%v", o)
}

func (b *Buffer) Write(pkt []byte) (n int, err error) {
	b.Lock()
	defer b.Unlock()

	if b.closed.get() {
		err = io.EOF
		return
	}

	// 如果暂时还没有绑定，则需要将该packet加入到pendingPacket队列，在bind时会处理
	if !b.bound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)
		b.pPackets = append(b.pPackets, pendingPackets{
			packet:      packet,
			arrivalTime: time.Now().UnixNano(),
		})
		return
	}

	// 核心逻辑
	b.calc(pkt, time.Now().UnixNano())
	return
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	for {
		if b.closed.get() {
			err = io.EOF
			return
		}
		// 其实也可以不实现？
		b.Lock()
		if b.pPackets != nil && len(b.pPackets) > b.lastPacketRead {
			if len(buff) < len(b.pPackets[b.lastPacketRead].packet) {
				err = errBufferTooSmall
				b.Unlock()
				return
			}

			n = len(b.pPackets[b.lastPacketRead].packet)
			copy(buff, b.pPackets[b.lastPacketRead].packet)
			b.lastPacketRead++
			b.Unlock()
			return
		}
		b.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
}

func (b *Buffer) Close() error {
	b.Lock()
	defer b.Unlock()

	b.closeOnce.Do(func() {
		b.closed.set(true)
		// 将申请的缓冲区放回Buffer Pool中
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeVideo {
			b.videoPool.Put(b.bucket.buf)
		}
		if b.bucket != nil && b.codecType == webrtc.RTPCodecTypeAudio {
			b.audioPool.Put(b.bucket.buf)
		}
		b.onClose()
		close(b.packetChan)
	})
	return nil
}

func (b *Buffer) OnClose(fn func()) {
	b.onClose = fn
}

// calc 主要用于将pkt写入bucket,将extPkt加入packetChan，并计算各种qos相关的数据，拥塞控制、音量
// 计算的数据有Jitter,bitrate
func (b *Buffer) calc(pkt []byte, arrivalTime int64) {
	//			The RTP header has the following format:
	//	0                   1                   2                   3
	//	0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//	|V=2|P|X|  CC   |M|     PT      |       sequence number         |
	//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//	|                           timestamp                           |
	//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//	|           synchronization source (SSRC) identifier            |
	//	+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+
	//	|            contributing source (CSRC) identifiers             |
	//	|                             ....                              |
	//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

	// 获取包序号
	sn := binary.BigEndian.Uint16(pkt[2:4])
	// 如果是第一个包，初始化
	if b.stats.PacketCount == 0 {
		b.baseSN = sn
		b.maxSeqNo = sn
		b.bucket.headSN = sn - 1
		b.lastReport = arrivalTime
	} else if (sn-b.maxSeqNo)&0x8000 == 0 {
		// 发生了回绕，这里的逻辑跟nack.go的push方法一样
		if sn < b.maxSeqNo {
			b.cycles += maxSN
		}
		b.maxSeqNo = sn
	}
	// 否则就是序号小，但后到达的包

	// 记录此时收到的总的包数，以及字节数
	b.stats.TotalByte += uint64(len(pkt))
	b.bitrateHelper += uint64(len(pkt))
	b.stats.PacketCount++

	var p rtp.Packet
	// sn == b.maxSeqNo 表示该包是按序到达的包里面最新的
	if err := p.Unmarshal(b.bucket.addPacket(pkt, sn, sn == b.maxSeqNo)); err != nil {
		return
	}
	ep := ExtPacket{
		Head:    sn == b.maxSeqNo,
		Cycle:   b.cycles,
		Packet:  p,
		Arrival: arrivalTime,
	}

	switch b.mime {
	case "video/vp8":
		vp8Packet := VP8{}
		if err := vp8Packet.Unmarshal(p.Payload); err != nil {
			return
		}
		ep.Payload = vp8Packet
		ep.KeyFrame = vp8Packet.IsKeyFrame
	case "video/h64":
		ep.KeyFrame = isH264Keyframe(p.Payload)
	}

	// 探测25包
	if b.minPacketProbe < 25 {
		if sn < b.baseSN {
			b.baseSN = sn
		}

		if b.mime == "video/vp8" {
			pld := ep.Payload.(VP8)
			mtl := atomic.LoadInt64(&b.maxTemporalLayer)
			if mtl < int64(pld.TID) {
				atomic.StoreInt64(&b.maxTemporalLayer, int64(pld.TID))
			}
		}

		b.minPacketProbe++
	}

	b.packetChan <- ep

	// https://blog.csdn.net/u012478275/article/details/99623110 这篇博客介绍了RTP时间戳
	// 为了使得时间戳单位更为精准，RTP报文中的时间戳计算的单位不是秒之类的单位，而是由采样频率所代替的单位
	// 如果是第一次更新或者时间戳更晚
	if b.latestTimestampTime == 0 || IsLaterTimestamp(p.Timestamp, b.latestTimestamp) {
		b.latestTimestamp = p.Timestamp
		b.latestTimestampTime = arrivalTime
	}

	// 计算到达时刻的RTP时间戳
	arrival := uint32(arrivalTime / 1e6 * int64(b.clockRate/1e3))
	// 传输时间(以RTP时间戳来算)，传输时间t = 到达时间tr -发送时间ts
	transit := arrival - p.Timestamp
	if b.lastTransit != 0 {
		// 传输时间间隔增量（在通过延时控制的拥塞算法中，传输时间间隔增量即拥塞值，横坐标为时间，纵坐标为传输时间，斜率就是拥塞值）
		//  Jitter的统计：
		//  D(i,j) = (Rj - Ri) - (Sj - Si) = (Rj - Sj) - (Ri - Si)
		d := int32(transit - b.lastTransit)
		if d < 0 {
			d = -d
		}

		//  平均Jitter的统计：
		//  J(i) = J(i-1) + (|D(i-1,i)| - J(i-1))/16
		b.stats.Jitter += (float64(d) - b.stats.Jitter) / 16
	}
	b.lastTransit = transit

	if b.twcc {
		// https://blog.jianchihu.net/webrtc-research-transport-cc-rtp-rtcp.html 这篇博客记录了TWCC的RTP扩展头
		// 以0xBEDE固定字段开头, One-Byte Header类型的扩展
		/*
				  0                   1                   2                   3
			      0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
			     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
			     |       0xBE    |    0xDE       |           length=1            |
			     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
			     |  ID   | L=1   |transport-wide sequence number | zero padding  |
			     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		*/
		if ext := p.GetExtension(b.twccExt); ext != nil && len(ext) > 1 {
			b.feedbackTWCC(binary.BigEndian.Uint16(ext[0:2]), arrivalTime, p.Marker)
		}
	}

	if b.audioLevel {
		// An RTP Header Extension for Client-to-Mixer Audio Level Indication
		// https://datatracker.ietf.org/doc/draft-lennox-avt-rtp-audio-level-exthdr/
		// 标志位（V）指示是否认为音频数据包包含语音活动 (1) 或不包含 (0)
		// The form of the audio level extension block:
		//    0                   1
		//    0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5
		//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		//   |  ID   | len=0 |V|   level     |
		//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		if e := p.GetExtension(b.audioExt); e != nil && b.onAudioLevel != nil {
			ext := rtp.AudioLevelExtension{}
			if err := ext.Unmarshal(e); err == nil {
				b.onAudioLevel(ext.Level)
			}
		}
	}

	diff := arrivalTime - b.lastReport
	// 每秒计算一次带宽
	if diff >= reportDelta {
		bitrate := (8 * b.bitrateHelper * uint64(reportDelta)) / uint64(diff)
		atomic.StoreUint64(&b.bitrate, bitrate)
		b.feedbackCB(b.getRTCP())
		b.lastReport = arrivalTime
		b.bitrateHelper = 0
	}
}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := b.bitrate
	log.Debugf("bitrate is %d", br)

	if b.stats.LostRate < 0.02 {
		br = uint64(float64(br)*1.09) + 2000
	}
	if b.stats.LostRate > 0.1 {
		br = uint64(float64(br) * float64(1-0.5*b.stats.LostRate))
	}
	if br > b.maxBitrate {
		br = b.maxBitrate
	}
	if br < 100000 {
		br = 100000
	}
	b.stats.TotalByte = 0
	return &rtcp.ReceiverEstimatedMaximumBitrate{
		Bitrate: float32(br),
		SSRCs:   []uint32{b.mediaSSRC},
	}
}

func (b *Buffer) buildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := b.cycles | uint32(b.maxSeqNo)
	expected := extMaxSeq - uint32(b.baseSN) + 1
	lost := uint32(0)
	// 丢失的包 = 预期发送的包的数量 - 实际统计的包的数量
	if b.stats.PacketCount < expected && b.stats.PacketCount != 0 {
		lost = expected - b.stats.PacketCount
	}
	// 计算 这一次预期发送的包 和 上一次预期发送的包 的差值
	expectedInterval := expected - b.stats.LastExpected
	b.stats.LastExpected = expected

	// 计算 这一次实际发送的包 和 上一次实际发送的包 的差值
	receivedInterval := b.stats.PacketCount - b.stats.LastReceived
	b.stats.LastReceived = b.stats.PacketCount

	// 两次报告之间，丢失的报文数量
	lostInterval := expectedInterval - receivedInterval
	b.stats.LostRate = float32(lostInterval) / float32(expectedInterval)

	// 丢包率8bits的数据段，计算方法为，loss fraction=lost rate x 256.
	// 例如，丢包率为25%，该字段为25%*256=64
	var fracLost uint8
	if expectedInterval != 0 && lostInterval > 0 {
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}

	// 接收到SR报文的时刻与发送该RR报文时刻的时间差值，单位时间是1/65536秒，即1s是65536个dlsr
	// 如果没有收到SR报文，该字段为0.
	var dlsr uint32
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		// 秒为单位的在高16位
		dlsr = (delayMS / 1e3) << 16
		// 毫秒部分转换成秒，再乘以65536
		dlsr |= ((delayMS % 1e3) / 1e3) * 65536
	}
	rr := rtcp.ReceptionReport{
		SSRC:               b.mediaSSRC,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(b.stats.Jitter),
		LastSenderReport:   uint32(b.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}
	return rr
}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.Lock()
	b.lastSRRTPTime = rtpTime
	b.lastSRNTPTime = ntpTime
	b.lastSRRecv = time.Now().UnixNano()
	b.Unlock()
}

func (b *Buffer) GetSenderReportData(rtpTime uint32, ntpTime uint64, lastReceivedTimeInNanosSinceEpoch int64) {
	rtpTime = atomic.LoadUint32(&b.lastSRRTPTime)
	ntpTime = atomic.LoadUint64(&b.lastSRNTPTime)
	lastReceivedTimeInNanosSinceEpoch = atomic.LoadInt64(&b.lastSRRecv)
	return
}

func (b *Buffer) getRTCP() []rtcp.Packet {
	var pkts []rtcp.Packet
	pkts = append(pkts, &rtcp.ReceiverReport{
		SSRC:    b.mediaSSRC,
		Reports: []rtcp.ReceptionReport{b.buildReceptionReport()},
	})

	if b.remb && !b.twcc {
		pkts = append(pkts, b.buildREMBPacket())
	}
	return pkts
}

func (b *Buffer) GetPacket(buff []byte, sn uint16) (int, error) {
	b.Lock()
	defer b.Unlock()
	if b.closed.get() {
		return 0, io.EOF
	}
	return b.bucket.getPacket(buff, sn)
}

func (b *Buffer) MaxTemporalLayer() int64 {
	return atomic.LoadInt64(&b.maxTemporalLayer)
}

func (b *Buffer) OnTransportWideCC(fn func(sn uint16, timeNS int64, marker bool)) {
	b.feedbackTWCC = fn
}

func (b *Buffer) OnFeedback(fn func(fb []rtcp.Packet)) {
	b.feedbackCB = fn
}

func (b *Buffer) OnAudioLevel(fn func(level uint8)) {
	b.onAudioLevel = fn
}

func (b *Buffer) GetMediaSSRC() uint32 {
	return b.mediaSSRC
}

func (b *Buffer) GetClockRate() uint32 {
	return b.clockRate
}

// IsTimestampWrapAround 如果从 timestamp1 到 timestamp2 发生回绕，则返回 true
func IsTimestampWrapAround(timestamp1 uint32, timestamp2 uint32) bool {
	return (timestamp1&0xC000000 == 0) && (timestamp2&0xC000000 == 0xC000000)
}

// IsLaterTimestamp 考虑到时间戳回绕，如果timestamp1晚于timestamp2，则返回true
func IsLaterTimestamp(timestamp1 uint32, timestamp2 uint32) bool {
	if timestamp1 > timestamp2 {
		if IsTimestampWrapAround(timestamp2, timestamp1) {
			return false
		}
		return true
	}
	if IsTimestampWrapAround(timestamp1, timestamp2) {
		return true
	}
	return false
}
