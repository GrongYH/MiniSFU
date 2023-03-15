package sfu

import "sync"

// Session represents a set of transports. Transports inside a session
// are automatically subscribed to each other.
type Session struct {
	id             string
	transports     map[string]Transport
	transportsLock sync.RWMutex
	onCloseHandler func()
}
