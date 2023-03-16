package sfu

import (
	"github.com/pion/webrtc/v3"
	"miniSFU/sfu/log"
)

// ICEServerConfig defines parameters for ice servers
type ICEServerConfig struct {
	URLs       []string `mapstructure:"urls"`
	Username   string   `mapstructure:"username"`
	Credential string   `mapstructure:"credential"`
}

// WebRTCConfig defines parameters for ice
type WebRTCConfig struct {
	ICEPortRange []uint16          `mapstructure:"portrange"`
	ICEServers   []ICEServerConfig `mapstructure:"iceserver"`
}

// ReceiverConfig defines receiver configurations
type ReceiverConfig struct {
	Video WebRTCVideoReceiverConfig `mapstructure:"video"`
}

// Config for base SFU
type Config struct {
	WebRTC   WebRTCConfig   `mapstructure:"webrtc"`
	Log      log.Config     `mapstructure:"log"`
	Receiver ReceiverConfig `mapstructure:"receiver"`
}

var (
	// only support unified plan
	cfg = webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	config  Config
	setting webrtc.SettingEngine
)
