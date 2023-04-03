package buffer

import "sync"

// Factory 工厂类，可以生产rtp报文的ReadWriteCloser 以及 rtcp报文的ReadWriteCloser
type Factory struct {
	sync.RWMutex
	videoPool  *sync.Pool
	audioPool  *sync.Pool
	rtpBuffers map[uint32]*Buffer
	rtcpReader map[uint32]*RTCPReader
}

func NewFactory() *Factory {
	return &Factory{
		videoPool: &sync.Pool{
			New: func() interface{} {
				// 2MB的视频buffer
				return make([]byte, 2*1024*1024)
			},
		},
		audioPool: &sync.Pool{
			New: func() interface{} {
				// 创建的buffer最多可以装25个音频packet
				return make([]byte, maxPktSize*25)
			},
		},
		rtpBuffers: make(map[uint32]*Buffer),
		rtcpReader: make(map[uint32]*RTCPReader),
	}
}
