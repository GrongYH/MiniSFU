package sfu

import (
	"mini-sfu/internal/log"
	"sync"
)

// Session 内的Peer会自动的订阅其他Peer
type Session struct {
	id     string
	mu     sync.RWMutex
	peers  map[string]*Peer
	closed atomicBool

	onCloseHandler func()
}

func NewSession(id string) *Session {
	return &Session{
		id:    id,
		peers: make(map[string]*Peer),
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

func (s *Session) AddPeer(peer *Peer) {
	s.mu.RLock()
	defer s.mu.Unlock()
	s.peers[peer.id] = peer
}

func (s *Session) RemovePeer(pid string) {
	s.mu.RLock()
	defer s.mu.Unlock()
	delete(s.peers, pid)
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
		// subscriber只是用来管理downTracks和生成SenderReport等
		// 实际收发包是WebRTCReceiver来控制，在writeRTP中
		if err := router.PubDownTracks(p.subscriber, r); err != nil {
			log.Errorf("Error subscribing transport to router: %s", err)
			continue
		}
	}
}

// Subscribe 订阅其他所有Peer
func (s *Session) Subscribe(peer *Peer) {
	peers := s.Peers()
	for _, p := range peers {
		if p == peer {
			continue
		}

		err := p.publisher.GetRouter().PubDownTracks(peer.subscriber, nil)
		if err != nil {
			log.Errorf("Error subscribing transport form router: %s", err)
			continue
		}
	}
}
func (s *Session) OnClose(f func()) {
	s.onCloseHandler = f
}
