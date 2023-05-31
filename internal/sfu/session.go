package sfu

import (
	"sync"

	"mini-sfu/internal/log"

	"github.com/pion/webrtc/v3"
)

// Session 内的Peer会自动的订阅其他Peer
type Session struct {
	id           string
	mu           sync.RWMutex
	peers        map[string]*Peer
	fanOutDCs    []string
	dataChannels []*DataChannel
	closed       atomicBool

	onCloseHandler func()
}

func NewSession(id string, dcs []*DataChannel) *Session {
	return &Session{
		id:           id,
		peers:        make(map[string]*Peer),
		dataChannels: dcs,
	}
}

func (s *Session) Peers() []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	return peers
}

func (s *Session) AddPeer(peer *Peer) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.peers[peer.id] = peer
}

func (s *Session) RemovePeer(pid string) {
	s.mu.RLock()
	delete(s.peers, pid)
	log.Infof("RemovePeer %s from session %s", pid, s.id)
	s.mu.RUnlock()

	// 当Peer数为0时，关闭session
	if len(s.peers) == 0 && s.onCloseHandler != nil && !s.closed.get() {
		s.closed.set(true)
		s.onCloseHandler()
	}
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
		if err := router.AddDownTracks(p.subscriber, r); err != nil {
			log.Errorf("Error subscribing transport to router: %s", err)
			continue
		}
	}
}

// Subscribe 订阅其他所有Peer (即其他Peer的publisher发布track到本Peer的subscriber)
func (s *Session) Subscribe(peer *Peer) {
	s.mu.RLock()
	fdc := make([]string, len(s.fanOutDCs))
	copy(fdc, s.fanOutDCs)
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		if p == peer {
			continue
		}
		peers = append(peers, p)
	}
	s.mu.RUnlock()

	// Subscribe to fan out datachannels
	log.Debugf("fanOutDCs is %v", fdc)
	for _, label := range fdc {
		n, err := peer.subscriber.AddDataChannel(label)
		if err != nil {
			log.Errorf("error adding datachannel: %s", err)
			continue
		}
		l := label
		n.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.onMessage(peer.id, l, msg)
		})
	}

	for _, p := range peers {
		err := p.publisher.GetRouter().AddDownTracks(peer.subscriber, nil)
		if err != nil {
			log.Errorf("Error subscribing transport from router: %s", err)
			continue
		}
	}
	peer.subscriber.negotiate()
}

func (s *Session) OnClose(f func()) {
	s.onCloseHandler = f
}
func (s *Session) AddDatachannel(owner string, dc *webrtc.DataChannel) {
	label := dc.Label()

	s.mu.Lock()
	s.fanOutDCs = append(s.fanOutDCs, label)
	s.peers[owner].subscriber.channels[label] = dc
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		if p.id == owner {
			continue
		}
		peers = append(peers, p)
	}
	s.mu.Unlock()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.onMessage(owner, label, msg)
	})

	for _, p := range peers {
		n, err := p.subscriber.AddDataChannel(label)

		if err != nil {
			log.Errorf("error adding datachannel: %s", err)
			continue
		}

		pid := p.id
		n.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.onMessage(pid, label, msg)
		})

		p.subscriber.negotiate()
	}
}

func (s *Session) onMessage(origin, label string, msg webrtc.DataChannelMessage) {
	dcs := s.getDataChannels(origin, label)
	for _, dc := range dcs {
		if msg.IsString {
			if err := dc.SendText(string(msg.Data)); err != nil {
				log.Errorf("Sending dc message err: %v", err)
			}
		} else {
			if err := dc.Send(msg.Data); err != nil {
				log.Errorf("Sending dc message err: %v", err)
			}
		}
	}
}

func (s *Session) getDataChannels(origin, label string) (dcs []*webrtc.DataChannel) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for pid, p := range s.peers {
		if origin == pid {
			continue
		}

		if dc, ok := p.subscriber.channels[label]; ok && dc.ReadyState() == webrtc.DataChannelStateOpen {
			dcs = append(dcs, dc)
		}
	}
	return
}
