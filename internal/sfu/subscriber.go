package sfu

import (
	"context"
	"github.com/bep/debounce"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"io"
	"mini-sfu/internal/log"
	"sync"
	"time"
)

const APIChannelLabel = "mini-sfu"

type Subscriber struct {
	sync.RWMutex

	id string
	pc *webrtc.PeerConnection
	me *webrtc.MediaEngine

	tracks   map[string][]*DownTrack
	channels map[string]*webrtc.DataChannel

	negotiate func()

	closeOnce sync.Once
}

// NewSubscriber 建立一个空pc
func NewSubscriber(id string, cfg WebRTCTransportConfig) (*Subscriber, error) {
	me, err := getMediaEngine()
	if err != nil {
		log.Errorf("NewPeer error: %v", err)
		return nil, errPeerConnectionInitFailed
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(cfg.setting))
	pc, err := api.NewPeerConnection(cfg.configuration)

	if err != nil {
		log.Errorf("NewPeer subscriber error: %v", err)
		return nil, err
	}

	s := &Subscriber{
		id:       id,
		pc:       pc,
		me:       me,
		tracks:   make(map[string][]*DownTrack),
		channels: make(map[string]*webrtc.DataChannel),
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
	go s.downTracksReports()
	return s, nil
}

func (s *Subscriber) AddDatachannel(peer *Peer, dc *DataChannel) error {
	ndc, err := s.pc.CreateDataChannel(dc.Label, &webrtc.DataChannelInit{})
	if err != nil {
		return err
	}

	mws := newDCChain(dc.middlewares)
	p := mws.Process(ProcessFunc(func(ctx context.Context, args ProcessArgs) {
		if dc.onMessage != nil {
			dc.onMessage(ctx, args, peer.session.getDataChannels(peer.id, dc.Label))
		}
	}))
	ndc.OnMessage(func(msg webrtc.DataChannelMessage) {
		p.Process(context.Background(), ProcessArgs{
			Peer:        peer,
			Message:     msg,
			DataChannel: ndc,
		})
	})

	s.channels[dc.Label] = ndc

	return nil
}

func (s *Subscriber) AddDataChannel(label string) (*webrtc.DataChannel, error) {
	s.Lock()
	defer s.Unlock()

	log.Infof("subscriber add dataChannel")
	if s.channels[label] != nil {
		return s.channels[label], nil
	}

	dc, err := s.pc.CreateDataChannel(label, &webrtc.DataChannelInit{})
	if err != nil {
		log.Errorf("dc creation error: %v", err)
		return nil, errCreatingDataChannel
	}

	s.channels[label] = dc

	return dc, nil
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
		log.Errorf("PeerId: %s, subscriber createOffer error: %v", s.id, err)
		return webrtc.SessionDescription{}, err
	}

	//gatherComplete := webrtc.GatheringCompletePromise(s.pc)
	err = s.pc.SetLocalDescription(offer)
	if err != nil {
		log.Errorf("PeerId: %s, subscriber SetLocalDescription error: %v", s.id, err)
		return webrtc.SessionDescription{}, err
	}
	//<-gatherComplete
	log.Debugf("PeerId: %s, subscriber createOffer success, offer: %s", s.id, offer.SDP)
	return offer, nil
}

// OnICECandidate handler
func (s *Subscriber) OnICECandidate(f func(c *webrtc.ICECandidate)) {
	log.Debugf("subscriber OnICECandidate")
	s.pc.OnICECandidate(f)
}

func (s *Subscriber) SetRemoteDescription(des webrtc.SessionDescription) error {
	err := s.pc.SetRemoteDescription(des)
	if err != nil {
		log.Errorf("PeerId: %s, subscriber SetRemoteDescription error: %v", s.id, err)
		return err
	}
	log.Debugf("PeerId: %s, Subscriber SetRemoteDescription success", s.id)
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
	defer s.Unlock()

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

func (s *Subscriber) downTracksReports() {
	for {
		//每隔5s发一次
		time.Sleep(5 * time.Second)

		if s.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		var r []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		s.RLock()
		for _, dts := range s.tracks {
			for _, dt := range dts {
				if !dt.bound.get() {
					continue
				}
				r = append(r, dt.CreateSenderReport())
				sd = append(sd, dt.CreateSourceDescriptionChunks()...)
			}
		}
		s.RUnlock()
		i := 0
		j := 0
		for i < len(sd) {
			i = (j + 1) * 15
			if i >= len(sd) {
				i = len(sd)
			}
			// 15个chunk组成一个SDES rtcp报文
			nsd := sd[j*15 : i]
			r = append(r, &rtcp.SourceDescription{Chunks: nsd})
			j++
			if err := s.pc.WriteRTCP(r); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					return
				}
				log.Errorf("Sending downtrack reports err: %v", err)
			}
			r = r[:0]
		}
	}
}
