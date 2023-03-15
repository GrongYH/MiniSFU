package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Sender defines a interface for a track receiver
type Sender interface {
	ReadRTCP() (rtcp.Packet, error)
	WriteRTP(*rtp.Packet)
	stats() string
	Close()
}
