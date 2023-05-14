package sfu

import (
	"mini-sfu/internal/log"
	"sync"
	"time"
)

const (
	maxPacketMetaHistory = 500

	ignoreRetransmission = 100 // 单位ms
)

type packetMeta struct {
	// 原始序列号, 用于查找来自publisher的原始数据包
	sourceSeqNo uint16
	// 该序号用于关联downTrack，是根据原始的偏移量获得的，是不共享的
	targetSeqNo uint16
	//关联的downTrack的修改时间戳。
	timestamp uint32
	// 上次请求此数据包的时间(ms)
	// 有时客户端会多次请求相同的数据包，因此跟踪请求的数据包有助于避免多次写入同一个数据包。
	lastNack uint32
	// 最后一次请求此数据包的时间
	layer uint8
}

// sequencer 存储downTrack发送的数据包序列
type sequencer struct {
	sync.Mutex
	init      bool
	seq       []packetMeta
	step      int
	headSN    uint16
	startTime int64 //ms为单位
}

// newSequencer 模块整体的逻辑其实和buffer中的bucket一样，都是找到数组中所在pos的数据
func newSequencer() *sequencer {
	return &sequencer{
		startTime: time.Now().UnixNano() / 1e6,
		seq:       make([]packetMeta, maxPacketMetaHistory),
	}
}

func (n *sequencer) push(sn, offSn uint16, timeStamp uint32, layer uint8, head bool) *packetMeta {
	n.Lock()
	defer n.Unlock()

	if !n.init {
		n.headSN = offSn
		n.init = true
	}

	pos := 0
	if head {
		// (headSN, offSn]
		// 此时的数据包是位于队列最前面的
		inc := offSn - n.headSN
		for i := uint16(1); i < inc; i++ {
			n.step++
			if n.step >= maxPacketMetaHistory {
				// 到达队列尾部，重新从队列头开始
				n.step = 0
			}
		}
		pos = n.step
		n.headSN = offSn
	} else {
		// [offSN......n.headSN]
		//             n.step
		pos = n.step - int(n.headSN-offSn)
		if pos < 0 {
			if pos*-1 >= maxPacketMetaHistory {
				log.Errorf("Old packet received, can not be sequenced", "head", sn, "received", offSn)
				return nil
			}
			pos = maxPacketMetaHistory + pos
		}
	}
	n.seq[n.step] = packetMeta{
		sourceSeqNo: sn,
		targetSeqNo: offSn,
		timestamp:   timeStamp,
		layer:       layer,
	}
	pm := &n.seq[n.step]
	n.step++
	if n.step >= maxPacketMetaHistory {
		n.step = 0
	}
	return pm
}

// getSeqNoPairs seqNo中装载的sn为偏移后的sn
func (n *sequencer) getSeqNoPairs(seqNo []uint16) []packetMeta {
	n.Lock()

	meta := make([]packetMeta, 0, 17)
	refTime := uint32(time.Now().UnixNano()/1e6 - n.startTime)
	for _, sn := range seqNo {
		pos := n.step - int(n.headSN-sn) - 1
		if pos < 0 {
			if pos*-1 >= maxPacketMetaHistory {
				continue
			}
			pos = maxPacketMetaHistory + pos
		}
		seq := &n.seq[pos]
		// 如果存在，将packetMeta追加到返回值里
		if seq.targetSeqNo == sn {
			//如果没有重传过，或者距离上次重传的时间大于100ms，才会重传
			if seq.lastNack == 0 || refTime-seq.lastNack > ignoreRetransmission {
				seq.lastNack = refTime
				meta = append(meta, *seq)
			}
		}
	}
	n.Unlock()
	return meta
}
