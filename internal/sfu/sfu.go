package sfu

import (
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/buffer"
	"sync"
)

// WebRTCTransportConfig defines parameters for ice
type WebRTCTransportConfig struct {
	configuration webrtc.Configuration
	setting       webrtc.SettingEngine
	router        RouterConfig
}

// SFU represents an sfu instance
type SFU struct {
	mu       sync.RWMutex
	sessions map[string]*Session
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

// NewSFU creates a new sfu instance
func NewSFU() *SFU {
	s := &SFU{
		sessions: make(map[string]*Session),
	}
	return s
}
