package sfu

import (
	"fmt"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"io"
	buffer2 "miniSFU/buffer"
	"miniSFU/log"
	"sync"
	"time"
)

const (
	// bandwidth range(kbps)
	minBandwidth = 200
	maxSize      = 100

	// tcc stuff
	tccExtMapID = 3
	//64ms = 64000us = 250 << 8
	//https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#41
	baseScaleFactor = 64000
	//https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#43
	timeWrapPeriodUs = (int64(1) << 24) * baseScaleFactor
)

// Receiver defines a interface for a track receiver
type Receiver interface {
	Track() *webrtc.Track
	GetPacket(sn uint16) *rtp.Packet
	ReadRTP() (*rtp.Packet, error)
	ReadRTCP() (rtcp.Packet, error)
	WriteRTCP(rtcp.Packet) error
	Close()
	stats() string
}

type rtpExtInfo struct {
	//transport sequence num
	TSN       uint16
	Timestamp int64
}

// WebRTCAudioReceiver receives a audio track
type WebRTCAudioReceiver struct {
	track *webrtc.Track
	stop  bool
	rtpCh chan *rtp.Packet
}

// NewWebRTCAudioReceiver creates a new audio track receiver
func NewWebRTCAudioReceiver(track *webrtc.Track) *WebRTCAudioReceiver {
	a := &WebRTCAudioReceiver{
		track: track,
		rtpCh: make(chan *rtp.Packet, maxSize),
	}

	return a
}

// ReadRTP read rtp packet
func (a *WebRTCAudioReceiver) ReadRTP() (*rtp.Packet, error) {
	if a.stop {
		return nil, errReceiverClosed
	}
	rtpPacket, err := a.track.ReadRTP()
	if err != nil {
		return nil, err
	}
	return rtpPacket, nil
}

// ReadRTCP read rtcp packet
func (a *WebRTCAudioReceiver) ReadRTCP() (rtcp.Packet, error) {
	return nil, errMethodNotSupported
}

// WriteRTCP write rtcp packet
func (a *WebRTCAudioReceiver) WriteRTCP(pkt rtcp.Packet) error {
	return errMethodNotSupported
}

// Track returns receiver track
func (a *WebRTCAudioReceiver) Track() *webrtc.Track {
	return a.track
}

// GetPacket returns nil since audio isn't buffered (uses fec)
func (a *WebRTCAudioReceiver) GetPacket(sn uint16) *rtp.Packet {
	return nil
}

// Close track
func (a *WebRTCAudioReceiver) Close() {
	a.stop = true
}

// Stats get stats for video receiver
func (a *WebRTCAudioReceiver) stats() string {
	return fmt.Sprintf("payload:%d", a.track.PayloadType())
}

// WebRTCVideoReceiver receives a video track
// videoTrack will use buffer, so that rtcp will be used
type WebRTCVideoReceiver struct {
	buffer         *buffer2.Buffer
	track          *webrtc.Track
	bandwidth      uint64
	lostRate       float64
	stop           bool
	rtpCh          chan *rtp.Packet
	rtcpCh         chan rtcp.Packet
	mu             sync.RWMutex
	rtpExtInfoChan chan rtpExtInfo

	pliCycle     int
	rembCycle    int
	tccCycle     int
	maxBandwidth int
	feedback     string
}

// WebRTCVideoReceiverConfig .
type WebRTCVideoReceiverConfig struct {
	// the remb cycle sending to pub, this told the pub it's bandwidth
	REMBCycle int `mapstructure:"rembcycle"`
	// pli cycle sending to pub, and pub will send a key frame
	PLICycle int `mapstructure:"plicycle"`
	// transport-cc cycle
	TCCCycle int `mapstructure:"tcccycle"`
	// this limit the remb bandwidth
	MaxBandwidth int `mapstructure:"maxbandwidth"`
	// max buffer time by ms
	MaxBufferTime int `mapstructure:"maxbuffertime"`
}

func NewWebRTCVideoReceiver(config WebRTCVideoReceiverConfig, track *webrtc.Track) *WebRTCVideoReceiver {
	v := &WebRTCVideoReceiver{
		buffer: buffer2.NewBuffer(uint32(track.SSRC()), uint8(track.PayloadType()), buffer2.BufferOptions{
			BufferTime: config.MaxBufferTime,
		}),
		track:          track,
		rtpCh:          make(chan *rtp.Packet, maxSize),
		rtcpCh:         make(chan rtcp.Packet, maxSize),
		rtpExtInfoChan: make(chan rtpExtInfo, maxSize),
		rembCycle:      config.REMBCycle,
		pliCycle:       config.PLICycle,
		tccCycle:       config.TCCCycle,
		maxBandwidth:   config.MaxBandwidth,
	}

	for _, feedback := range track.Codec().RTCPFeedback {
		switch feedback.Type {
		case webrtc.TypeRTCPFBTransportCC:
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBTransportCC)
			v.feedback = webrtc.TypeRTCPFBTransportCC
			go v.tccLoop()
		case webrtc.TypeRTCPFBGoogREMB:
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBGoogREMB)
			v.feedback = webrtc.TypeRTCPFBGoogREMB
			go v.rembLoop()
		}
	}
	go v.receiveRTP()
	go v.pliLoop()
	go v.bufferRtcpLoop()
	return v
}

// ReadRTP read rtp packets
func (v *WebRTCVideoReceiver) ReadRTP() (*rtp.Packet, error) {
	rtp, ok := <-v.rtpCh
	if !ok {
		return nil, errChanClosed
	}
	return rtp, nil
}

// ReadRTCP read rtcp packets
func (v *WebRTCVideoReceiver) ReadRTCP() (rtcp.Packet, error) {
	rtcp, ok := <-v.rtcpCh
	if !ok {
		return nil, errChanClosed
	}
	return rtcp, nil
}

// WriteRTCP write rtcp packet
func (v *WebRTCVideoReceiver) WriteRTCP(pkt rtcp.Packet) error {
	v.rtcpCh <- pkt
	return nil
}

// Track returns receiver track
func (v *WebRTCVideoReceiver) Track() *webrtc.Track {
	return v.track
}

// GetPacket get a buffered packet if we have one
func (v *WebRTCVideoReceiver) GetPacket(sn uint16) *rtp.Packet {
	return v.buffer.GetPacket(sn)
}

// Close track
func (v *WebRTCVideoReceiver) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stop {
		return
	}
	v.stop = true
	v.buffer.Stop()
}

// receiveRTP receive all incoming tracks' rtp and sent to one channel
// and use buffer to manager nack
func (v *WebRTCVideoReceiver) receiveRTP() {
	for {
		v.mu.RLock()
		if v.stop {
			return
		}
		v.mu.RUnlock()
		pkt, err := v.track.ReadRTP()
		log.Tracef("got packet %v", pkt)
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Errorf("rtp err => %v", err)
		}
		v.buffer.Push(pkt)
		if v.feedback == webrtc.TypeRTCPFBTransportCC {
			//store arrival time
			timestampUs := time.Now().UnixNano() / 1000
			rtpTCC := rtp.TransportCCExtension{}
			err = rtpTCC.Unmarshal(pkt.GetExtension(tccExtMapID))
			if err == nil {
				v.rtpExtInfoChan <- rtpExtInfo{
					TSN:       rtpTCC.TransportSequence,
					Timestamp: timestampUs,
				}
			}
		}
		v.rtpCh <- pkt
		if err != nil {
			log.Errorf("jb err => %v", err)
		}
	}
}

func (v *WebRTCVideoReceiver) pliLoop() {
	for {
		v.mu.RLock()
		if v.stop {
			return
		}
		v.mu.RUnlock()

		if v.pliCycle <= 0 {
			time.Sleep(time.Second)
			continue
		}

		time.Sleep(time.Duration(v.pliCycle) * time.Second)
		pli := &rtcp.PictureLossIndication{SenderSSRC: uint32(v.track.SSRC()), MediaSSRC: uint32(v.track.SSRC())}
		// log.Infof("pliLoop send pli=%d pt=%v", buffer.GetSSRC(), buffer.GetPayloadType())

		v.rtcpCh <- pli
	}
}

func (v *WebRTCVideoReceiver) bufferRtcpLoop() {
	for pkt := range v.buffer.GetRTCPChan() {
		v.mu.RLock()
		if v.stop {
			return
		}
		v.mu.RUnlock()
		v.rtcpCh <- pkt
	}
}

func (v *WebRTCVideoReceiver) rembLoop() {
	for {
		v.mu.RLock()
		if v.stop {
			return
		}
		v.mu.RUnlock()

		if v.rembCycle <= 0 {
			time.Sleep(time.Second)
			continue
		}

		time.Sleep(time.Duration(v.rembCycle) * time.Second)
		// only calc video recently
		v.lostRate, v.bandwidth = v.buffer.GetLostRateBandwidth(uint64(v.rembCycle))
		var bw uint64
		if v.lostRate == 0 && v.bandwidth == 0 {
			bw = uint64(v.maxBandwidth)
		} else if v.lostRate >= 0 && v.lostRate < 0.1 {
			bw = uint64(v.bandwidth * 2)
		} else {
			bw = uint64(float64(v.bandwidth) * (1 - v.lostRate))
		}
		if bw < minBandwidth {
			bw = minBandwidth
		}

		if bw > uint64(v.maxBandwidth) {
			bw = uint64(v.maxBandwidth)
		}
		remb := &rtcp.ReceiverEstimatedMaximumBitrate{
			SenderSSRC: v.buffer.GetSSRC(),
			Bitrate:    float32(bw * 1000),
			SSRCs:      []uint32{v.buffer.GetSSRC()},
		}
		v.rtcpCh <- remb
	}
}

// rtp扩展头
// 0                   1                   2                   3
// 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |       0xBE    |    0xDE       |           length=1            |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |  ID   | L=1   |transport-wide sequence number | zero padding  |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

//sequence number：媒体层序列号，每个媒体流独立递增，比如音频视频是不同的SSRC，也是不同的序列号。
//transport-wide sequence number：传输层序列号，对于同一个流发送的所有包使用同一个计数器，音视频使用同一个序列号递增。

// rtcp包结构（twcc）
//    0                   1                   2                   3
//    0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |V=2|P|  FMT=15 |    PT=205     |           length              |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |                     SSRC of packet sender                     |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |                      SSRC of media source                     |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |      base sequence number     |      packet status count      |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |                 reference time                | fb pkt. count |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |          packet chunk         |         packet chunk          |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   .                                                               .
//   .                                                               .
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |         packet chunk          |  recv delta   |  recv delta   |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   .                                                               .
//   .                                                               .
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//   |           recv delta          |  recv delta   | zero padding  |
//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

func (v *WebRTCVideoReceiver) tccLoop() {
	feedbackPacketCount := uint8(0) // feedback packet count | 反馈包号 |本包是第几个transport-cc包，每次加1
	t := time.NewTicker(time.Duration(v.tccCycle) * time.Second)
	defer t.Stop()

	for {
		if v.stop {
			return
		}
		<-t.C

		cap := len(v.rtpExtInfoChan)
		if cap == 0 {
			return
		}

		// get all rtp extension infos from channel。
		// rtpExtInfo[info.TSN][info.Timestamp]
		rtpExtInfo := make(map[uint16]int64)
		for i := 0; i < cap; i++ {
			info := <-v.rtpExtInfoChan
			rtpExtInfo[info.TSN] = info.Timestamp
		}

		//find the min and max transport sn
		var minTSN, maxTSN uint16
		for tsn := range rtpExtInfo {
			//init
			if minTSN == 0 {
				minTSN = tsn
			}

			if minTSN > tsn {
				minTSN = tsn
			}

			if maxTSN < tsn {
				maxTSN = tsn
			}
		}

		//      force small deta rtcp.RunLengthChunk
		//   	 0                   1
		//       0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5
		//      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		//      |T| S |       Run Length        |
		//      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		chunk := &rtcp.RunLengthChunk{
			Type:               rtcp.TypeTCCRunLengthChunk,
			PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta,
			RunLength:          maxTSN - minTSN + 1,
		}
		//gather deltas 接收时间差
		var recvDeltas []*rtcp.RecvDelta
		var refTime uint32
		var lastTS int64
		var baseTimeTicks int64
		for i := minTSN; i <= maxTSN; i++ {
			ts, ok := rtpExtInfo[i]
			//lost packet
			if !ok {
				recvDelta := &rtcp.RecvDelta{
					Type: rtcp.TypeTCCPacketReceivedSmallDelta,
				}
				recvDeltas = append(recvDeltas, recvDelta)
				continue
			}
			// init lastTS
			if lastTS == 0 {
				lastTS = ts
			}

			//received packet
			if baseTimeTicks == 0 {
				baseTimeTicks = (ts % timeWrapPeriodUs) / baseScaleFactor
			}
			var delta int64
			if lastTS == ts {
				delta = ts%timeWrapPeriodUs - baseTimeTicks*baseScaleFactor
			} else {
				delta = (ts - lastTS) % timeWrapPeriodUs
			}

			if refTime == 0 {
				refTime = uint32(baseTimeTicks) & 0x007FFFFF
			}

			recvDelta := &rtcp.RecvDelta{
				Type:  rtcp.TypeTCCPacketReceivedSmallDelta,
				Delta: delta,
			}
			recvDeltas = append(recvDeltas, recvDelta)
		}
		rtcpTCC := &rtcp.TransportLayerCC{
			Header: rtcp.Header{
				Padding: false,
				Count:   rtcp.FormatTCC,
				Type:    rtcp.TypeTransportSpecificFeedback,
			},
			MediaSSRC:          uint32(v.track.SSRC()),
			BaseSequenceNumber: minTSN,
			PacketStatusCount:  maxTSN - minTSN + 1,
			ReferenceTime:      refTime,
			FbPktCount:         feedbackPacketCount,
			PacketChunks:       []rtcp.PacketStatusChunk{chunk},
			RecvDeltas:         recvDeltas,
		}
		rtcpTCC.Header.Length = rtcpTCC.Len()/4 - 1
		if !v.stop {
			v.rtcpCh <- rtcpTCC
			feedbackPacketCount++
		}
	}
}

// Stats get stats for video receiver
func (v *WebRTCVideoReceiver) stats() string {
	return fmt.Sprintf("payload: %d | lostRate: %.2f | bandwidth: %dkbps", v.buffer.GetPayloadType(), v.lostRate, v.bandwidth)
}
