package sfu

import (
	"github.com/bep/debounce"
	"github.com/pion/rtcp"
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

func (s *Subscriber) RemoveDownTrack(streamID string, downTrack *DownTrack) {
	s.Lock()
	s.Unlock()

	dts, ok := s.tracks[streamID]
	if ok {
		index := -1
		for i, dt := range dts {
			if dt == downTrack {
				index = i
			}
		}

		if index >= 0 {
			dts[index] = dts[len(dts)-1]
			dts[len(dts)-1] = nil
			dts = dts[:len(dts)-1]
			s.tracks[streamID] = dts
		}
	}
}

// sendStreamDownTracksReports 给subscriber pc 发送源描述报文
// SDES报文是用来描述（音视频）媒体源的。
// 唯一有价值的是CNAME项，其作用是将不同的源（SSRC）绑定到同一个CNAME上。
// 比如当SSRC有冲突时，可以通过CNAME将旧的SSRC更换成新的SSRC，从而保证在通信的每个SSRC都是唯一的。
func (s *Subscriber) sendStreamDownTracksReports(streamID string) {
	var r []rtcp.Packet
	var sd []rtcp.SourceDescriptionChunk

	s.RLock()
	dts := s.tracks[streamID]
	for _, dt := range dts {
		if !dt.bound.get() {
			continue
		}
		sd = append(sd, dt.CreateSourceDescriptionChunks()...)
	}
	s.RUnlock()

	r = append(r, &rtcp.SourceDescription{Chunks: sd})

	go func() {
		r := r
		i := 0
		for {
			if err := s.pc.WriteRTCP(r); err != nil {
				log.Errorf("Sending track binding reports err:%v", err)
			}

			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}
