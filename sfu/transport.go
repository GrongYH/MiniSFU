package sfu

import (
	"github.com/lucsky/cuid"
	"github.com/pion/webrtc/v3"
	"miniSFU/sfu/log"
	"sync"
	"time"
)

const (
	statCycle = 6 * time.Second
)

// Transport represents a transport that media can be sent over
type Transport interface {
	ID() string
	GetRouter(uint32) *Router
	Routers() map[uint32]*Router
	NewSender(*webrtc.Track) (Sender, error)
	stats() string
}

// WebRTCTransport represents a sfu peer connection
type WebRTCTransport struct {
	id                         string
	pc                         *webrtc.PeerConnection
	me                         MediaEngine
	mu                         sync.RWMutex
	stop                       bool
	session                    *Session
	routers                    map[uint32]*Router
	onNegotiationNeededHandler func()
	onTrackHandler             func(*webrtc.Track, *webrtc.RTPReceiver)
}

func NewWebRTCTransport(session *Session, offer webrtc.SessionDescription) (*WebRTCTransport, error) {
	me := MediaEngine{}
	if err := me.PopulateFromSDP(offer); err != nil {
		return nil, errSdpParseFailed
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me.MediaEngine), webrtc.WithSettingEngine(setting))
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		log.Errorf("NewPeer error: %v", err)
		return nil, errPeerConnectionInitFailed
	}

	p := &WebRTCTransport{
		id:      cuid.New(),
		pc:      pc,
		me:      me,
		session: session,
		routers: make(map[uint32]*Router),
	}
}

// CreateOffer generates the localDescription
func (p *WebRTCTransport) CreateOffer() (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Errorf("CreateOffer error: %v", err)
		return webrtc.SessionDescription{}, err
	}

	return offer, nil
}

// SetLocalDescription sets the SessionDescription of the remote peer
func (p *WebRTCTransport) SetLocalDescription(desc webrtc.SessionDescription) error {
	err := p.pc.SetLocalDescription(desc)
	if err != nil {
		log.Errorf("SetLocalDescription error: %v", err)
		return err
	}

	return nil
}

// CreateAnswer generates the localDescription
func (p *WebRTCTransport) CreateAnswer() (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		log.Errorf("CreateAnswer error: %v", err)
		return webrtc.SessionDescription{}, err
	}

	return offer, nil
}

// SetRemoteDescription sets the SessionDescription of the remote peer
func (p *WebRTCTransport) SetRemoteDescription(desc webrtc.SessionDescription) error {
	err := p.pc.SetRemoteDescription(desc)
	if err != nil {
		log.Errorf("SetRemoteDescription error: %v", err)
		return err
	}

	return nil
}

// AddICECandidate to peer connection
func (p *WebRTCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(candidate)
}

// OnICECandidate handler
func (p *WebRTCTransport) OnICECandidate(f func(c *webrtc.ICECandidate)) {
	p.pc.OnICECandidate(f)
}
