package sfu

import (
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/log"
	"sync"
)

type Publisher struct {
	id string
	pc *webrtc.PeerConnection

	router  Router
	session *Session

	closeOnce sync.Once
}

func NewPublisher(id string, session *Session, cfg WebRTCTransportConfig) (*Publisher, error) {
	me, err := getPublisherMediaEngine()
	if err != nil {
		log.Errorf("NewPeer error: %v", err)
		return nil, errPeerConnectionInitFailed
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(cfg.setting))
	pc, err := api.NewPeerConnection(cfg.configuration)
	if err != nil {
		log.Errorf("NewPeer error: %v", err)
		return nil, errPeerConnectionInitFailed
	}

	log.Infof("PeerId: %s， publisher pc init success", id)
	p := &Publisher{
		id:      id,
		pc:      pc,
		session: session,
		router:  newRouter(id, pc, session, cfg.router),
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Debugf("Peer %s | got remote track id: %s | mediaSSRC: %d rid :%s | streamID: %s",
			p.id, track.ID(), track.SSRC(), track.RID(), track.StreamID())

		// 每当收到一个UpTrack时，新建一个Receiver用来接收。
		// 同时让Router管理该Receiver
		r, pub := p.router.AddReceiver(receiver, track)
		if pub {
			//这里会把UpTrack发布到房间内，其他peer会订阅到
			p.session.Publish(p.router, r)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debugf("PeerId: %s, Publisher ice connection state: %s", p.id, state)
		switch state {
		case webrtc.ICEConnectionStateFailed:
			fallthrough
		case webrtc.ICEConnectionStateClosed:
			p.Close()
		}
	})
	return p, nil
}

// Close 关闭publisher
func (p *Publisher) Close() {
	p.closeOnce.Do(func() {
		log.Debugf("webrtc ice closing... for peer: %s", p.id)
		p.router.Stop()
		if err := p.pc.Close(); err != nil {
			log.Errorf("webrtc transport close err: %v", err)
			return
		}
		log.Debugf("webrtc ice closed for peer: %s", p.id)
	})
}

// Answer 接收offer，返回answer
func (p *Publisher) Answer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		log.Errorf("PeerId: %s, publisher setRemoteSDP error: %v", err)
		return webrtc.SessionDescription{}, err
	}

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		log.Errorf("PeerId: %s, publisher CreateAnswer error: %v", err)
		return webrtc.SessionDescription{}, err
	}

	if err := p.pc.SetLocalDescription(answer); err != nil {
		log.Errorf("PeerId: %s, publisher SetLocalDescription error: %v", err)
		return webrtc.SessionDescription{}, err
	}
	return answer, nil
}

func (p *Publisher) GetRouter() Router {
	return p.router
}

func (p *Publisher) SignalingState() webrtc.SignalingState {
	return p.pc.SignalingState()
}
