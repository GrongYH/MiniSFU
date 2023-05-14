package sfu

import (
	"github.com/gammazero/workerpool"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"io"
	"mini-sfu/internal/buffer"
	"mini-sfu/internal/log"
	"sync"
	"time"
)

type Receiver interface {
	TrackID() string
	StreamID() string
	Codec() webrtc.RTPCodecParameters
	Kind() webrtc.RTPCodecType
	SSRC(layer int) uint32
	AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer)
	AddDownTrack(track *DownTrack, bestQualityFirst bool)
	DeleteDownTrack(layer int, id string)
	SubDownTrack(track *DownTrack, layer int) error
	RetransmitPackets(track *DownTrack, packets []packetMeta) error
	GetBitrate() [3]uint64
	OnCloseHandler(f func())
	SendRTCP(p []rtcp.Packet)
	SetRTCPCh(rtcp chan []rtcp.Packet)
}

type WebRTCReceiver struct {
	rtcpMu    sync.Mutex
	closeOnce sync.Once

	peerID         string
	trackID        string
	streamID       string
	kind           webrtc.RTPCodecType
	bandwidth      uint64
	lastPli        int64
	stream         string
	receiver       *webrtc.RTPReceiver
	codec          webrtc.RTPCodecParameters
	rtcpCh         chan []rtcp.Packet
	locks          [3]sync.Mutex
	buffers        [3]*buffer.Buffer
	upTracks       [3]*webrtc.TrackRemote
	downTracks     [3][]*DownTrack
	nackWorker     *workerpool.WorkerPool
	isSimulcast    bool
	onCloseHandler func()
}

// NewWebRTCReceiver 同一个TrackID的Track共享一个Receiver
func NewWebRTCReceiver(recv *webrtc.RTPReceiver, track *webrtc.TrackRemote, pid string) Receiver {
	return &WebRTCReceiver{
		peerID:      pid,
		receiver:    recv,
		trackID:     track.ID(),
		streamID:    track.StreamID(),
		codec:       track.Codec(),
		kind:        track.Kind(),
		nackWorker:  workerpool.New(1),
		isSimulcast: len(track.RID()) > 0,
	}
}

func (w *WebRTCReceiver) StreamID() string {
	return w.streamID
}

func (w *WebRTCReceiver) TrackID() string {
	return w.trackID
}

func (w *WebRTCReceiver) Codec() webrtc.RTPCodecParameters {
	return w.codec
}

func (w *WebRTCReceiver) Kind() webrtc.RTPCodecType {
	return w.kind
}

func (w *WebRTCReceiver) SSRC(layer int) uint32 {
	if track := w.upTracks[layer]; track != nil {
		return uint32(track.SSRC())
	}
	return 0
}

func (w *WebRTCReceiver) SetRTCPCh(rtcp chan []rtcp.Packet) {
	w.rtcpCh = rtcp
}

// AddUpTrack 添加上行Track
func (w *WebRTCReceiver) AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer) {
	var layer int
	switch track.RID() {
	case fullResolution:
		layer = 2
	case halfResolution:
		layer = 1
	default:
		layer = 0
	}

	w.upTracks[layer] = track
	w.buffers[layer] = buff
	// 一个upTrack对应多个downTrack
	w.downTracks[layer] = make([]*DownTrack, 0, 10)

	// 启动发包流程
	go w.writeRTP(layer)
}

// AddDownTrack 将downTrack添加到指定层
func (w *WebRTCReceiver) AddDownTrack(track *DownTrack, bestQualityFirst bool) {
	layer := 0
	// 如果是simulcast，优先订阅质量最好的层
	if w.isSimulcast {
		for i, t := range w.upTracks {
			// 有可能此时这一层的upTrack还没来，所以暂时还是nil
			if t != nil {
				layer = i
				if !bestQualityFirst {
					// 如果不是质量优先，那随便挂载到哪一层均可
					break
				}
				// 否则挂载到当前已经到来的upTrack里面最好的一层
			}
		}
		track.SetInitialLayers(int64(layer))
		track.maxSpatialLayer = 2
		track.trackType = SimulcastDownTrack
		track.payload = packetFactory.Get().([]byte)
	} else {
		track.SetInitialLayers(0)
		track.trackType = SimpleDownTrack
	}
	// 将downTrack添加到指定层
	w.locks[layer].Lock()
	w.downTracks[layer] = append(w.downTracks[layer], track)
	w.locks[layer].Unlock()
}

// GetBitrate 获取上行码率
func (w *WebRTCReceiver) GetBitrate() [3]uint64 {
	var br [3]uint64
	for i, buff := range w.buffers {
		if buff != nil {
			br[i] = buff.Bitrate()
		}
	}
	return br
}

func (w *WebRTCReceiver) OnCloseHandler(f func()) {
	w.onCloseHandler = f
}

// DeleteDownTrack 将sub id为id的downTrack从receiver的layer层删除
func (w *WebRTCReceiver) DeleteDownTrack(layer int, id string) {
	w.locks[layer].Lock()
	defer w.locks[layer].Unlock()

	idx := -1
	for i, dt := range w.downTracks[layer] {
		if dt.peerID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return
	}

	w.downTracks[layer][idx] = w.downTracks[layer][len(w.downTracks[layer])-1]
	w.downTracks[layer][len(w.downTracks[layer])-1] = nil
	w.downTracks[layer] = w.downTracks[layer][:len(w.downTracks[layer])-1]
}

// SubDownTrack 把downTrack挂载到layer层
func (w *WebRTCReceiver) SubDownTrack(track *DownTrack, layer int) error {
	w.locks[layer].Lock()
	defer w.locks[layer].Unlock()

	if dts := w.downTracks[layer]; dts != nil {
		w.downTracks[layer] = append(dts, track)
	} else {
		return errNoReceiverFound
	}
	return nil
}

// writeRTP 每添加一个UpTrack，就开始启动发包流程
func (w *WebRTCReceiver) writeRTP(layer int) {
	defer func() {
		w.closeOnce.Do(func() {
			go w.closeTracks()
		})
	}()

	var del []int
	for pkt := range w.buffers[layer].PacketChan() {
		w.locks[layer].Lock()
		for i, dt := range w.downTracks[layer] {
			// 该层下所有的downTrack都发该包
			if err := dt.WriteRTP(pkt); err == io.EOF {
				// 该track发包结束，删除该downTrack
				del = append(del, i)
			}
		}

		if len(del) > 0 {
			// 删除已经发包结束的downTrack
			for _, idx := range del {
				w.downTracks[layer][idx] = w.downTracks[layer][len(w.downTracks[layer])-1]
				w.downTracks[layer][len(w.downTracks[layer])-1] = nil
				w.downTracks[layer] = w.downTracks[layer][:len(w.downTracks[layer])-1]
			}
			del = del[:0]
		}

		w.locks[layer].Unlock()
	}
}

// RetransmitPackets 根据偏移后的SN从sequencer里面找到发送包号，并根据发送包号从buffer中取出缓存的packet
func (w *WebRTCReceiver) RetransmitPackets(track *DownTrack, packets []packetMeta) error {
	if w.nackWorker.Stopped() {
		return io.ErrClosedPipe
	}

	w.nackWorker.Submit(func() {
		for _, meta := range packets {
			pktBuff := packetFactory.Get().([]byte)
			buff := w.buffers[meta.layer]
			if buff == nil {
				break
			}
			// 从buffer中获取缓存的packet
			pktLen, err := buff.GetPacket(pktBuff, meta.sourceSeqNo)
			if err != nil {
				if err == io.EOF {
					break
				}
				continue
			}
			var pkt rtp.Packet
			if err = pkt.Unmarshal(pktBuff[:pktLen]); err != nil {
				continue
			}

			// 将packet的元数据修改
			pkt.Header.SequenceNumber = meta.targetSeqNo
			pkt.Header.Timestamp = meta.timestamp

			if _, err = track.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
				log.Errorf("Writing rtx packet err: %v", err)
			}
			packetFactory.Put(pktBuff)
		}
	})
	return nil
}

// closeTracks 关闭Receiver中所有Track
func (w *WebRTCReceiver) closeTracks() {
	// 不同层的downTracks
	for i, layer := range w.downTracks {
		w.locks[i].Lock()
		// 关闭该层的所有Tracks
		for _, dt := range layer {
			dt.Close()
		}
		w.downTracks[i] = w.downTracks[i][:0]
		w.locks[i].Unlock()
	}
	w.nackWorker.Stop()
	if w.onCloseHandler != nil {
		w.onCloseHandler()
	}
}

func (w *WebRTCReceiver) SendRTCP(p []rtcp.Packet) {
	// 如果是pli，且发送间隔小于500ms，则不发送
	if _, ok := p[0].(*rtcp.PictureLossIndication); ok {
		w.rtcpMu.Lock()
		defer w.rtcpMu.Unlock()
		if time.Now().UnixNano()-w.lastPli < 500e6 {
			// 间隔小于500ms，不发送
			return
		}
		w.lastPli = time.Now().UnixNano()
	}
	w.rtcpCh <- p
}
