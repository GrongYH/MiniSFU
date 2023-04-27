package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/buffer"
	"mini-sfu/internal/log"
	"sync"
)

type RouterConfig struct {
}

type Router interface {
	ID() string
	AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool)
	PubDownTracks(s *Subscriber, r Receiver) error
	Stop()
}

// Router manager a track rtp/rtcp router
type router struct {
	sync.RWMutex
	id     string
	config RouterConfig

	session *Session
	pc      *webrtc.PeerConnection

	rtcpCh    chan []rtcp.Packet
	twcc      *buffer.Responder
	receivers map[string]Receiver
}

// NewRouter for routing rtp/rtcp packets
func newRouter(tid string, pc *webrtc.PeerConnection, session *Session, config RouterConfig) Router {
	r := &router{
		id:        tid,
		pc:        pc,
		session:   session,
		config:    config,
		receivers: make(map[string]Receiver),
		rtcpCh:    make(chan []rtcp.Packet, 10),
	}
	return r
}

func (r *router) ID() string {
	return r.id
}

func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool) {
	r.Lock()
	defer r.Unlock()

	publish := false
	trackID := track.ID()
	rid := track.RID()

	//这里获取了之前init函数中，new出来的buffer和rtcpReader
	//需要注意的是，开启大小流后，三层的streamId、trackId是一样的，但是rid和ssrc是不同的，rid一般是f、h、q
	//因此这里同一个trackId的不同层，获取的buffer是不一样的
	rtcpReader, buff := bufferFactory.GetBufferPair(uint32(track.SSRC()))

	buff.OnFeedback(func(fb []rtcp.Packet) {
		r.rtcpCh <- fb
	})

	//如果是视频track，创建twcc计算器，并设置回调，当计算器生成twcc包就会回调
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		if r.twcc == nil {
			// 一个Router管理多个Receiver，多个Receiver用同一个twcc计算器，因为这里的sn是传输层的sn
			r.twcc = buffer.NewTransportWideCCResponder(uint32(track.SSRC()))
			r.twcc.OnFeedback(func(p rtcp.RawPacket) {
				r.rtcpCh <- []rtcp.Packet{&p}
			})
		}

		buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
			r.twcc.Push(sn, timeNS, marker)
		})
	}

	//if r.config.WithStats{
	//
	//}

	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			log.Errorf("Unmarshal rtcp receiver packets err: %v", err)
			return
		}
		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SourceDescription:
			case *rtcp.SenderReport:
				buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
			}
		}
		recv, ok := r.receivers[trackID]
		if !ok {
			recv = NewWebRTCReceiver
		}
	})

}

// PubDownTracks 新peer加入时，发布track到其他subscriber
func (r *router) PubDownTracks(s *Subscriber, recv Receiver) error {
	if recv != nil {
		if err := r.addDownTrack(s, recv); err != nil {
			log.Errorf("Peer %s pub track to peer %s error: %v", r.id, s.id, err)
			return err
		}
		s.negotiate()

	} else if len(r.receivers) > 0 {
		//新peer加入时，订阅其他Peer的所有receiver
		for _, rev := range r.receivers {
			if err := r.addDownTrack(s, rev); err != nil {
				log.Errorf("Peer %s sub track from peer %s error: %v", r.id, s.id, err)
				return err
			}
		}
		s.negotiate()
	}
	return nil
}

func (r *router) Stop() {
	// TODO: implement me
	panic(nil)
}

func (r *router) addDownTrack(sub *Subscriber, recv Receiver) error {
	// TODO: implement me
	panic(nil)
}
