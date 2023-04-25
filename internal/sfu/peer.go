package sfu

import "sync"

// Peer 提供一对PC
type Peer struct {
	sync.RWMutex
	id      string
	closed  atomicBool
	session *Session

	publisher  *Publisher
	subscriber *Subscriber
}
