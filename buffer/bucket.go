package buffer

import (
	"github.com/pion/rtcp"
	"math"
)

const maxPktSize = 1460

type Bucket struct {
	buf    []byte
	nacker *nackQueue

	headSN   uint16
	step     int
	maxSteps int

	onLost func(nack []rtcp.NackPair, askKeyframe bool)
}

func NewBucket(buf []byte, nack bool) *Bucket {
	b := &Bucket{
		buf:      buf,
		maxSteps: int(math.Floor(float64(len(buf))/float64(maxPktSize))) - 1,
	}
	if nack {
		b.nacker = newNACKQueue()
	}
	return b
}
