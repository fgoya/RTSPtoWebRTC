package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/deepch/RTSPtoWebRTC/rtsp"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec"
	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/codec/h265parser"
	"github.com/pion/rtp"
)

type RTSPStream struct {
	name      string
	AudioOnly bool
	keyTest   *time.Timer

	fuStarted       bool
	BufferRtpPacket *bytes.Buffer
	// startVideoTS    int64
	// startAudioTS    int64
	videoIDX  int8
	audioIDX  int8
	CodecData []av.CodecData

	codecVideo        av.VideoCodecData
	sps               []byte
	pps               []byte
	vps               []byte
	codecAudio        av.AudioCodecData
	AudioTimeLine     time.Duration
	AudioTimeScale    int64
	audioCodec        av.CodecType
	videoCodec        av.CodecType
	PreAudioTS        int64
	PreVideoTS        int64
	PreSequenceNumber int
	FPS               int
}

func (s *RTSPStream) setupCodec(p *rtsp.Player) error {

	if s.CodecData != nil {
		return nil
	}

	var err error
	m := p.VideoMedia
	if m != nil {
		if m.Type == av.H264 {
			if len(m.SpropParameterSets) > 1 {
				if codecData, err := h264parser.NewCodecDataFromSPSAndPPS(m.SpropParameterSets[0], m.SpropParameterSets[1]); err == nil {
					s.sps = m.SpropParameterSets[0]
					s.pps = m.SpropParameterSets[1]
					s.codecVideo = codecData
					// log.Println("SPS", base64.StdEncoding.EncodeToString(s.sps), s.sps)
					// log.Println("PPS", base64.StdEncoding.EncodeToString(s.pps), s.pps)
				}
			} else {
				s.codecVideo = h264parser.CodecData{}
			}
			s.FPS = m.FPS
			s.videoCodec = av.H264
		} else if m.Type == av.H265 {
			if len(m.SpropVPS) > 1 && len(m.SpropSPS) > 1 && len(m.SpropPPS) > 1 {
				if codecData, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(m.SpropVPS, m.SpropSPS, m.SpropPPS); err == nil {
					s.vps = m.SpropVPS
					s.sps = m.SpropSPS
					s.pps = m.SpropPPS
					s.codecVideo = codecData
				}
			} else {
				s.codecVideo = h265parser.CodecData{}
			}
			s.videoCodec = av.H265
		} else {
			return fmt.Errorf("SDP Video Codec Type Not Supported %s", m.Type)
		}
		s.CodecData = append(s.CodecData, s.codecVideo)
		s.videoIDX = int8(len(s.CodecData) - 1)
	}

	m = p.AudioMedia
	if m != nil {
		switch m.Type {
		case av.AAC:
			s.codecAudio, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(m.Config)
			if err != nil {
				// return fmt.Errorf("audio AAC bad config: %#v", err)
				log.Printf("audio AAC bad config: %#v", err)
			}
		case av.OPUS:
			var cl av.ChannelLayout
			switch m.ChannelCount {
			case 1:
				cl = av.CH_MONO
			case 2:
				cl = av.CH_STEREO
			default:
				cl = av.CH_MONO
			}
			s.codecAudio = codec.NewOpusCodecData(m.TimeScale, cl)
		case av.PCM_MULAW:
			s.codecAudio = codec.NewPCMMulawCodecData()
		case av.PCM_ALAW:
			s.codecAudio = codec.NewPCMAlawCodecData()
		case av.PCM:
			s.codecAudio = codec.NewPCMCodecData()
		default:
			// return fmt.Errorf("audio Codec %s not supported", m.Type)
			log.Printf("audio Codec %s not supported", m.Type)
		}
		if s.codecAudio != nil {
			s.CodecData = append(s.CodecData, s.codecAudio)
			s.audioIDX = int8(len(s.CodecData) - 1)
			s.audioCodec = s.codecAudio.Type()
			if m.TimeScale != 0 {
				s.AudioTimeScale = int64(m.TimeScale)
			}
			if p.VideoMedia == nil {
				s.AudioOnly = true
			}
		}
	}

	// TODO: My camera actually inform wrong SPS and PPS in sdp, so this
	// actually is bad do it here, instead of when update SPS and PPS
	if len(s.CodecData) > 0 {
		Config.coAd(s.name, s.CodecData)
	}

	return nil
}

func (s *RTSPStream) onVideoPacket(player *rtsp.Player, p *rtp.Packet) error {

	err := s.setupCodec(player)
	if err != nil {
		return err
	}
	if s.codecVideo == nil {
		return nil
	}
	if s.PreVideoTS == 0 {
		s.PreVideoTS = int64(p.Timestamp)
	}
	if int64(p.Timestamp)-s.PreVideoTS < 0 {
		if math.MaxUint32-s.PreVideoTS < 90*100 { //100 ms
			s.PreVideoTS = 0
			s.PreVideoTS -= (math.MaxUint32 - s.PreVideoTS)
		} else {
			s.PreVideoTS = 0
		}
	}
	if s.PreSequenceNumber != 0 && int(p.SequenceNumber)-s.PreSequenceNumber != 1 {
		log.Println("drop packet", int(p.SequenceNumber)-1)
	}
	s.PreSequenceNumber = int(p.SequenceNumber)
	if s.BufferRtpPacket.Len() > 4048576 {
		log.Println("Big Buffer Flush")
		s.BufferRtpPacket.Truncate(0)
		s.BufferRtpPacket.Reset()
	}
	nalRaw, _ := h264parser.SplitNALUs(p.Payload)
	if len(nalRaw) == 0 || len(nalRaw[0]) == 0 {
		// return nil, false
		return fmt.Errorf("len(nalRaw) == 0 || len(nalRaw[0]) == 0")
	}
	var retmap []*av.Packet
	for _, nal := range nalRaw {
		if s.videoCodec == av.H265 {
			naluType := (nal[0] >> 1) & 0x3f
			switch naluType {
			case h265parser.NAL_UNIT_CODED_SLICE_TRAIL_R:
				retmap = append(retmap, &av.Packet{
					Data:            binSize(nal),
					CompositionTime: time.Duration(1) * time.Millisecond,
					Idx:             s.videoIDX,
					IsKeyFrame:      false,
					Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
					Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				})
			case h265parser.NAL_UNIT_VPS:
				s.CodecUpdateVPS(nal)
			case h265parser.NAL_UNIT_SPS:
				s.CodecUpdateSPS(nal)
			case h265parser.NAL_UNIT_PPS:
				s.CodecUpdatePPS(nal)
			case h265parser.NAL_UNIT_UNSPECIFIED_49:
				se := nal[2] >> 6
				naluType := nal[2] & 0x3f
				if se == 2 {
					s.BufferRtpPacket.Truncate(0)
					s.BufferRtpPacket.Reset()
					s.BufferRtpPacket.Write([]byte{(nal[0] & 0x81) | (naluType << 1), nal[1]})
					r := make([]byte, 2)
					r[1] = nal[1]
					r[0] = (nal[0] & 0x81) | (naluType << 1)
					s.BufferRtpPacket.Write(nal[3:])
				} else if se == 1 {
					s.BufferRtpPacket.Write(nal[3:])
					retmap = append(retmap, &av.Packet{
						Data:            binSize(s.BufferRtpPacket.Bytes()),
						CompositionTime: time.Duration(1) * time.Millisecond,
						Idx:             s.videoIDX,
						IsKeyFrame:      naluType == h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL,
						Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
						Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
					})
				} else {
					s.BufferRtpPacket.Write(nal[3:])
				}
			default:
				log.Println("Unsupported Nal", naluType)
			}

		} else if s.videoCodec == av.H264 {
			naluType := nal[0] & 0x1f
			switch {
			case naluType >= 1 && naluType <= 5:
				if naluType != 1 {
					log.Println("naluType >= 1 && naluType <= 5:", naluType)
				}
				retmap = append(retmap, &av.Packet{
					Data:            binSize(nal),
					CompositionTime: time.Duration(1) * time.Millisecond,
					Idx:             s.videoIDX,
					IsKeyFrame:      naluType == 5,
					Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
					Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				})
			case naluType == 7:
				// log.Println("naluType == 7")
				s.CodecUpdateSPS(nal)
				if len(nal) > 30 {
					log.Printf("naluType == 7 Packet: %#v", p)
				}
			case naluType == 8:
				// log.Println("naluType == 8")
				s.CodecUpdatePPS(nal)
			case naluType == 24:
				packet := nal[1:]
				for len(packet) >= 2 {
					size := int(packet[0])<<8 | int(packet[1])
					if size+2 > len(packet) {
						break
					}
					naluTypefs := packet[2] & 0x1f
					switch {
					case naluTypefs >= 1 && naluTypefs <= 5:
						retmap = append(retmap, &av.Packet{
							Data:            binSize(packet[2 : size+2]),
							CompositionTime: time.Duration(1) * time.Millisecond,
							Idx:             s.videoIDX,
							IsKeyFrame:      naluType == 5,
							Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
							Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
						})
					case naluTypefs == 7:
						s.CodecUpdateSPS(packet[2 : size+2])
					case naluTypefs == 8:
						s.CodecUpdatePPS(packet[2 : size+2])
					}
					packet = packet[size+2:]
				}
			case naluType == 28:
				fuIndicator := p.Payload[0]
				fuHeader := p.Payload[1]
				isStart := fuHeader&0x80 != 0
				isEnd := fuHeader&0x40 != 0
				// log.Println("naluType == 28:", fuIndicator, fuHeader, isStart, isEnd)
				if isStart {
					s.fuStarted = true
					s.BufferRtpPacket.Truncate(0)
					s.BufferRtpPacket.Reset()
					s.BufferRtpPacket.Write([]byte{fuIndicator&0xe0 | fuHeader&0x1f})
				}
				if s.fuStarted {
					if !bytes.Equal(p.Payload[2:], p.Payload[2:len(p.Payload)]) {
						log.Println("FIX: Reslicing payload, not sure this is the correct way at player.go:475")
					}
					s.BufferRtpPacket.Write(p.Payload[2:])
					if isEnd {
						s.fuStarted = false
						naluTypef := s.BufferRtpPacket.Bytes()[0] & 0x1f
						if naluTypef == 7 || naluTypef == 9 {
							bufered, _ := h264parser.SplitNALUs(append([]byte{0, 0, 0, 1}, s.BufferRtpPacket.Bytes()...))
							for _, v := range bufered {
								naluTypefs := v[0] & 0x1f
								switch {
								case naluTypefs == 5:
									// log.Println("naluTypefs == 5")
									s.BufferRtpPacket.Reset()
									s.BufferRtpPacket.Write(v)
									naluTypef = 5
								case naluTypefs == 7:
									// log.Println("naluTypefs == 7")
									// log.Println("SPS2", base64.StdEncoding.EncodeToString(v), v)
									s.CodecUpdateSPS(v)
								case naluTypefs == 8:
									// log.Println("naluTypefs == 8")
									// log.Println("PPS2", base64.StdEncoding.EncodeToString(v), v)
									s.CodecUpdatePPS(v)
								}
							}
						}
						retmap = append(retmap, &av.Packet{
							Data:            binSize(s.BufferRtpPacket.Bytes()),
							CompositionTime: time.Duration(1) * time.Millisecond,
							Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
							Idx:             s.videoIDX,
							IsKeyFrame:      naluTypef == 5,
							Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
						})
					}
				}
			default:
				log.Println("Unsupported NAL Type", naluType)
			}
		}
	}
	if len(retmap) > 0 {
		s.PreVideoTS = int64(p.Timestamp)
		// return retmap, true
	}
	for _, p := range retmap {
		if p.IsKeyFrame {
			if !s.keyTest.Stop() {
				<-s.keyTest.C
			}
			s.keyTest.Reset(20 * time.Second)
		}
		Config.cast(s.name, *p)
	}
	return nil
}

func (s *RTSPStream) CodecUpdateSPS(val []byte) {
	if s.videoCodec != av.H264 && s.videoCodec != av.H265 {
		return
	}
	if bytes.Equal(val, s.sps) {
		return
	}
	tmp := make([]byte, len(val))
	copy(tmp, val)
	val = tmp
	s.sps = val
	if (s.videoCodec == av.H264 && len(s.pps) == 0) || (s.videoCodec == av.H265 && (len(s.vps) == 0 || len(s.pps) == 0)) {
		return
	}
	var codecData av.VideoCodecData
	var err error
	switch s.videoCodec {
	case av.H264:
		log.Println("Codec Update SPS", base64.StdEncoding.EncodeToString(val), val)
		codecData, err = h264parser.NewCodecDataFromSPSAndPPS(val, s.pps)
		if err != nil {
			log.Println("Parse Codec Data Error", err)
			return
		}
	case av.H265:
		codecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(s.vps, val, s.pps)
		if err != nil {
			log.Println("Parse Codec Data Error", err)
			return
		}
	}
	s.codecVideo = codecData
	s.CodecData[int(s.videoIDX)] = s.codecVideo
	// if len(s.CodecData) > 0 {
	// 	for i, i2 := range s.CodecData {
	// 		if i2.Type().IsVideo() {
	// 			s.CodecData[i] = codecData
	// 		}
	// 	}
	// } else {
	// 	s.CodecData = append(s.CodecData, codecData)
	// }
	Config.coAd(s.name, s.CodecData)
}

func (s *RTSPStream) CodecUpdatePPS(val []byte) {
	if s.videoCodec != av.H264 && s.videoCodec != av.H265 {
		return
	}
	if bytes.Equal(val, s.pps) {
		return
	}
	tmp := make([]byte, len(val))
	copy(tmp, val)
	val = tmp
	s.pps = val
	if (s.videoCodec == av.H264 && len(s.sps) == 0) || (s.videoCodec == av.H265 && (len(s.vps) == 0 || len(s.sps) == 0)) {
		return
	}
	var codecData av.VideoCodecData
	var err error
	switch s.videoCodec {
	case av.H264:
		log.Println("Codec Update PPS", base64.StdEncoding.EncodeToString(val), val)
		codecData, err = h264parser.NewCodecDataFromSPSAndPPS(s.sps, val)
		if err != nil {
			log.Println("Parse Codec Data Error", err)
			return
		}
	case av.H265:
		codecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(s.vps, s.sps, val)
		if err != nil {
			log.Println("Parse Codec Data Error", err)
			return
		}
	}
	s.codecVideo = codecData
	s.CodecData[int(s.videoIDX)] = s.codecVideo
	// if len(s.CodecData) > 0 {
	// 	for i, i2 := range s.CodecData {
	// 		if i2.Type().IsVideo() {
	// 			s.CodecData[i] = codecData
	// 		}
	// 	}
	// } else {
	// 	s.CodecData = append(s.CodecData, codecData)
	// }
	Config.coAd(s.name, s.CodecData)
}

func (s *RTSPStream) CodecUpdateVPS(val []byte) {
	if s.videoCodec != av.H265 {
		return
	}
	if bytes.Equal(val, s.vps) {
		return
	}
	tmp := make([]byte, len(val))
	copy(tmp, val)
	val = tmp
	s.vps = val
	if len(s.sps) == 0 || len(s.pps) == 0 {
		return
	}
	codecData, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(val, s.sps, s.pps)
	if err != nil {
		log.Println("Parse Codec Data Error", err)
		return
	}
	s.codecVideo = codecData
	s.CodecData[int(s.videoIDX)] = s.codecVideo
	// if len(s.CodecData) > 0 {
	// 	for i, i2 := range s.CodecData {
	// 		if i2.Type().IsVideo() {
	// 			s.CodecData[i] = codecData
	// 		}
	// 	}
	// } else {
	// 	s.CodecData = append(s.CodecData, codecData)
	// }
	Config.coAd(s.name, s.CodecData)
}

//binSize
func binSize(data []byte) []byte {
	buf := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(buf, uint32(len(data)))
	return append(buf, data...)
}

func (s *RTSPStream) onAudioPacket(player *rtsp.Player, p *rtp.Packet) error {

	err := s.setupCodec(player)
	if err != nil {
		return err
	}
	if s.codecAudio == nil {
		return nil
	}
	if s.PreAudioTS == 0 {
		s.PreAudioTS = int64(p.Timestamp)
	}
	nalRaw, _ := h264parser.SplitNALUs(p.Payload)
	var retmap []*av.Packet
	for _, nal := range nalRaw {
		var duration time.Duration
		switch s.audioCodec {
		case av.PCM_MULAW:
			duration = time.Duration(len(nal)) * time.Second / time.Duration(s.AudioTimeScale)
			s.AudioTimeLine += duration
			data := make([]byte, len(nal))
			copy(data, nal)
			retmap = append(retmap, &av.Packet{
				Data:            data,
				CompositionTime: time.Duration(1) * time.Millisecond,
				Duration:        duration,
				Idx:             s.audioIDX,
				IsKeyFrame:      false,
				Time:            s.AudioTimeLine,
			})
		case av.PCM_ALAW:
			duration = time.Duration(len(nal)) * time.Second / time.Duration(s.AudioTimeScale)
			s.AudioTimeLine += duration
			data := make([]byte, len(nal))
			copy(data, nal)
			retmap = append(retmap, &av.Packet{
				Data:            data,
				CompositionTime: time.Duration(1) * time.Millisecond,
				Duration:        duration,
				Idx:             s.audioIDX,
				IsKeyFrame:      false,
				Time:            s.AudioTimeLine,
			})
		case av.OPUS:
			duration = time.Duration(20) * time.Millisecond
			s.AudioTimeLine += duration
			data := make([]byte, len(nal))
			copy(data, nal)
			retmap = append(retmap, &av.Packet{
				Data:            data,
				CompositionTime: time.Duration(1) * time.Millisecond,
				Duration:        duration,
				Idx:             s.audioIDX,
				IsKeyFrame:      false,
				Time:            s.AudioTimeLine,
			})
		case av.AAC:
			auHeadersLength := uint16(0) | (uint16(nal[0]) << 8) | uint16(nal[1])
			auHeadersCount := auHeadersLength >> 4
			framesPayloadOffset := 2 + int(auHeadersCount)<<1
			auHeaders := nal[2:framesPayloadOffset]
			framesPayload := nal[framesPayloadOffset:]
			for i := 0; i < int(auHeadersCount); i++ {
				auHeader := uint16(0) | (uint16(auHeaders[0]) << 8) | uint16(auHeaders[1])
				frameSize := auHeader >> 3
				frame := framesPayload[:frameSize]
				auHeaders = auHeaders[2:]
				framesPayload = framesPayload[frameSize:]
				if _, _, _, _, err := aacparser.ParseADTSHeader(frame); err == nil {
					frame = frame[7:]
				}
				duration = time.Duration((float32(1024)/float32(s.AudioTimeScale))*1000*1000*1000) * time.Nanosecond
				//duration = time.Duration((float32(1024)/float32(s.AudioTimeScale))*1000) * time.Millisecond
				s.AudioTimeLine += duration
				data := make([]byte, len(frame))
				copy(data, frame)
				retmap = append(retmap, &av.Packet{
					Data:            data,
					CompositionTime: time.Duration(1) * time.Millisecond,
					Duration:        duration,
					Idx:             s.audioIDX,
					IsKeyFrame:      false,
					Time:            s.AudioTimeLine,
				})
			}
		}
	}
	if len(retmap) > 0 {
		s.PreAudioTS = int64(p.Timestamp)
		// return retmap, true
		if s.AudioOnly {
			if !s.keyTest.Stop() {
				<-s.keyTest.C
			}
			s.keyTest.Reset(20 * time.Second)
		}
	}
	for _, p := range retmap {
		Config.cast(s.name, *p)
	}
	return nil
}
