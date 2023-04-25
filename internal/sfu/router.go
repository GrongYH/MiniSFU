package sfu

import (
	"github.com/pion/webrtc/v3"
	"sync"
)

type RouterConfig struct {
}

type Router interface {
	ID() string
	AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool)
	AddDownTracks(s *Subscriber, r Receiver) error
	Stop()
}

// Router manager a track rtp/rtcp router
type router struct {
	mu     sync.RWMutex
	id     string
	config RouterConfig

	session *Session
	pc      *webrtc.PeerConnection
}

// NewRouter for routing rtp/rtcp packets
func newRouter(tid string, pc *webrtc.PeerConnection, session *Session, config RouterConfig) Router {
	r := &router{
		id:      tid,
		pc:      pc,
		session: session,
		config:  config,
	}
	return r
}

func (r *router) ID() string {
	return r.id
}

func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool) {
	// TODO: implement me
	panic(nil)
}

func (r *router) AddDownTracks(s *Subscriber, recv Receiver) error {
	// TODO: implement me
	panic(nil)
}

func (r *router) Stop() {
	// TODO: implement me
	panic(nil)
}
