package buffer

import (
	"fmt"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"miniSFU/sfu/log"
	"sync"
)

const (
	maxSN      = 65536 //sequence number
	maxPktSize = 1000

	// vp8 vp9 h264 clock rate 90000Hz
	videoClock = 90000

	// 1 + 1（FSN + BLP）
	// FSN : Identifies the first sequence number lost.
	// BLP : Bitmask of following lost packets
	maxNackLostSize = 17

	defaultBufferTime = 1000 //ms
)

type Buffer struct {
	pktBuffer [maxSN]*rtp.Packet

	// last pkt which calc nack
	lastNackSN  uint16
	lastClearTS uint32 //timeStamp
	lastClearSN uint16

	// Last seqnum that has been added to buffer
	lastPushSN uint16

	//each buffer just reveive one ssrc
	ssrc        uint32
	payloadType uint8

	//calc lost rate
	receivedPkt int
	lostPkt     int

	//response nack channel
	rtcpCh chan rtcp.Packet

	//calc bandwidth
	totalByte uint64

	maxBufferTS uint32
	stop        bool
	mu          sync.RWMutex
}

type BufferOptions struct {
	BufferTime int
}

func NewBuffer(ssrc uint32, pt uint8, opt BufferOptions) *Buffer {
	b := &Buffer{
		ssrc:        ssrc,
		payloadType: pt,
		rtcpCh:      make(chan rtcp.Packet, maxPktSize),
	}

	if opt.BufferTime <= 0 {
		opt.BufferTime = defaultBufferTime
	}
	b.maxBufferTS = uint32(opt.BufferTime) * videoClock / 1000
	// b.bufferStartTS = time.Now()
	log.Debugf("NewBuffer BufferOptions=%v", opt)
	return b
}

// Push adds a RTP Packet, when out of order, new packet may be arrived later
func (b *Buffer) Push(p *rtp.Packet) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.receivedPkt++
	b.totalByte += uint64(p.MarshalSize())

	// init ssrc payloadType (first receive)
	if b.ssrc == 0 || b.payloadType == 0 {
		b.ssrc = p.SSRC
		b.payloadType = p.PayloadType
	}

	// init lastClearTS
	if b.lastClearTS == 0 {
		b.lastClearTS = p.Timestamp
	}

	// init lastClearSN
	if b.lastClearSN == 0 {
		b.lastClearSN = p.SequenceNumber
	}

	// init lastNackSN
	if b.lastNackSN == 0 {
		b.lastNackSN = p.SequenceNumber
	}

	b.pktBuffer[p.SequenceNumber] = p
	b.lastPushSN = p.SequenceNumber

	// clear old packet by timestamp
	b.clearOldPkt(p.Timestamp, p.SequenceNumber)

	// limit nack range
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		b.lastNackSN = b.lastPushSN - maxNackLostSize
	}

	//该策略下，丢的包只会收到一次nack。
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		nackPair, lostPkt := b.GetNackPair(b.pktBuffer, b.lastNackSN, b.lastPushSN)
		b.lastNackSN = b.lastPushSN
		//log.Infof("b.lastNackSN=%v, b.lastPushSN=%v, lostPkt=%v, nackPair=%v", b.lastNackSN, b.lastPushSN, lostPkt, nackPair)
		if lostPkt > 0 {
			b.lostPkt += lostPkt
			nack := &rtcp.TransportLayerNack{
				MediaSSRC: b.ssrc,
				Nacks: []rtcp.NackPair{
					nackPair,
				},
			}
			b.rtcpCh <- nack
		}
	}
}

func tsDelta(x, y uint32) uint32 {
	if x > y {
		return x - y
	}
	return y - x
}

// clearOldPkt 清除缓冲区中过期的包
func (b *Buffer) clearOldPkt(pushPktTS uint32, pushPktSN uint16) {
	clearTS := b.lastClearTS
	clearSN := b.lastClearSN
	log.Infof("clearOldPkt pushPktTS=%d pushPktSN=%d     clearTS=%d  clearSN=%d ", pushPktTS, pushPktSN, clearTS, clearSN)

	if tsDelta(pushPktTS, clearTS) >= b.maxBufferTS {
		//pushPktSN will loop from 0 to 65535
		if pushPktSN == 0 {
			//make sure clear the old packet from 655xx to 65535
			pushPktSN = maxSN - 1
		}
		var skipCount int
		for i := clearSN + 1; i <= pushPktSN; i++ {
			// 如果buffer中该序号为空则跳过
			if b.pktBuffer[i] == nil {
				skipCount++
				continue
			}
			if tsDelta(pushPktTS, b.pktBuffer[i].Timestamp) >= b.maxBufferTS {
				b.lastClearTS = b.pktBuffer[i].Timestamp
				b.lastClearSN = i
				b.pktBuffer[i] = nil
			} else {
				break
			}
		}

		if skipCount > 0 {
			log.Tracef("b.pktBuffer nil count : %d", skipCount)
		}

		//如果该包SN为0，则lastNackSN直接从0开始计。
		if pushPktSN == maxSN-1 {
			b.lastClearSN = 0
			b.lastNackSN = 0
		}
	}
}

// GetNackPair 计算第PacketID和blp
func (b *Buffer) GetNackPair(buffer [65536]*rtp.Packet, begin, end uint16) (rtcp.NackPair, int) {
	var lostPkt int
	//size is <= 17
	if end-begin > maxNackLostSize {
		return rtcp.NackPair{}, lostPkt
	}
	//Bitmask of following lost packets (BLP)
	blp := uint16(0)
	lost := uint16(0)

	//计算PID
	for i := begin; i < end; i++ {
		if buffer[i] == nil {
			lost = i
			lostPkt++
			break
		}
	}

	//没有丢包
	if lost == 0 {
		return rtcp.NackPair{}, lostPkt
	}

	//calc blp
	for i := lost; i < end; i++ {
		//calc from next lost packet
		if i > lost && buffer[i] == nil {
			blp = blp | (1 << (i - lost - 1))
			lostPkt++
		}
	}

	// log.Tracef("NackPair begin=%v end=%v buffer=%v\n", begin, end, buffer[begin:end])
	return rtcp.NackPair{PacketID: lost, LostPackets: rtcp.PacketBitmap(blp)}, lostPkt
}

// Stop buffer
func (b *Buffer) Stop() {
	b.stop = true
	close(b.rtcpCh)
	b.clear()
}

func (b *Buffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.pktBuffer {
		b.pktBuffer[i] = nil
	}
}

// GetPayloadType get payloadtype
func (b *Buffer) GetPayloadType() uint8 {
	return b.payloadType
}

func (b *Buffer) stats() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return fmt.Sprintf("buffer: [%d, %d] | lastNackSN: %d", b.lastClearSN, b.lastPushSN, b.lastNackSN)
}

// GetSSRC get ssrc
func (b *Buffer) GetSSRC() uint32 {
	return b.ssrc
}

// GetRTCPChan return rtcp channel
func (b *Buffer) GetRTCPChan() chan rtcp.Packet {
	return b.rtcpCh
}

// GetLostRateBandwidth calc lostRate and bandwidth by cycle
func (b *Buffer) GetLostRateBandwidth(cycle uint64) (float64, uint64) {
	lostRate := float64(b.lostPkt) / float64(b.receivedPkt+b.lostPkt)
	byteRate := b.totalByte / cycle
	log.Tracef("Buffer.CalcLostRateByteRate b.receivedPkt=%d b.lostPkt=%d   lostRate=%v byteRate=%v", b.receivedPkt, b.lostPkt, lostRate, byteRate)
	b.receivedPkt, b.lostPkt, b.totalByte = 0, 0, 0
	return lostRate, byteRate * 8 / 1000
}

// GetPacket get packet by sequence number
func (b *Buffer) GetPacket(sn uint16) *rtp.Packet {
	return b.pktBuffer[sn]
}
