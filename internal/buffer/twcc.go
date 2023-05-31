package buffer

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
)

const (
	tccReportDelta          = 1e8 //10ms
	tccReportDeltaAfterMark = 5e8 //50ms
)

// rtp 扩展信息
type rtpExtInfo struct {
	ExtTSN    uint32 //扩展SN
	Timestamp int64
}

type Responder struct {
	sync.Mutex

	extInfo    []rtpExtInfo
	lastReport int64
	cycles     uint32
	lastExtSN  uint32
	lastSn     uint16
	pktCtn     uint8

	sSSRC uint32
	mSSRC uint32

	len      uint16 // 报头+chunk的长度
	deltaLen uint16 // delta的长度
	payload  [100]byte
	deltas   [200]byte
	chunk    uint16

	onFeedback func(pkt rtcp.RawPacket)
}

func NewTransportWideCCResponder(ssrc uint32) *Responder {
	return &Responder{
		extInfo:    make([]rtpExtInfo, 0, 101),
		sSSRC:      rand.Uint32(),
		mSSRC:      ssrc,
		lastReport: time.Now().Add(100 * time.Millisecond).UnixNano(),
	}
}

func (t *Responder) Push(sn uint16, timeNS int64, marker bool) {
	t.Lock()
	defer t.Unlock()

	// 判断是否回绕
	if sn < 0x0fff && (t.lastSn&0xffff) > 0xf000 {
		t.cycles += 1 << 16
	}

	t.extInfo = append(t.extInfo, rtpExtInfo{
		ExtTSN:    t.cycles | uint32(sn),
		Timestamp: timeNS / 1e3, //以us为单位
	})
	t.lastSn = sn
	delta := timeNS - t.lastReport

	// 扩展信息数组>20且发送时间间隔达到阈值10ms
	// 或者达到了帧结尾，达到阈值50ms
	// 这两个参数都是经验参数
	if len(t.extInfo) > 20 && t.mSSRC != 0 && (delta >= tccReportDelta) ||
		len(t.extInfo) > 100 ||
		(marker && delta >= tccReportDeltaAfterMark) {
		//log.Infof("len(t.extInfo):%d, delta:%d, marker:%d", len(t.extInfo), delta, marker)
		if pkt := t.buildTransportCCPacket(); pkt != nil {
			t.onFeedback(pkt)
		}
		t.lastReport = timeNS
	}
}

func (t *Responder) OnFeedback(f func(p rtcp.RawPacket)) {
	t.onFeedback = f
}

// buildTransportCCPacket 构造twcc rtcp包的核心逻辑
func (t *Responder) buildTransportCCPacket() rtcp.RawPacket {
	if len(t.extInfo) == 0 {
		return nil
	}

	// 将extInfo按照ExtTSN从小到大排列
	sort.Slice(t.extInfo, func(i, j int) bool {
		return t.extInfo[i].ExtTSN < t.extInfo[j].ExtTSN
	})

	//新建一个tccPkts切片，把老的extInfo拷贝进去，因为中间可能存在SN跳跃，不方便计算，所以要补充
	tccPkts := make([]rtpExtInfo, 0, int(float64(len(t.extInfo))*1.2))
	for _, tccExtInfo := range t.extInfo {
		if tccExtInfo.ExtTSN < t.lastExtSN {
			continue
		}

		//如果扩展SN有跳跃，则补充中间缺少的
		if t.lastExtSN != 0 {
			for j := t.lastExtSN + 1; j < tccExtInfo.ExtTSN; j++ {
				// 中间没有收到的SN，时间戳设置为0
				tccPkts = append(tccPkts, rtpExtInfo{ExtTSN: j})
			}
		}
		t.lastExtSN = tccExtInfo.ExtTSN
		tccPkts = append(tccPkts, tccExtInfo)
	}

	//清空extInfo
	t.extInfo = t.extInfo[:0]

	firstRecv := false
	same := true
	timestamp := int64(0)                                //us级
	lastStatus := rtcp.TypeTCCPacketReceivedWithoutDelta //作为结束一轮run length trunk构造的标记
	maxStatus := rtcp.TypeTCCPacketNotReceived

	var statusList deque.Deque[uint16]
	statusList.SetMinCapacity(3)

	// 循环计算,开始构造
	/*
	   0                   1                   2                   3
	   0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |V=2|P|  FMT=15 |    PT=205     |           length              |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                     SSRC of packet sender                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                      SSRC of media source                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |      base sequence number     |      packet status count      |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                 reference time                | fb pkt. count |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |          packet chunk         |         packet chunk          |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   .                                                               .
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |         packet chunk          |  recv delta   |  recv delta   |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   .                                                               .
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |           recv delta          |  recv delta   | zero padding  |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	for _, stat := range tccPkts {
		status := rtcp.TypeTCCPacketNotReceived

		// 只有到达的packet才会构造recv delta
		if stat.Timestamp != 0 {
			var delta int64

			// 拿数组里面的第一个包计算base sn,packet status count, reference time, 一定会触发该逻辑
			if !firstRecv {
				firstRecv = true
				//计算refTime := stat.Timestamp(us)/64ms(64000us)
				refTime := stat.Timestamp / 64e3
				timestamp = refTime * 64e3                                                      // 时间戳的整数
				t.writeHeader(uint16(tccPkts[0].ExtTSN), uint16(len(tccPkts)), uint32(refTime)) //写入twcc头
				//本包是第几个transport-cc包，每次加1，然后发送给客户端
				t.pktCtn++
			}

			// 计算该序号的delta，差值delta=(每个包的真实时间戳(us)-基准时间戳(us))/250us
			delta = (stat.Timestamp - timestamp) / 250
			// 如果超出255，则需要16位的large delta
			if delta < 0 || delta > 255 {
				status = rtcp.TypeTCCPacketReceivedLargeDelta
				rDelta := int16(delta)
				// 判断delta是否越界
				if int64(rDelta) != delta {
					if rDelta > 0 {
						rDelta = math.MaxInt16
					} else {
						rDelta = math.MinInt16
					}
				}
				// 存储时间差
				t.writeDelta(status, uint16(rDelta))
			} else {
				//这种情况常见，一般包间隔在255us以内
				status = rtcp.TypeTCCPacketReceivedSmallDelta
				t.writeDelta(status, uint16(delta))
			}
			// 记录该包到达的时间戳作为新的基准时间
			timestamp = stat.Timestamp
		}

		// 如果前几个包类型相同，且当前包的类型和上一个不同，且上一个包的类型不是TypeTCCPacketReceivedWithoutDelta
		if same && status != lastStatus && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta {
			//长度>7，使用RunLengthChunk格式写入
			if statusList.Len() > 7 {
				t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
				statusList.Clear()
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				same = true
			} else {
				same = false
			}
		}

		// 将当前包的类型存储
		statusList.PushBack(status)
		if status > maxStatus {
			maxStatus = status
		}
		lastStatus = status

		//如果几个包类型不同，并且包含TypeTCCPacketReceivedLargeDelta状态，则最多存7个，写入statusSymbolChunk，此时为Status vector chunk
		if !same && maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta && statusList.Len() > 6 {
			for i := 0; i < 7; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront(), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true

			//重新整理队列中其余的包
			for i := 0; i < statusList.Len(); i++ {
				status = statusList.At(i)
				if status > maxStatus {
					maxStatus = status
				}
				if same && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
					same = false
				}
				lastStatus = status
			}
			//如果几个包类型不同，并且不包含TypeTCCPacketReceivedLargeDelta状态，则最多存14个，写入StatusSymbolChunk
		} else if !same && statusList.Len() > 13 {
			for i := 0; i < 14; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront(), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true
		}
	}

	//如果有剩余的，继续填充状态块
	if statusList.Len() > 0 {
		if same {
			t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
		} else if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta {
			// len < 7
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront(), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
		} else {
			// len < 14
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront(), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
		}
	}

	// 计算rtp标准头信息，包括长度、类型、pading
	pLen := t.len + t.deltaLen + 4
	pad := pLen%4 != 0
	var padSize uint8
	for pLen%4 != 0 {
		padSize++
		pLen++
	}

	hdr := rtcp.Header{
		Padding: pad,
		Length:  (pLen / 4) - 1,                     // Length以4字节为单位 - 1
		Count:   rtcp.FormatTCC,                     //FMT
		Type:    rtcp.TypeTransportSpecificFeedback, //PT
	}
	// 序列化头
	hb, _ := hdr.Marshal()
	//复制状态块，时间差delta
	pkt := make(rtcp.RawPacket, pLen)
	copy(pkt, hb)
	copy(pkt[4:], t.payload[:t.len])
	copy(pkt[4+t.len:], t.deltas[:t.deltaLen])
	if pad {
		pkt[len(pkt)-1] = padSize
	}
	t.deltaLen = 0
	t.len = 0
	return pkt
}

func (t *Responder) writeHeader(nSN, packetCount uint16, refTime uint32) {
	/*
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                     SSRC of packet sender                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                      SSRC of media source                     |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |      base sequence number     |      packet status count      |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |                 reference time                | fb pkt. count |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	binary.BigEndian.PutUint32(t.payload[0:], t.sSSRC)
	binary.BigEndian.PutUint32(t.payload[4:], t.mSSRC)
	binary.BigEndian.PutUint16(t.payload[8:], nSN)
	binary.BigEndian.PutUint16(t.payload[10:], packetCount)
	binary.BigEndian.PutUint32(t.payload[12:], refTime<<8|uint32(t.pktCtn))
	t.len = 16
}

func (t *Responder) writeRunLengthChunk(symbol uint16, runLength uint16) {
	/*
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	   |T| S |       Run Length        |
	   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	binary.BigEndian.PutUint16(t.payload[t.len:], symbol<<13|runLength)
	t.len += 2
}

func (t *Responder) createStatusSymbolChunk(symbolSize, symbol uint16, i int) {
	/*
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		|T|S|       symbol list         |
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	*/
	numOfBits := symbolSize + 1
	t.chunk = setNBitsOfUint16(t.chunk, numOfBits, numOfBits*uint16(i)+2, symbol)
}

func (t *Responder) writeStatusSymbolChunk(symbolSize uint16) {
	t.chunk = setNBitsOfUint16(t.chunk, 1, 0, 1)
	t.chunk = setNBitsOfUint16(t.chunk, 1, 1, symbolSize)
	binary.BigEndian.PutUint16(t.payload[t.len:], t.chunk)
	t.chunk = 0
	t.len += 2
}

func (t *Responder) writeDelta(deltaType, delta uint16) {
	if deltaType == rtcp.TypeTCCPacketReceivedSmallDelta {
		t.deltas[t.deltaLen] = byte(delta)
		t.deltaLen++
		return
	}
	binary.BigEndian.PutUint16(t.deltas[t.deltaLen:], delta)
	t.deltaLen += 2
}

// 将val截断为size位,并且左移到i位置
func setNBitsOfUint16(src, size, startIndex, val uint16) uint16 {
	if startIndex+size > 16 {
		return 0
	}
	val &= (1 << size) - 1
	return src | (val << (16 - size - startIndex))
}
