package sfu

import "github.com/pion/webrtc/v3"

type Receiver interface {
}

type WebRTCReceiver struct {
}

func NewWebRTCReceiver(recv *webrtc.RTPReceiver, track *webrtc.TrackRemote) Receiver {

}
