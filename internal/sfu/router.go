package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"mini-sfu/internal/buffer"
	"mini-sfu/internal/log"
	"sync"
)

type RouterConfig struct {
	MaxBufferTime int             `json:"maxBufferTime"`
	MaxBandwidth  uint64          `json:"maxBandwidth"`
	Simulcast     SimulcastConfig `json:"simulcast"`
}

type Router interface {
	ID() string
	AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool)
	AddDownTracks(s *Subscriber, r Receiver) error
	Stop()
}

// Router manager a track rtp/rtcp router
type router struct {
	sync.RWMutex
	id     string
	config RouterConfig

	session *Session
	// 实际上是Publisher的pc
	pc *webrtc.PeerConnection

	rtcpCh    chan []rtcp.Packet
	stopCh    chan struct{}
	twcc      *buffer.Responder
	receivers map[string]Receiver
}

// NewRouter for routing rtp/rtcp packets
func newRouter(tid string, pc *webrtc.PeerConnection, session *Session, config RouterConfig) Router {
	r := &router{
		id:        tid,
		pc:        pc,
		session:   session,
		config:    config,
		receivers: make(map[string]Receiver),
		rtcpCh:    make(chan []rtcp.Packet, 10),
		stopCh:    make(chan struct{}),
	}
	go r.sendRTCP()
	return r
}

func (r *router) ID() string {
	return r.id
}

func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote) (Receiver, bool) {
	r.Lock()
	defer r.Unlock()

	publish := false
	trackID := track.ID()
	rid := track.RID()

	//这里获取了之前init函数中，new出来的buffer和rtcpReader
	//需要注意的是，开启大小流后，三层的streamId、trackId是一样的，但是rid和ssrc是不同的，rid一般是f、h、q
	//因此同一个trackId的不同层，获取的buffer是不一样的
	rtcpReader, buff := bufferFactory.GetBufferPair(uint32(track.SSRC()))

	// buffer设置回调
	buff.OnFeedback(func(fb []rtcp.Packet) {
		r.rtcpCh <- fb
	})

	//如果是视频track，创建twcc计算器，并设置回调，当计算器生成twcc包就会回调
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		if r.twcc == nil {
			// 一个Router管理多个Receiver，多个Receiver用同一个twcc计算器，因为这里的sn是传输层的sn
			r.twcc = buffer.NewTransportWideCCResponder(uint32(track.SSRC()))
			r.twcc.OnFeedback(func(p rtcp.RawPacket) {
				r.rtcpCh <- []rtcp.Packet{&p}
			})
		}

		buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
			r.twcc.Push(sn, timeNS, marker)
		})
	}

	//如果收到了rtcp报文，判断是否是SR报文，记录该报文到来的时间
	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			log.Errorf("Unmarshal rtcp receiver packets err: %v", err)
			return
		}

		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SenderReport:
				buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
			}
		}
	})

	recv, ok := r.receivers[trackID]
	if !ok {
		recv = NewWebRTCReceiver(receiver, track, r.id)
		r.receivers[trackID] = recv
		// 所有receiver的rtcp报文统一写入到router的rtcpCh，由router发送给publisher
		recv.SetRTCPCh(r.rtcpCh)
		recv.OnCloseHandler(func() {
			r.Lock()
			delete(r.receivers, trackID)
			r.Unlock()
		})
		if len(rid) == 0 || r.config.Simulcast.BestQualityFirst && rid == fullResolution ||
			!r.config.Simulcast.BestQualityFirst && rid == quarterResolution {
			publish = true
		}
	} else if r.config.Simulcast.BestQualityFirst && rid == fullResolution ||
		!r.config.Simulcast.BestQualityFirst && rid == quarterResolution ||
		!r.config.Simulcast.BestQualityFirst && rid == halfResolution {
		publish = true
	}

	// 添加上行Track
	recv.AddUpTrack(track, buff)
	buff.Bind(receiver.GetParameters(), buffer.Options{
		MaxBitRate: r.config.MaxBandwidth,
	})

	return recv, publish
}

func (r *router) DeleteReceiver(track string) {
	r.Lock()
	defer r.Unlock()
	delete(r.receivers, track)
}

// AddDownTracks 给Subscriber和receiver中添加DownTrack
func (r *router) AddDownTracks(s *Subscriber, recv Receiver) error {
	r.Lock()
	defer r.Unlock()

	if recv != nil {
		if err := r.addDownTrack(s, recv); err != nil {
			log.Errorf("Peer %s pub track to peer %s error: %v", r.id, s.id, err)
			return err
		}
		s.negotiate()

	}

	if len(r.receivers) > 0 {
		for _, rcv := range r.receivers {
			if err := r.addDownTrack(s, rcv); err != nil {
				log.Errorf("Peer %s sub track from peer %s error: %v", r.id, s.id, err)
				return err
			}
		}
		s.negotiate()
	}
	return nil
}

func (r *router) Stop() {
	r.stopCh <- struct{}{}
}

// addDownTrack 将downTrack添加到sub，并挂载到recv中
func (r *router) addDownTrack(sub *Subscriber, recv Receiver) error {
	// 避免重复添加downTrack
	for _, dt := range sub.GetDownTracks(recv.StreamID()) {
		if dt.ID() == recv.TrackID() {
			return nil
		}
	}

	codec := recv.Codec()
	//if err := sub.me.RegisterCodec(codec, recv.Kind()); err != nil {
	//	log.Errorf("peer %s subscriber register codec failed, error: %v", sub.id, err)
	//	return err
	//}
	//sub.me.RegisterDefaultCodecs()
	//log.Debugf("sub.me %v", sub.me)

	// 创建downTrack，用于给客户端下发流，downTrack标识了被谁订阅
	// 因此一个receiver中同一层的不同downTrack编解码参数是一样的，不一样的是sub.id
	downTrack, err := NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: []webrtc.RTCPFeedback{{"goog-remb", ""}, {"nack", ""}, {"nack", "pli"}},
	}, recv, sub.id)
	if err != nil {
		log.Errorf("peer %s create downTrack failed, error: %v", r.id, err)
		return err
	}
	//把downTrack增加到pc中
	if downTrack.transceiver, err = sub.pc.AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
		// 方向为sendonly
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		log.Errorf("peer %s add downTrack to pc error: %v", r.id, err)
		return err
	}

	//删除downTrack回调
	downTrack.OnCloseHandler(func() {
		if sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			// 从pc中删除
			err := sub.pc.RemoveTrack(downTrack.transceiver.Sender())
			if err != nil {
				if err == webrtc.ErrConnectionClosed {
					return
				}
				log.Errorf("Error closing down track: %v", err)
			} else {
				// 从subscriber删除
				sub.RemoveDownTrack(recv.StreamID(), downTrack)
				// 从subscriber删除downtrack时，需要进行重协商
				sub.negotiate()
			}
		}
	})

	// 协商成功后，开始发送源描述报文
	downTrack.OnBind(func() {
		go sub.sendStreamDownTracksReports(recv.StreamID())
	})

	// 增加downTrack到sub中，sub只是用来管理downTrack和生成SenderReport等
	sub.AddDownTrack(recv.StreamID(), downTrack)
	// 增加downTrack到WebRTCReceiver中，实际收发包是WebRTCReceiver来控制，在writeRTP中
	recv.AddDownTrack(downTrack, r.config.Simulcast.BestQualityFirst)
	log.Debugf("add downTrack to subscriber and receiver")
	return nil
}

// sendRTCP 发送RTCP报文(整合所有receiver收到的, 以及buffer中构造的twcc报文和nack，pli报文)
func (r *router) sendRTCP() {
	for {
		select {
		case pkt := <-r.rtcpCh:
			if err := r.pc.WriteRTCP(pkt); err != nil {
				log.Errorf("Write rtcp to peer %s err :%v", r.id, err)
			}
		case <-r.stopCh:
			log.Infof("Stop to send rtcp to peer %s", r.id)
			return
		}
	}
}
