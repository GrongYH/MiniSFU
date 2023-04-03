package buffer

import "sync"

// Factor 工厂类，可以生产rtp报文的ReadWriteCloser 以及 rtcp报文的ReadWriteCloser
type Factory struct {
	sync.RWMutex
	videoPool  *sync.Pool
	audioPool  *sync.Pool
	rtpBuffers map[uint32]*Buffer
	rtcpReader map[uint32]*RTCPReader
}
