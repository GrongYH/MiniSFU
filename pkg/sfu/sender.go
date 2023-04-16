package sfu

import (
	"fmt"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"io"
	"math"
	"miniSFU/log"
	"sync"
	"time"
)

// Sender defines a interface for a track receiver
type Sender interface {
	ReadRTCP() (rtcp.Packet, error)
	WriteRTP(*rtp.Packet)
	stats() string
	Close()
}

// WebRTCSender represents a Sender which writes RTP to a webrtc track
type WebRTCSender struct {
	mu     sync.RWMutex
	track  *webrtc.Track
	stop   bool
	rtcpCh chan rtcp.Packet

	useRemb  bool
	rembCh   chan *rtcp.ReceiverEstimatedMaximumBitrate
	target   float32
	last     uint16
	sendChan chan *rtp.Packet
}

// NewWebRTCSender creates a new track sender instance
func NewWebRTCSender(track *webrtc.Track, sender *webrtc.RTPSender) *WebRTCSender {
	s := &WebRTCSender{
		track:    track,
		rtcpCh:   make(chan rtcp.Packet, maxSize),
		rembCh:   make(chan *rtcp.ReceiverEstimatedMaximumBitrate, maxSize),
		sendChan: make(chan *rtp.Packet, maxSize),
	}

	for _, feedback := range track.Codec().RTCPFeedback {
		switch feedback.Type {
		case webrtc.TypeRTCPFBGoogREMB:
			log.Debugf("Using sender feedback %s", webrtc.TypeRTCPFBGoogREMB)
			s.useRemb = true
			go s.rembLoop()
		case webrtc.TypeRTCPFBTransportCC:
			log.Debugf("Using sender feedback %s", webrtc.TypeRTCPFBTransportCC)
			// TODO
		}
	}

	go s.receiveRTCP(sender)
	// add rtp to track
	go s.sendRTP()

	return s
}

func (s *WebRTCSender) sendRTP() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for pkt := range s.sendChan {
		pt := s.track.Codec().PayloadType
		newpkt := *pkt
		newpkt.Header.PayloadType = uint8(pt)
		pkt = &newpkt

		if err := s.track.WriteRTP(pkt); err != nil {
			log.Errorf("wt.WriteRTP err=%v", err)
		}
		s.last = pkt.SequenceNumber
	}
	log.Infof("Closing send writer")
}

// ReadRTCP read rtp packet
func (s *WebRTCSender) ReadRTCP() (rtcp.Packet, error) {
	rtcp, ok := <-s.rtcpCh
	if !ok {
		return nil, errChanClosed
	}
	return rtcp, nil
}

// WriteRTP to the track
func (s *WebRTCSender) WriteRTP(pkt *rtp.Packet) {
	s.sendChan <- pkt
}

// Close track
func (s *WebRTCSender) Close() {
	s.stop = true
	close(s.sendChan)
}

func (s *WebRTCSender) receiveRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, err := sender.ReadRTCP()
		if err == io.EOF || err == io.ErrClosedPipe {
			return
		}

		if err != nil {
			log.Errorf("rtcp err => %v", err)
		}

		if s.stop {
			return
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest, *rtcp.TransportLayerNack:
				s.rtcpCh <- pkt
			case *rtcp.ReceiverEstimatedMaximumBitrate:
				if s.useRemb {
					s.rembCh <- pkt
				}
			default:
			}
		}
	}
}

func (s *WebRTCSender) rembLoop() {
	lastRembTime := time.Now()
	maxRembTime := 200 * time.Millisecond
	rembMin := float32(100000)
	rembMax := float32(5000000)
	if rembMin == 0 {
		rembMin = 10000 // 10 KBit
	}
	if rembMax == 0 {
		rembMax = 100000000 // 100 MBit
	}
	var lowest float32 = math.MaxFloat32
	var rembCount uint64
	var rembTotalRate float32
	for pkt := range s.rembCh {
		// Update stats
		rembCount++
		rembTotalRate += pkt.Bitrate
		if pkt.Bitrate < lowest {
			lowest = pkt.Bitrate
		}

		// Send upstream if time
		if time.Since(lastRembTime) > maxRembTime {
			lastRembTime = time.Now()
			s.target = lowest
			if s.target < rembMin {
				s.target = rembMin
			} else if s.target > rembMax {
				s.target = rembMax
			}

			newPkt := &rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate:    s.target,
				SenderSSRC: 1,
				SSRCs:      pkt.SSRCs,
			}
			s.rtcpCh <- newPkt

			// Reset stats
			rembCount = 0
			rembTotalRate = 0
			lowest = math.MaxFloat32
		}
	}
}

func (s *WebRTCSender) stats() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("payload: %d | remb: %dkbps | pkt: %d", s.track.PayloadType(), s.target/1000, s.last)
}