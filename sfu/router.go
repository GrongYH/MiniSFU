package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// Router defines a track rtp/rtcp Router
// Router 管理receiver，将receiver收到的track发给其他Peer的subscriber
type Router interface {
	ID() string
	AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, trackID, streamID string) (Receiver, bool)
	AddDownTracks(s *Subscriber, r Receiver) error
	SetRTCPWriter(func([]rtcp.Packet) error)
	AddDownTrack(s *Subscriber, r Receiver) (*DownTrack, error)
	Stop()
	GetReceiver() map[string]Receiver
	OnAddReceiverTrack(f func(receiver Receiver))
	OnDelReceiverTrack(f func(receiver Receiver))
}

// RouterConfig defines Router configurations
type RouterConfig struct {
	WithStats           bool            `mapstructure:"withstats"`
	MaxBandwidth        uint64          `mapstructure:"maxbandwidth"`
	MaxPacketTrack      int             `mapstructure:"maxpackettrack"`
	AudioLevelInterval  int             `mapstructure:"audiolevelinterval"`
	AudioLevelThreshold uint8           `mapstructure:"audiolevelthreshold"`
	AudioLevelFilter    int             `mapstructure:"audiolevelfilter"`
	Simulcast           SimulcastConfig `mapstructure:"simulcast"`
}
