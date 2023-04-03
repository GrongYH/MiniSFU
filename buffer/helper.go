package buffer

import "sync/atomic"

type atomicBool int32

func (a *atomicBool) get() bool {
	return atomic.LoadInt32((*int32)(a)) != 0
}

func (a *atomicBool) set(value bool) {
	var i int32
	if value {
		i = 1
	}
	atomic.StoreInt32((*int32)(a), i)
}
