package sfu

import "errors"

var (
	errSdpParseFailed           = errors.New("sdp parse failed")
	ErrTransportExists          = errors.New("rtc transport already exists for this connection")
	ErrNoTransportEstablished   = errors.New("rtc transport not exists for this connection")
	ErrOfferIgnored             = errors.New("offered ignored")
	errPeerConnectionInitFailed = errors.New("pc init failed")
	errChanClosed               = errors.New("channel closed")
	errPtNotSupported           = errors.New("payload type not supported")
	errMethodNotSupported       = errors.New("method not supported")
	errReceiverClosed           = errors.New("receiver closed")
)
