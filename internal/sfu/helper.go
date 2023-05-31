package sfu

import (
	"strings"
	"sync/atomic"

	"github.com/pion/webrtc/v3"
)

const (
	ntpEpoch = 2208988800
)

type atomicBool int32

func (a *atomicBool) set(value bool) {
	var i int32
	if value {
		i = 1
	}
	atomic.StoreInt32((*int32)(a), i)
}

func (a *atomicBool) get() bool {
	return atomic.LoadInt32((*int32)(a)) != 0
}

// codecParametersFuzzySearch 在编解码器列表中模糊查找编解码器，并且找到匹配项
func codecParametersFuzzySearch(needle webrtc.RTPCodecParameters, haystack []webrtc.RTPCodecParameters) (webrtc.RTPCodecParameters, error) {
	// 首先尝试在MimeType和SDPFmtpLine上进行匹配（对应SDP中的 a=rtpmap中如opus，a=fmtp中的fmtpline)
	for _, c := range haystack {
		if strings.EqualFold(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) &&
			c.RTPCodecCapability.SDPFmtpLine == needle.RTPCodecCapability.SDPFmtpLine {
			// 匹配成功，返回编解码器参数
			return c, nil
		}
	}

	//如果MimeType和SDPFmtpLine上没有匹配上，只匹配MimeType
	for _, c := range haystack {
		if strings.EqualFold(c.RTPCodecCapability.MimeType, needle.RTPCodecCapability.MimeType) {
			return c, nil
		}
	}
	// 没有匹配到，codec not found
	return webrtc.RTPCodecParameters{}, webrtc.ErrCodecNotFound
}

func timeToNtp(ns int64) uint64 {
	seconds := uint64(ns/1e9 + ntpEpoch)
	fraction := uint64(((ns % 1e9) << 32) / 1e9)
	return seconds<<32 | fraction
}
