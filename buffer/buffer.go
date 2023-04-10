package buffer

import (
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"io"
	"miniSFU/log"
	"strings"
	"sync"
	"time"
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
	Head     bool
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

	lastPacketRead int

	// 支持的反馈方式
	remb       bool
	nack       bool
	twcc       bool
	audioLevel bool

	// 回调
	onClose      func()
	onAudioLevel func(level uint8)
	feedbackCB   func([]rtcp.Packet)
	feedbackTWCC func(sn uint16, timeNS int64, marker bool)
}

// Options buffer 配置选项
type Options struct {
	BufferTime int
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

	// 如果没有绑定，则需要将该packet加入到pendingPacket队列，在bind时会处理
	if !b.bound {
		packet := make([]byte, len(pkt))
		copy(packet, pkt)
		b.pPackets = append(b.pPackets, pendingPackets{
			packet:      packet,
			arrivalTime: time.Now().UnixNano(),
		})
		return
	}

	b.calc(pkt, time.Now().UnixNano())
	return
}

func (b *Buffer) Read(buff []byte) (n int, err error) {
	for {
		if b.closed.get() {
			err = io.EOF
			return
		}

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

	// 将申请的缓冲区放回Pool中
	b.closeOnce.Do(func() {
		b.closed.set(true)
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

func (b *Buffer) calc(pkt []byte, arrivalTime int64) {

}

func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {

}

func (b *Buffer) buildReceptionReport() rtcp.ReceptionReport {

}

func (b *Buffer) SetSenderReportData(rtpTime uint32, ntpTime uint64) {

}

func (b *Buffer) getRTCP() []rtcp.Packet {

}

func (b *Buffer) GetPacket(buff []byte, sn uint16) (int, error) {

}