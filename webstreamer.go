package main

import (
	"bytes"
	"fmt"
	"log"

	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"golang.org/x/net/websocket"
)

var webRTCAPI *webrtc.API

func InitWebRTC(portMin, portMax uint16, hostIP []string) error {
	var err error

	m := &webrtc.MediaEngine{}
	err = m.RegisterDefaultCodecs()
	if err != nil {
		return err
	}

	i := &interceptor.Registry{}
	err = webrtc.RegisterDefaultInterceptors(m, i)
	if err != nil {
		return err
	}

	var e webrtc.SettingEngine
	if portMin > 0 && portMax > 0 {
		err = e.SetEphemeralUDPPortRange(portMin, portMax)
		if err != nil {
			return err
		}
		log.Println("Set UDP ports to", portMin, "..", portMax)
	}

	if len(hostIP) > 0 {
		e.SetNAT1To1IPs(hostIP, webrtc.ICECandidateTypeHost)
		log.Println("Set ICECandidates", hostIP)
	}

	webRTCAPI = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(e))

	return nil
}

type WebRTCStreamer struct {
	WS     *websocket.Conn
	pc     *webrtc.PeerConnection
	tracks map[int8]*webrtc.TrackLocalStaticSample
	codecs []av.CodecData
	stateC chan webrtc.ICEConnectionState
}

func (s *WebRTCStreamer) run(url, sdp string) {
	// log.Println("Enter WebRTCStreamer.run")
	// defer log.Println("Exit WebRTCStreamer.run")
	var err error

	err = s.setup(Config.coGe(url))
	if err != nil {
		log.Println(err)
		return
	}
	var state webrtc.ICEConnectionState
	defer func() {
		err := s.pc.Close()
		if err != nil {
			log.Println("failed close ICE connection", err)
		}
		for state != webrtc.ICEConnectionStateClosed {
			state = <-s.stateC
		}
	}()

	promise, err := s.gather(sdp)
	if err != nil {
		log.Println(err)
		return
	}

	// timeout := time.NewTimer(10 * time.Second)
	// defer timeout.Stop()
	for {
		select {
		// case <-timeout.C:
		// 	log.Printf("timeout at WebRTC gather promise during offer/answer")
		// 	return
		case state = <-s.stateC:
			switch state {
			case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed:
				log.Println("disconnected/failed ICE connection during offer/answer")
				return
			}
			continue
		case <-promise:
			//Connected
			// if !timeout.Stop() {
			// 	<-timeout.C
			// }
		}
		break
	}
	resp := s.pc.LocalDescription()

	err = websocket.JSON.Send(s.WS, Response{Type: "webrtc", Sdp: resp.SDP})
	if err != nil {
		log.Println("websocket.JSON.Send", err)
		// websocket is prolly broken, better close it, instead of let it retry.
		// it might be better to do it at the original WS go routine,
		// and launch the second go routine after signaling is done.
		if err := s.WS.Close(); err != nil {
			// log.Println("failed close websocket:", err)
		}
		return
	}

	// TODO: try to use single cid/ch
	cid, ch := Config.clAd(url)
	defer Config.clDe(url, cid)

	var start bool
	// timeout.Reset(10 * time.Second)
	for {
		switch state {
		case webrtc.ICEConnectionStateNew, webrtc.ICEConnectionStateChecking:
			state = <-s.stateC
			continue
		case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed:
			log.Println("disconnected ICE connection")
			return
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			select {
			case state = <-s.stateC:
				continue
			// case <-timeout.C:
			// 	log.Println("noVideo")
			// 	return
			case pck := <-ch:
				if pck.IsKeyFrame {
					// if !timeout.Stop() {
					// 	<-timeout.C
					// }
					// timeout.Reset(10 * time.Second)
					start = true
				}
				if !start {
					continue
				}
				// check unsupported codecs inside
				err = s.WritePacket(&pck)
				if err != nil {
					log.Println("muxerWebRTC.WritePacket", err)
					return
				}
				continue
			}
		default:
			log.Println("unexpected ICE connection state:", state)
		}
		break
	}
}

func (s *WebRTCStreamer) setup(codecs []av.CodecData) error {
	var err error

	tracks := make(map[int8]*webrtc.TrackLocalStaticSample)
	for i, c := range codecs {
		var track *webrtc.TrackLocalStaticSample
		if c.Type().IsVideo() {
			if c.Type() == av.H264 {
				track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
					MimeType: webrtc.MimeTypeH264,
				}, "pion-rtsp-video", "pion-rtsp-video")
				if err != nil {
					return err
				}
			}
		} else if c.Type().IsAudio() {
			AudioCodecString := webrtc.MimeTypePCMA
			switch c.Type() {
			case av.PCM_ALAW:
				AudioCodecString = webrtc.MimeTypePCMA
			case av.PCM_MULAW:
				AudioCodecString = webrtc.MimeTypePCMU
			case av.OPUS:
				AudioCodecString = webrtc.MimeTypeOpus
			default:
				log.Println("WebRTC Ignore Audio Track codec not supported WebRTC support only PCM_ALAW or PCM_MULAW")
				continue
			}
			track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
				MimeType:  AudioCodecString,
				Channels:  uint16(c.(av.AudioCodecData).ChannelLayout().Count()),
				ClockRate: uint32(c.(av.AudioCodecData).SampleRate()),
			}, "pion-rtsp-audio", "pion-rtsp-audio")
			if err != nil {
				return err
			}
		}
		tracks[int8(i)] = track
	}
	if len(tracks) == 0 {
		return fmt.Errorf("WebRTC Not Track Available")
	}

	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	if servers := Config.GetICEServers(); len(servers) > 0 {
		c.ICEServers = append(c.ICEServers, webrtc.ICEServer{
			URLs:           servers,
			Username:       Config.GetICEUsername(),
			Credential:     Config.GetICECredential(),
			CredentialType: webrtc.ICECredentialTypePassword,
		})
		log.Println("Set ICEServers", servers)
	}

	if webRTCAPI == nil {
		panic("webrtcstreamer.go: WebRTC was not initialized")
	}
	pc, err := webRTCAPI.NewPeerConnection(c)
	if err != nil {
		return err
	}

	for _, track := range tracks {
		sender, err := pc.AddTrack(track)
		if err != nil {
			if err := pc.Close(); err != nil {
				log.Println("failed close WebRTC peer connection: ", err)
			}
			return err
		}
		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
	}

	stateC := make(chan webrtc.ICEConnectionState)
	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		stateC <- connectionState
		// element.status = connectionState
		// if connectionState == webrtc.ICEConnectionStateDisconnected {
		// 	element.Close()
		// }
	})
	// peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
	// 	d.OnMessage(func(msg webrtc.DataChannelMessage) {
	// 		element.ClientACK.Reset(5 * time.Second)
	// 	})
	// })
	s.pc = pc
	s.stateC = stateC
	s.tracks = tracks
	s.codecs = codecs

	return nil
}

func (s *WebRTCStreamer) gather(sdp string) (promise <-chan struct{}, err error) {
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}

	err = s.pc.SetRemoteDescription(offer)
	if err != nil {
		return nil, err
	}

	promise = webrtc.GatheringCompletePromise(s.pc)

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	err = s.pc.SetLocalDescription(answer)
	if err != nil {
		return nil, err
	}

	return promise, nil
}

func (s *WebRTCStreamer) WritePacket(pkt *av.Packet) error {
	var err error

	track := s.tracks[pkt.Idx]
	if track == nil {
		return nil
	}

	// element.StreamACK.Reset(10 * time.Second)
	if len(pkt.Data) < 5 {
		return nil
	}
	c := s.codecs[pkt.Idx]
	switch c.Type() {
	case av.H264:
		nalus, _ := h264parser.SplitNALUs(pkt.Data)
		for _, nalu := range nalus {
			naltype := nalu[0] & 0x1f
			if naltype == 5 {
				codec := c.(h264parser.CodecData)
				err = track.WriteSample(media.Sample{Data: bytes.Join([][]byte{{}, codec.SPS(), codec.PPS(), nalu}, []byte{0, 0, 0, 1}), Duration: pkt.Duration})
			} else if naltype == 1 {
				err = track.WriteSample(media.Sample{Data: append([]byte{0, 0, 0, 1}, nalu...), Duration: pkt.Duration})
			}
			if err != nil {
				return err
			}
		}
		// WritePacketSuccess = true
		return nil
		/*

			if pkt.IsKeyFrame {
				pkt.Data = append([]byte{0, 0, 0, 1}, bytes.Join([][]byte{codec.SPS(), codec.PPS(), pkt.Data[4:]}, []byte{0, 0, 0, 1})...)
			} else {
				pkt.Data = pkt.Data[4:]
			}

		*/
	case av.PCM_ALAW:
	case av.OPUS:
	case av.PCM_MULAW:
	case av.AAC:
		//TODO: NEED ADD DECODER AND ENCODER
		return fmt.Errorf("WebRTC Codec Not Supported")
	case av.PCM:
		//TODO: NEED ADD ENCODER
		return fmt.Errorf("WebRTC Codec Not Supported")
	default:
		return fmt.Errorf("WebRTC Codec Not Supported")
	}

	err = track.WriteSample(media.Sample{Data: pkt.Data, Duration: pkt.Duration})
	if err != nil {
		return err
	}

	return nil
}
