package sfu

import (
	"fmt"
	"github.com/pion/ice/v2"
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/buffer"
	"mini-sfu/internal/log"
	"sync"
)

// WebRTCTransportConfig 定义了ice参数
type WebRTCTransportConfig struct {
	configuration webrtc.Configuration
	setting       webrtc.SettingEngine
	router        RouterConfig
}

type Config struct {
	IceLite      bool         `mapstructure:"iceLite"`
	ICEPortRange []uint16     `mapstructure:"portRange"`
	SDPSemantics string       `json:"sdpSemantics"`
	Router       RouterConfig `json:"router"`
}

// SFU represents an sfu instance
type SFU struct {
	sync.RWMutex
	webrtc       WebRTCTransportConfig
	sessions     map[string]*Session
	dataChannels []*DataChannel
}

var (
	bufferFactory *buffer.Factory
	packetFactory *sync.Pool
)

func init() {
	bufferFactory = buffer.NewBufferFactory()
	packetFactory = &sync.Pool{
		New: func() interface{} {
			return make([]byte, 1460)
		},
	}
}

func NewWebRTCTransportConfig(c Config) WebRTCTransportConfig {
	se := webrtc.SettingEngine{}

	// 设置ice连接的udp端口范围
	var icePortStart, icePortEnd uint16
	if len(c.ICEPortRange) == 2 {
		icePortStart = c.ICEPortRange[0]
		icePortEnd = c.ICEPortRange[1]
	}

	if icePortStart != 0 || icePortEnd != 0 {
		if err := se.SetEphemeralUDPPortRange(icePortStart, icePortEnd); err != nil {
			log.Panicf("setting ice udp range error")
		}
	}

	// 设置为iceLite模式
	if c.IceLite {
		se.SetLite(c.IceLite)
	}

	//将buffer设置为自定义buffer
	se.BufferFactory = bufferFactory.GetOrNew

	sdpSemantics := webrtc.SDPSemanticsUnifiedPlan
	switch c.SDPSemantics {
	case "unified-plan-with-fallback":
		sdpSemantics = webrtc.SDPSemanticsUnifiedPlanWithFallback
	case "plan-b":
		sdpSemantics = webrtc.SDPSemanticsPlanB
	}

	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	return WebRTCTransportConfig{
		configuration: webrtc.Configuration{
			SDPSemantics: sdpSemantics,
		},
		setting: se,
		router:  c.Router,
	}
}

// NewSFU creates a new sfu instance
func NewSFU(c Config) *SFU {
	w := NewWebRTCTransportConfig(c)
	s := &SFU{
		webrtc:   w,
		sessions: make(map[string]*Session),
	}
	return s
}

// newSession 创建一个session实例并加入sfu
func (s *SFU) newSession(id string) *Session {
	fmt.Println()
	log.Debugf("%d", len(s.dataChannels))
	fmt.Println()

	session := NewSession(id, s.dataChannels)
	session.OnClose(func() {
		s.Lock()
		delete(s.sessions, id)
		s.Unlock()
	})

	s.Lock()
	s.sessions[id] = session
	s.Unlock()

	return session
}

func (s *SFU) getSession(id string) *Session {
	s.RLock()
	defer s.RUnlock()
	return s.sessions[id]
}

// GetSession 获取session以及用于初始化publisher, subscriber, router的配置
func (s *SFU) GetSession(sid string) (*Session, WebRTCTransportConfig) {
	session := s.getSession(sid)
	// session不存在就创建一个
	if session == nil {
		session = s.newSession(sid)
	}
	return session, s.webrtc
}

func (s *SFU) NewDatachannel(label string) *DataChannel {
	dc := &DataChannel{Label: label}
	s.dataChannels = append(s.dataChannels, dc)
	return dc
}
