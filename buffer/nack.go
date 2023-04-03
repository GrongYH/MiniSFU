package buffer

import (
	"sort"

	"github.com/pion/rtcp"
)

const (
	maxNackTimes = 3   // 一个包最多被发送Nack的次数
	maxNackCache = 100 // sfu将保存的最大NACK数量
)

type nack struct {
	sn     uint32
	nacked uint8
}

type nackQueue struct {
	nacks   []nack
	counter uint8
	maxSN   uint16
	kfSN    uint32
	cycles  uint32
}

func newNACKQueue() *nackQueue {
	return &nackQueue{
		nacks:  make([]nack, maxNackCache+1),
		maxSN:  0,
		cycles: 0,
	}
}

func (n *nackQueue) push(sn uint16) {
	var extSN uint32
	if n.maxSN == 0 {
		n.maxSN = sn
	} else if (sn-n.maxSN)&0x8000 == 0 {
		if sn < n.maxSN {
			//说明序列号循环了一圈
			n.cycles += maxSN
		}
		n.maxSN = sn
	}
	extSN = n.cycles | uint32(sn)
	// 找出nacks中序列号大于等于extSN的第一个索引
	i := sort.Search(len(n.nacks), func(i int) bool {
		return n.nacks[i].sn >= extSN
	})
	if i < len(n.nacks) && n.nacks[i].sn == extSN {
		//去重，该序列号不插入到nack队列
		return
	}
	n.nacks = append(n.nacks, nack{})
	copy(n.nacks[i+1:], n.nacks[i:])
	n.nacks[i] = nack{
		sn:     extSN,
		nacked: 0,
	}

	if len(n.nacks) > maxNackCache {
		n.nacks = n.nacks[1:]
	}
	n.counter++
}

func (n *nackQueue) remove(sn uint16) {
	var extSN uint32
	extSN = n.cycles | uint32(sn)
	// 找出该序号的nack
	i := sort.Search(len(n.nacks), func(i int) bool {
		return n.nacks[i].sn >= extSN
	})
	//不存在，所以不移除
	if i >= len(n.nacks) || n.nacks[i].sn != extSN {
		return
	}
	copy(n.nacks[i:], n.nacks[i+1:])
	n.nacks = n.nacks[:len(n.nacks)-1]
}

func (n *nackQueue) pairs() ([]rtcp.NackPair, bool) {
	// 2byte + 2byte（FSN + BLP）
	// FSN : Identifies the first sequence number lost.
	// BLP : Bitmask of following lost packets

	if n.counter < 2 {
		n.counter++
		return nil, false
	}

	n.counter = 0
	i := 0
	askKeyFrame := false
	var np rtcp.NackPair
	var nps []rtcp.NackPair
	for _, nck := range n.nacks {
		if nck.nacked >= maxNackTimes {
			// 简单地使用NACK申请重传的机制，这样就会有大量的RTCP NACK报文
			// 当发送端收到相应的报文，又会发送大量指定的RTP报文，反而会增加网络的拥塞程度，可能导致更高的丢包率
			// 这时采用申请I帧的方式，即PLI申请关键帧，是更为有效的方式
			// 因此如果该序号已经发送了3次nack，则该序号的包不再发送nack
			if nck.sn > n.kfSN {
				n.kfSN = nck.sn
				askKeyFrame = true
			}
			continue
		}
		n.nacks[i] = nack{
			sn:     nck.sn,
			nacked: nck.nacked + 1,
		}
		i++
		//如果PacketID == 0，说明是从第一个开始遍历，PacketID就是当前的序号
		//如果当前序号 > PacketID+16，则说明前17个序号都已经用nackPair表示，需要将当前的nackPair追加到切片中，新开一个nackPair
		if np.PacketID == 0 || uint16(nck.sn) > np.PacketID+16 {
			if np.PacketID != 0 {
				nps = append(nps, np)
			}
			np.PacketID = uint16(nck.sn)
			np.LostPackets = 0
			continue
		}
		// 否则用BLP表示
		np.LostPackets |= 1 << (uint16(nck.sn) - np.PacketID - 1)
	}
	if np.PacketID != 0 {
		nps = append(nps, np)
	}
	n.nacks = n.nacks[:i]
	return nps, askKeyFrame
}
