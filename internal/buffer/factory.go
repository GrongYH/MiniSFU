package buffer

import (
	"io"
	"sync"

	"github.com/pion/transport/v2/packetio"
)

// Factory 工厂类，可以生产rtp报文的ReadWriteCloser 以及 rtcp报文的ReadWriteCloser
type Factory struct {
	sync.RWMutex
	videoPool  *sync.Pool
	audioPool  *sync.Pool
	rtpBuffers map[uint32]*Buffer
	rtcpReader map[uint32]*RTCPReader
}

func NewBufferFactory() *Factory {
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

func (f *Factory) GetOrNew(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser {
	f.Lock()
	defer f.Unlock()

	switch packetType {
	case packetio.RTCPBufferPacket:
		if reader, ok := f.rtcpReader[ssrc]; ok {
			return reader
		}
		reader := NewRTCPReader(ssrc)
		f.rtcpReader[ssrc] = reader
		reader.OnClose(func() {
			f.Lock()
			delete(f.rtcpReader, ssrc)
			f.Unlock()
		})
		return reader
	case packetio.RTPBufferPacket:
		if reader, ok := f.rtpBuffers[ssrc]; ok {
			return reader
		}
		buffer := NewBuffer(ssrc, f.videoPool, f.audioPool)
		f.rtpBuffers[ssrc] = buffer
		buffer.OnClose(func() {
			f.Lock()
			delete(f.rtpBuffers, ssrc)
			f.Unlock()
		})
		return buffer
	}
	return nil
}

func (f *Factory) GetBufferPair(ssrc uint32) (*RTCPReader, *Buffer) {
	f.RLock()
	defer f.RUnlock()
	return f.rtcpReader[ssrc], f.rtpBuffers[ssrc]
}

func (f *Factory) GetBuffer(ssrc uint32) *Buffer {
	f.RLock()
	defer f.RUnlock()
	return f.rtpBuffers[ssrc]
}

func (f *Factory) GetRTCPReader(ssrc uint32) *RTCPReader {
	f.RLock()
	defer f.RUnlock()
	return f.rtcpReader[ssrc]
}
