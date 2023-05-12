package buffer

import (
	"encoding/binary"
	"github.com/pion/rtcp"
	"math"
	"mini-sfu/internal/log"
)

// 一个rtp包的最大长度
const maxPktSize = 1460

type Bucket struct {
	buf    []byte
	nacker *nackQueue

	headSN   uint16
	step     int
	maxSteps int
	init     bool

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

// addPacket 先判断是不是乱序包，如果是之前丢失的包，就将该序号从NackQueue里面移除
// 如果是乱序包，就需要将该包和上一次发的最后一个包之间的序号都加入NackQueue
func (b *Bucket) addPacket(pkt []byte, sn uint16, latest bool) []byte {
	// 判断是否是第一次addPacket，如果是第一次，则b.headSN = sn - 1
	// 如果不这样，就会使得0到sn之间的序号全都加入nackQueue
	if !b.init {
		b.headSN = sn - 1
		b.init = true
	}

	if !latest {
		if b.nacker != nil {
			b.nacker.remove(sn)
		}
		return b.set(sn, pkt)
	}
	diff := sn - b.headSN
	// （b.headSN, sn]之间的packet没有收到，添加到nacker队列
	b.headSN = sn
	for i := uint16(1); i < diff; i++ {
		b.step++
		if b.nacker != nil {
			b.nacker.push(sn - i)
		}
		if b.step >= b.maxSteps {
			b.step = 0
		}
	}

	if b.nacker != nil {
		// 最多每接收两个包，就检验一次有没有丢包，调整一次NackQueue
		np, akf := b.nacker.pairs()
		if len(np) > 0 {
			b.onLost(np, akf)
		}
	}
	return b.push(pkt)
}

func (b *Bucket) set(sn uint16, pkt []byte) []byte {
	if b.headSN-sn >= uint16(b.maxSteps+1) {
		log.Errorf("too old")
		return nil
	}
	log.Debugf("b.step: %d, b.headSN: %d", b.step, b.headSN)

	// pos是从0开始算的,类似于数组的下标
	pos := b.step - int(b.headSN-sn+1)
	log.Debugf("sn: %d, pos:%d, maxStep:%d", sn, pos, b.maxSteps)
	if pos < 0 {
		// 说明buffer里面的包到达最后一个位置以后，又从buffer的第0个位置开始
		pos = pos + b.maxSteps
	}
	off := pos * maxPktSize
	if off > len(b.buf) || off < 0 {
		log.Errorf("packet越界")
		return nil
	}
	// packet前两个字节是packet的大小，按照大端存储
	binary.BigEndian.PutUint16(b.buf[off:], uint16(len(pkt)))
	// 从第二个字节以后，存pkt的实际内容
	copy(b.buf[off+2:], pkt)
	return b.buf[off+2 : off+2+len(pkt)]
}

// push 用于非乱序包，set 用于乱序包
func (b *Bucket) push(pkt []byte) []byte {
	binary.BigEndian.PutUint16(b.buf[b.step*maxPktSize:], uint16(len(pkt)))
	off := b.step*maxPktSize + 2
	copy(b.buf[off:], pkt)
	b.step++
	if b.step >= b.maxSteps {
		b.step = 0
	}
	return b.buf[off : off+len(pkt)]
}

func (b *Bucket) getPacket(buf []byte, sn uint16) (i int, err error) {
	p := b.get(sn)
	if p == nil {
		err = errPacketNotFound
	}
	i = len(p)
	if cap(buf) < i {
		err = errBufferTooSmall
		return
	}

	if len(buf) < i {
		buf = buf[:i]
	}
	copy(buf, p)
	return
}

func (b *Bucket) get(sn uint16) []byte {
	pos := b.step - int(b.headSN-sn+1)
	if pos < 0 {
		if pos*-1 > b.maxSteps {
			return nil
		}
		pos = b.maxSteps + pos
	}
	off := pos * maxPktSize
	if off > len(b.buf) {
		return nil
	}
	if binary.BigEndian.Uint16(b.buf[off+4:off+6]) != sn {
		return nil
	}
	sz := int(binary.BigEndian.Uint16(b.buf[off : off+2]))
	return b.buf[off+2 : off+2+sz]
}
