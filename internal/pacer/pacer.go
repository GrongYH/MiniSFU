package pacer

import (
	"sync"
	"time"

	"mini-sfu/internal/log"

	"github.com/gammazero/deque"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type Packet struct {
	Header      *rtp.Header
	Payload     []byte
	WriteStream webrtc.TrackLocalWriter
}

type Pacer interface {
	Enqueue(Packet)
	SetTargetBitrate(uint64)
	Stop()
}

type BucketPacer struct {
	targetBitrate     uint64
	targetBitrateLock sync.Mutex
	f                 float64

	lock           sync.RWMutex
	pacingInterval time.Duration
	lastSend       time.Time
	packets        deque.Deque[Packet]
	done           chan struct{}
}

func NewBucketPacer(initialBitrate uint64) *BucketPacer {
	p := &BucketPacer{
		targetBitrate:  initialBitrate,
		pacingInterval: 5 * time.Millisecond,
		f:              1.5,
		done:           make(chan struct{}),
		lastSend:       time.Now(),
	}
	p.packets.SetMinCapacity(9)

	go p.run()
	return p
}

// SetTargetBitrate 设置pacer发包的码率
func (p *BucketPacer) SetTargetBitrate(rate uint64) {
	p.targetBitrateLock.Lock()
	defer p.targetBitrateLock.Unlock()
	p.targetBitrate = uint64(p.f * float64(rate))
}

func (p *BucketPacer) getTargetBitrate() uint64 {
	p.targetBitrateLock.Lock()
	defer p.targetBitrateLock.Unlock()

	return p.targetBitrate
}

func (p *BucketPacer) Enqueue(pkt Packet) {
	p.lock.Lock()
	p.packets.PushBack(pkt)
	p.lock.Unlock()
}

func (p *BucketPacer) Stop() {
	close(p.done)
}

func (p *BucketPacer) run() {
	ticker := time.NewTicker(p.pacingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case now := <-ticker.C:
			delta := float64(now.Sub(p.lastSend).Milliseconds())
			budget := int(delta * float64(p.getTargetBitrate()) / 8000.0)
			p.lock.Lock()
			//log.Infof("budget=%v, len(queue)=%v, targetBitrate=%v, delta= %f", budget, p.packets.Len(), p.getTargetBitrate(), delta)

			for p.packets.Len() != 0 && budget > 0 {
				pkt := p.packets.PopFront()
				p.lock.Unlock()
				n, err := pkt.WriteStream.WriteRTP(pkt.Header, pkt.Payload)
				if err != nil {
					log.Errorf("failed to write packet: %v", err)
				}
				p.lastSend = now
				budget -= n
				p.lock.Lock()
			}
			p.lock.Unlock()
		}
	}
}
