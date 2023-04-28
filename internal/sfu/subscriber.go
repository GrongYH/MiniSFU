package sfu

import (
	"github.com/bep/debounce"
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/log"
	"sync"
	"time"
)

type Subscriber struct {
	sync.RWMutex

	id     string
	pc     *webrtc.PeerConnection
	me     *webrtc.MediaEngine
	tracks map[string][]*DownTrack

	negotiate func()

	closeOnce sync.Once
}

// NewSubscriber 建立一个空pc
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

func (s *Subscriber) OnNegotiationNeeded(f func()) {
	debounced := debounce.New(250 * time.Millisecond)
	s.negotiate = func() {
		debounced(f)
	}
}

func (s *Subscriber) CreateOffer() (webrtc.SessionDescription, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		log.Errorf("PeerId: %s, subscriber CreateOffer error: %v", err)
		return webrtc.SessionDescription{}, nil
	}
	log.Debugf("PeerId: %s, Subscriber  CreateOffer success, offer: %s", offer.SDP)
	err = s.pc.SetLocalDescription(offer)
	if err != nil {
		log.Errorf("PeerId: %s, subscriber SetLocalDescription error: %v", err)
		return webrtc.SessionDescription{}, nil
	}
	return offer, nil
}

func (s *Subscriber) SetRemoteDescription(des webrtc.SessionDescription) error {
	err := s.pc.SetRemoteDescription(des)
	if err != nil {
		log.Errorf("PeerId: %s, subscriber SetRemoteDescription error: %v", err)
		return err
	}
	log.Debugf("PeerId: %s, Subscriber SetRemoteDescription success")
	return nil
}

func (s *Subscriber) GetDownTracks(sid string) []*DownTrack {
	s.Lock()
	defer s.Unlock()
	return s.tracks[sid]
}

// AddDownTrack 给subscriber中添加downtrack
func (s *Subscriber) AddDownTrack(streamID string, track *DownTrack) {
	s.Lock()
	defer s.Unlock()

	if dt, ok := s.tracks[streamID]; ok {
		dt = append(dt, track)
		s.tracks[streamID] = dt
	} else {
		s.tracks[streamID] = []*DownTrack{track}
	}
}
