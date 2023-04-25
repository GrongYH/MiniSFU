package sfu

import (
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/log"
	"sync"
)

type Subscriber struct {
	sync.RWMutex

	id string
	pc *webrtc.PeerConnection
	me *webrtc.MediaEngine

	closeOnce sync.Once
}

func NewSubscriber(id string, cfg WebRTCTransportConfig) (*Subscriber, error) {
	me := &webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(cfg.setting))
	pc, err := api.NewPeerConnection(cfg.configuration)
	if err != nil {
		log.Errorf("NewPeer subscriber error: %v", err)
		return nil, err
	}

	s := &Subscriber{
		id: id,
		pc: pc,
		me: me,
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debugf("PeerId: %s, Subscriber ice connection state: %s", s.id, state)
		switch state {
		case webrtc.ICEConnectionStateFailed:
			fallthrough
		case webrtc.ICEConnectionStateClosed:
			s.Close()
		}
	})
	return s, nil
}

// Close 关闭publisher
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() {
		log.Debugf("webrtc ice closing... for peer: %s", s.id)
		if err := s.pc.Close(); err != nil {
			log.Errorf("webrtc transport close err: %v", err)
			return
		}
		log.Debugf("webrtc ice closed for peer: %s", s.id)
	})
}
