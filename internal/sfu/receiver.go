package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/buffer"
)

type Receiver interface {
	TrackID() string
	StreamID() string
	RID() string
	Codec() webrtc.RTPCodecParameters
	Kind() webrtc.RTPCodecType
	SetRTCPCh(rtcp chan []rtcp.Packet)
	OnCloseHandler(f func())
	AddUpTrack(track *webrtc.TrackRemote, buff *buffer.Buffer)
}

type WebRTCReceiver struct {
}

func NewWebRTCReceiver(recv *webrtc.RTPReceiver, track *webrtc.TrackRemote) Receiver {

}
