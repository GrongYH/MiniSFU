package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"io"
	"mini-sfu/internal/buffer"
	"sync"
	"time"
)

type Receiver interface {
	TrackID() string
	StreamID() string
	Codec() webrtc.RTPCodecParameters
	Kind() webrtc.RTPCodecType
	SetRTCPCh(rtcp chan []rtcp.Packet)
	OnCloseHandler(f func())
	AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer)
	AddDownTrack(track *DownTrack, bestQualityFirst bool)
}

type WebRTCReceiver struct {
	rtcpMu    sync.Mutex
	closeOnce sync.Once

	peerID     string
	trackID    string
	streamID   string
	kind       webrtc.RTPCodecType
	bandwidth  uint64
	lastPli    int64
	stream     string
	receiver   *webrtc.RTPReceiver
	codec      webrtc.RTPCodecParameters
	rtcpCh     chan []rtcp.Packet
	locks      [3]sync.Mutex
	buffers    [3]*buffer.Buffer
	upTracks   [3]*webrtc.TrackRemote
	downTracks [3][]*DownTrack

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

func (w *WebRTCReceiver) AddDownTrack(track *DownTrack, bestQualityFirst bool) {
}

func (w *WebRTCReceiver) OnCloseHandler(f func()) {
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
