package sfu

import (
	"sync"
	"sync/atomic"

	"mini-sfu/internal/log"

	"github.com/pion/webrtc/v3"
)

type Publisher struct {
	id string
	pc *webrtc.PeerConnection

	router                            Router
	session                           *Session
	candidates                        []webrtc.ICECandidateInit
	onICEConnectionStateChangeHandler atomic.Value // func(webrtc.ICEConnectionState)

	closeOnce sync.Once
}

func NewPublisher(id string, session *Session, cfg WebRTCTransportConfig) (*Publisher, error) {
	me, err := getMediaEngine()
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
		id:         id,
		pc:         pc,
		session:    session,
		router:     newRouter(id, pc, session, cfg.router),
		candidates: make([]webrtc.ICECandidateInit, 0, 10),
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

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == APIChannelLabel {
			// terminate api data channel
			log.Infof("publisher onDataChannel")
			return
		}
		p.session.AddDatachannel(id, dc)
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debugf("PeerId: %s, Publisher ice connection state: %s", p.id, state)
		switch state {
		case webrtc.ICEConnectionStateFailed:
			fallthrough
		case webrtc.ICEConnectionStateClosed:
			log.Debugf("webrtc ice closed for peer: %s", p.id)
			p.Close()
		}

		if handler, ok := p.onICEConnectionStateChangeHandler.Load().(func(webrtc.ICEConnectionState)); ok && handler != nil {
			handler(state)
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

func (p *Publisher) OnICEConnectionStateChange(f func(connectionState webrtc.ICEConnectionState)) {
	p.onICEConnectionStateChangeHandler.Store(f)
}

// Answer 接收offer，返回answer
func (p *Publisher) Answer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		log.Errorf("PeerId: %s, publisher setRemoteSDP error: %v", p.id, err)
		return webrtc.SessionDescription{}, err
	}

	for _, c := range p.candidates {
		if err := p.pc.AddICECandidate(c); err != nil {
			log.Errorf("Add publisher ice candidate to peer %s err: %v", p.id, err)
		}
	}
	p.candidates = nil

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		log.Errorf("PeerId: %s, publisher CreateAnswer error: %v", p.id, err)
		return webrtc.SessionDescription{}, err
	}

	if err := p.pc.SetLocalDescription(answer); err != nil {
		log.Errorf("PeerId: %s, publisher SetLocalDescription error: %v", p.id, err)
		return webrtc.SessionDescription{}, err
	}
	return answer, nil
}

// AddICECandidate to peer connection
func (p *Publisher) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if p.pc.RemoteDescription() != nil {
		log.Debugf("publisher add trickle ice candidate")
		return p.pc.AddICECandidate(candidate)
	}
	p.candidates = append(p.candidates, candidate)
	return nil
}

// OnICECandidate handler
func (p *Publisher) OnICECandidate(f func(c *webrtc.ICECandidate)) {
	p.pc.OnICECandidate(f)
}

func (p *Publisher) GetRouter() Router {
	return p.router
}

func (p *Publisher) SignalingState() webrtc.SignalingState {
	return p.pc.SignalingState()
}
