package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/transport/packetio"
	"mini-sfu/internal/buffer"
	"strings"

	"github.com/pion/webrtc/v3"
)

// DownTrackType 决定了track的类型
type DownTrackType int

const (
	SimpleDownTrack DownTrackType = iota + 1
	SimulcastDownTrack
)

// 最新的Pion中，移除了track，而是用TrackRemote和TrackLocal替代。
// TrackRemote是在OnTrack时接收远端的track，
// TrackLocal用来发送本地的Track，
// 在sfu中，接收远端track后，要转换成本地track再发送给其他Peer，
// downTrack就是实现一个TrackLocal，代码结构上主要参考pion中的track_loacal_static.go

// DownTrack 实现TrackLocal，是用于将数据包写入SFU Subscriber的Track，
// 该Track处理simple、simulcast Publisher 的数据包。
type DownTrack struct {
	id          string
	peerID      string
	bound       atomicBool
	mine        string
	ssrc        uint32
	streamID    string
	rid         string
	payloadType uint8

	//sequencer *sequencer
	trackType DownTrackType
	//skipFB    int64
	//payload   []byte

	//spatialLayer  int32
	//temporalLayer int32

	enabled atomicBool
	reSync  atomicBool
	//snOffset uint16
	//tsOffset uint32
	//lastSSRC uint32
	//lastSN   uint16
	//lastTS   uint32
	//
	//simulcast        simulcastTrackHelpers
	//maxSpatialLayer  int64
	//maxTemporalLayer int64

	codec          webrtc.RTPCodecCapability
	transceiver    *webrtc.RTPTransceiver
	receiver       Receiver
	writeStream    webrtc.TrackLocalWriter
	onCloseHandler func()
	onBind         func()
	//closeOnce      sync.Once

	//// Report helpers
	//octetCount   uint32
	//packetCount  uint32
	//maxPacketTs  uint32
	//lastPacketMs int64
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
			// d.sequencer = newSequencer()
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
	return d.rid
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

func (d *DownTrack) handleRTCP(p []byte) {

}
