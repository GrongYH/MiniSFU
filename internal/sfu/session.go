package sfu

import (
	"mini-sfu/internal/log"
	"sync"
)

// Session 内的Peer会自动的订阅其他Peer
type Session struct {
	id             string
	mu             sync.RWMutex
	peers          map[string]*Peer
	closed         atomicBool
	fanOutDCs      []string
	datachannels   []*Datachannel
	onCloseHandler func()
}

func NewSession(id string, dcs []*Datachannel) *Session {
	return &Session{
		id:           id,
		peers:        make(map[string]*Peer),
		datachannels: dcs,
	}
}

func (s *Session) Peers() []*Peer {
	s.mu.RLock()
	defer s.mu.Unlock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	return peers
}

// Publish 会把track发布到Session内，其他的peer会自动订阅
func (s *Session) Publish(router Router, r Receiver) {
	peers := s.Peers()
	for _, p := range peers {
		// 不订阅自身
		if router.ID() == p.id {
			continue
		}

		log.Infof("Publishing track to peer %s", p.id)
		// 其他peer订阅
		// 给对应的subscriber和receiver添加downTrack
		// subscriber只是用来管理downTracks和生成SenderReport等
		// 实际收发包是WebRTCReceiver来控制，在writeRTP中
		if err := router.AddDownTracks(p.subscriber, r); err != nil {
			log.Errorf("Error subscribing transport to router: %s", err)
			continue
		}
	}
}
