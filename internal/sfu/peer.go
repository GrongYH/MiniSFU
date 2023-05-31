package sfu

import (
	"fmt"
	"sync"

	"mini-sfu/internal/log"

	"github.com/lucsky/cuid"
	"github.com/pion/webrtc/v3"
)

const (
	publisher  = 0
	subscriber = 1
)

type SessionProvider interface {
	GetSession(sid string) (*Session, WebRTCTransportConfig)
}

// Peer 提供一对PC
type Peer struct {
	sync.RWMutex
	id       string
	closed   atomicBool
	session  *Session
	provider SessionProvider

	publisher  *Publisher
	subscriber *Subscriber

	OnOffer                    func(*webrtc.SessionDescription)
	OnICEConnectionStateChange func(state webrtc.ICEConnectionState)
	OnIceCandidate             func(*webrtc.ICECandidateInit, int)

	remoteAnswerPending bool
	negotiationPending  bool
}

func NewPeer(provider SessionProvider) *Peer {
	return &Peer{
		provider: provider,
	}
}

// Join 构造双PC，加入sid对应的session，注册subscriber的重协商回调
func (p *Peer) Join(sid string) error {
	if p.publisher != nil {
		log.Debugf("peer already exists")
		return ErrTransportExists
	}

	pid := cuid.New()
	p.id = pid

	var (
		cfg WebRTCTransportConfig
		err error
	)

	// 根据sessionID, 从SFU中获取该session
	p.session, cfg = p.provider.GetSession(sid)

	p.subscriber, err = NewSubscriber(p.id, cfg)
	if err != nil {
		log.Errorf("error creating subscriber pc: %v", err)
		return errPeerConnectionInitFailed
	}
	p.publisher, err = NewPublisher(p.id, p.session, cfg)
	if err != nil {
		log.Errorf("error creating publisher pc: %v", err)
		return errPeerConnectionInitFailed
	}

	for _, dc := range p.session.dataChannels {
		if err := p.subscriber.AddDatachannel(p, dc); err != nil {
			return fmt.Errorf("error setting subscriber default dc datachannel")
		}
	}

	// 需要协商时，subscriber创建offer并发给对端
	p.subscriber.OnNegotiationNeeded(func() {
		// 避免协商时的状态产生冲突，因此需要上锁
		p.Lock()
		defer p.Unlock()

		// 如果处于remoteAnswerPending状态，说明上一次协商还未结束，本次协商设为pending状态
		if p.remoteAnswerPending {
			p.negotiationPending = true
			return
		}
		log.Debugf("peer %s negotiation needed", p.id)
		offer, err := p.subscriber.CreateOffer()
		if err != nil {
			log.Errorf("CreateOffer error: %v", err)
			return
		}

		p.remoteAnswerPending = true
		// 如果subscriber有offer生成，则将offer发送给对端
		if p.OnOffer != nil && !p.closed.get() {
			log.Infof("peer %s send offer", p.id)
			p.OnOffer(&offer)
		}
	})

	p.publisher.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if p.OnICEConnectionStateChange != nil && !p.closed.get() {
			p.OnICEConnectionStateChange(s)
		}
	})

	p.subscriber.OnICECandidate(func(c *webrtc.ICECandidate) {
		log.Debugf("on subscriber ice candidate called for peer " + p.id)
		if c == nil {
			return
		}

		if p.OnIceCandidate != nil && !p.closed.get() {
			json := c.ToJSON()
			p.OnIceCandidate(&json, subscriber)
		}
	})

	p.publisher.OnICECandidate(func(c *webrtc.ICECandidate) {
		log.Debugf("on publisher ice candidate called for peer " + p.id)
		if c == nil {
			return
		}

		if p.OnIceCandidate != nil && !p.closed.get() {
			json := c.ToJSON()
			p.OnIceCandidate(&json, publisher)
		}
	})

	p.session.AddPeer(p)
	log.Infof("peer %s join session %s", p.id, sid)
	//加入房间时订阅Session内所有Peer
	p.session.Subscribe(p)
	return nil
}

// Answer Publisher收到offer后，会创建answer
func (p *Peer) Answer(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	if p.publisher == nil {
		return nil, ErrNoTransportEstablished
	}
	log.Infof("peer %s got offer， %s", p.id, offer.SDP)

	// 如果处于unstable状态，忽略本次offer
	// 本文介绍了webrtc信令状态机和完美协商 https://juejin.cn/post/7014074633347416072
	if p.publisher.SignalingState() != webrtc.SignalingStateStable {
		return nil, ErrOfferIgnored
	}

	answer, err := p.publisher.Answer(offer)
	if err != nil {
		log.Errorf("error create answer for peer %s", p.id)
		return nil, err
	}
	log.Infof("peer %s send answer", p.id)
	log.Infof("%s", answer.SDP)
	return &answer, nil
}

// SetRemoteDescription Subscriber重协商发出offer后，会收到Answer。
func (p *Peer) SetRemoteDescription(answer webrtc.SessionDescription) error {
	if p.subscriber == nil {
		log.Errorf("no subscriber exists")
		return ErrNoTransportEstablished
	}
	p.Lock()
	defer p.Unlock()

	//log.Infof("SetRemoteDescription: peer %s got answer, %s", p.id, answer.SDP)
	if err := p.subscriber.SetRemoteDescription(answer); err != nil {
		log.Errorf("peer %s set remote description failed: %v", p.id, err)
		return err
	}

	p.remoteAnswerPending = false
	// 上一次协商结束后，如果本次协商处于pending状态，则进行本次协商
	if p.negotiationPending {
		log.Debugf("negotiationPending is true")
		p.negotiationPending = false
		p.subscriber.negotiate()
	}
	log.Debugf("set remote sdp success")
	return nil
}

func (p *Peer) Close() {
	p.Lock()
	defer p.Unlock()

	p.closed.set(true)
	// 从session中删除Peer
	if p.session != nil {
		p.session.RemovePeer(p.id)
	}

	if p.publisher != nil {
		p.publisher.Close()
	}

	if p.subscriber != nil {
		p.subscriber.Close()
	}
}

// Trickle candidates available for this peer
func (p *Peer) Trickle(candidate webrtc.ICECandidateInit, target int) error {
	if p.subscriber == nil || p.publisher == nil {
		return ErrNoTransportEstablished
	}
	switch target {
	case publisher:
		if err := p.publisher.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("error setting ice candidate: %s", err)
		}
	case subscriber:
		if err := p.subscriber.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("error setting ice candidate: %s", err)
		}
	}
	return nil
}

func (p *Peer) Subscriber() *Subscriber {
	return p.subscriber
}

func (p *Peer) Publisher() *Publisher {
	return p.publisher
}

func (p *Peer) Session() *Session {
	return p.session
}
