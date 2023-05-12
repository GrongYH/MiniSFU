package sfu

import "errors"

var (
	// Peer error
	ErrTransportExists          = errors.New("rtc transport already exists for this connection")
	ErrNoTransportEstablished   = errors.New("rtc transport not exists for this connection")
	ErrOfferIgnored             = errors.New("offered ignored")
	errPeerConnectionInitFailed = errors.New("pc init failed")

	// router error
	errNoReceiverFound = errors.New("no receiver found")
)
