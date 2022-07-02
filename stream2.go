package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/deepch/RTSPtoWebRTC/nal"
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

	// fuStarted       bool
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

	if len(s.CodecData) > 0 {
		// My camera incorrect report SPS and PPS in SDP
		Config.coNilAd(s.name, s.CodecData)
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

	// This don't make too much sense to me. I believe since it is RTP packets it
	// shouldn't use Byte Stream, maybe some bad cameras do it?
	// nalRaw, _ := h264parser.SplitNALUs(p.Payload)
	nalus := []nal.Unit{p.Payload}
	if len(nalus) == 0 || len(nalus[0]) == 0 {
		// return nil, false
		return fmt.Errorf("len(nalus) == 0 || len(nalus[0]) == 0")
	}
	if t := nalus[0].Type(); t == 7 || t == 9 {
		nalus, _ = nal.AnnexBSplit(nalus[0])
	}
	var retmap []*av.Packet
	for _, nalu := range nalus {
		if s.videoCodec == av.H265 {
			naluType := (nalu[0] >> 1) & 0x3f
			switch naluType {
			case h265parser.NAL_UNIT_CODED_SLICE_TRAIL_R:
				retmap = append(retmap, &av.Packet{
					Data:            binSize(nalu),
					CompositionTime: time.Duration(1) * time.Millisecond,
					Idx:             s.videoIDX,
					IsKeyFrame:      false,
					Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
					Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				})
			case h265parser.NAL_UNIT_VPS:
				s.CodecUpdateVPS(nalu)
			case h265parser.NAL_UNIT_SPS:
				s.CodecUpdateSPS(nalu)
			case h265parser.NAL_UNIT_PPS:
				s.CodecUpdatePPS(nalu)
			case h265parser.NAL_UNIT_UNSPECIFIED_49:
				se := nalu[2] >> 6
				naluType := nalu[2] & 0x3f
				if se == 2 {
					s.BufferRtpPacket.Truncate(0)
					s.BufferRtpPacket.Reset()
					s.BufferRtpPacket.Write([]byte{(nalu[0] & 0x81) | (naluType << 1), nalu[1]})
					r := make([]byte, 2)
					r[1] = nalu[1]
					r[0] = (nalu[0] & 0x81) | (naluType << 1)
					s.BufferRtpPacket.Write(nalu[3:])
				} else if se == 1 {
					s.BufferRtpPacket.Write(nalu[3:])
					retmap = append(retmap, &av.Packet{
						Data:            binSize(s.BufferRtpPacket.Bytes()),
						CompositionTime: time.Duration(1) * time.Millisecond,
						Idx:             s.videoIDX,
						IsKeyFrame:      naluType == h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL,
						Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
						Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
					})
				} else {
					s.BufferRtpPacket.Write(nalu[3:])
				}
			default:
				log.Println("Unsupported Nal", naluType)
			}

		} else if s.videoCodec == av.H264 {
			switch nalu.Type() { // unsigned integer using 5 bits
			case 1, 2, 3, 4: // VCL
				retmap = append(retmap, &av.Packet{
					Data:            binSize(nalu),
					CompositionTime: time.Duration(1) * time.Millisecond,
					Idx:             s.videoIDX,
					IsKeyFrame:      false,
					Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
					Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				})
			case 5:
				retmap = append(retmap, &av.Packet{
					Data:            binSize(nalu),
					CompositionTime: time.Duration(1) * time.Millisecond,
					Idx:             s.videoIDX,
					IsKeyFrame:      true,
					Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
					Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				})
			case 7: // Sequence parameter set
				s.CodecUpdateSPS(nalu)
			case 8: // Picture parameter set
				s.CodecUpdatePPS(nalu)
			case 24: // RFC6184: STAP-A Single-time aggregation packet
				b := nalu.Payload()
				for len(b) >= 2 {
					size := int(b[1]) | int(b[0])<<8
					if size == 0 || len(b) < 2+size {
						log.Println("incorrect packet size in nal_unit_type 24")
						break
					}
					nalu := nal.Unit(b[2 : size+2])
					switch nalu.Type() {
					case 1, 2, 3, 4:
						retmap = append(retmap, &av.Packet{
							Data:            binSize(nalu),
							CompositionTime: time.Duration(1) * time.Millisecond,
							Idx:             s.videoIDX,
							IsKeyFrame:      false,
							Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
							Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
						})
					case 5:
						retmap = append(retmap, &av.Packet{
							Data:            binSize(nalu),
							CompositionTime: time.Duration(1) * time.Millisecond,
							Idx:             s.videoIDX,
							IsKeyFrame:      true,
							Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
							Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
						})
					case 7:
						s.CodecUpdateSPS(nalu)
					case 8:
						s.CodecUpdatePPS(nalu)
					default:
						log.Println("24: Unsupported NAL Type", nalu.Type())
					}
					b = b[2+size:]
				}
			case 28: // RFC6184: FU-A Fragmentation unit
				// nalu[0] is the fragmentation unit indicator
				// nalu[1] is the fragmentation unit header
				// first bit of the header is start
				// second bit of the header is end
				// third bit of the header is reserved
				// remaining five bits is the actual nal_unit_type
				start := nalu[1]&0x80 != 0
				end := nalu[1]&0x40 != 0
				if start {
					s.BufferRtpPacket.Reset()
					s.BufferRtpPacket.WriteByte((nalu[0] & 0xE0) | (nalu[1] & 0x1F)) // replace 5 bit nal_unit_type
				}
				if s.BufferRtpPacket.Len() > 0 {
					s.BufferRtpPacket.Write(nalu[2:])
					if !end {
						break
					}
					nalus := []nal.Unit{s.BufferRtpPacket.Bytes()}
					s.BufferRtpPacket.Reset()
					// RFC6184 seems to not allow Annex B, but my camera work like this
					if t := nalus[0].Type(); t == 7 || t == 9 {
						nalus, _ = nal.AnnexBSplit(nalus[0])
					}
					for _, nalu := range nalus {
						switch nalu.Type() {
						case 1, 2, 3, 4:
							retmap = append(retmap, &av.Packet{
								Data:            binSize(nalu),
								CompositionTime: time.Duration(1) * time.Millisecond,
								Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
								Idx:             s.videoIDX,
								IsKeyFrame:      false,
								Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
							})
						case 5:
							retmap = append(retmap, &av.Packet{
								Data:            binSize(nalu),
								CompositionTime: time.Duration(1) * time.Millisecond,
								Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
								Idx:             s.videoIDX,
								IsKeyFrame:      true,
								Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
							})
						case 7: // Sequence parameter set
							s.CodecUpdateSPS(nalu)
						case 8: // Picture parameter set
							s.CodecUpdatePPS(nalu)
						case 12: // I got a few of these, not sure what to do yet
						default:
							log.Println("28: Unsupported NAL Type", nalu.Type())
						}
					}
				}

				// fuIndicator := p.Payload[0]
				// fuHeader := p.Payload[1]
				// isStart := fuHeader&0x80 != 0
				// isEnd := fuHeader&0x40 != 0
				// if isStart {
				// 	s.fuStarted = true
				// 	// s.BufferRtpPacket.Truncate(0)
				// 	s.BufferRtpPacket.Reset()
				// 	s.BufferRtpPacket.Write([]byte{fuIndicator&0xe0 | fuHeader&0x1f})
				// }
				// if s.fuStarted {
				// 	s.BufferRtpPacket.Write(p.Payload[2:])
				// 	if isEnd {
				// 		s.fuStarted = false
				// 		payload := s.BufferRtpPacket.Bytes()
				// 		naluTypef := payload[0] & 0x1f
				// 		if naluTypef == 7 || naluTypef == 9 {
				// 			log.Println("naluTypef == 7 || naluTypef == 9")
				// 			// bufered, _ := h264parser.SplitNALUs(append([]byte{0, 0, 0, 1}, s.BufferRtpPacket.Bytes()...))
				// 			bufered := nal2.AnnexBSplit(payload)
				// 			for _, v := range bufered {
				// 				naluTypefs := v[0] & 0x1f
				// 				switch {
				// 				case naluTypefs == 5:
				// 					// log.Println("naluTypefs == 5")
				// 					s.BufferRtpPacket.Reset()
				// 					s.BufferRtpPacket.Write(v)
				// 					naluTypef = 5
				// 				case naluTypefs == 7:
				// 					// log.Println("naluTypefs == 7")
				// 					// log.Println("SPS2", base64.StdEncoding.EncodeToString(v), v)
				// 					s.CodecUpdateSPS(v)
				// 				case naluTypefs == 8:
				// 					// log.Println("naluTypefs == 8")
				// 					// log.Println("PPS2", base64.StdEncoding.EncodeToString(v), v)
				// 					s.CodecUpdatePPS(v)
				// 				default:
				// 					log.Println("28: Unsupported NAL Type", naluTypefs, len(bufered), naluTypef)
				// 				}
				// 			}
				// 		}
				// 		retmap = append(retmap, &av.Packet{
				// 			Data:            binSize(s.BufferRtpPacket.Bytes()),
				// 			CompositionTime: time.Duration(1) * time.Millisecond,
				// 			Duration:        time.Duration(float32(int64(p.Timestamp)-s.PreVideoTS)/90) * time.Millisecond,
				// 			Idx:             s.videoIDX,
				// 			IsKeyFrame:      naluTypef == 5,
				// 			Time:            time.Duration(p.Timestamp/90) * time.Millisecond,
				// 		})
				// 	}
				// }
			default:
				log.Println("Unsupported NAL Type", nalu.Type())
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
	nalus, _ := h264parser.SplitNALUs(p.Payload)
	var retmap []*av.Packet
	for _, nalu := range nalus {
		var duration time.Duration
		switch s.audioCodec {
		case av.PCM_MULAW:
			duration = time.Duration(len(nalu)) * time.Second / time.Duration(s.AudioTimeScale)
			s.AudioTimeLine += duration
			data := make([]byte, len(nalu))
			copy(data, nalu)
			retmap = append(retmap, &av.Packet{
				Data:            data,
				CompositionTime: time.Duration(1) * time.Millisecond,
				Duration:        duration,
				Idx:             s.audioIDX,
				IsKeyFrame:      false,
				Time:            s.AudioTimeLine,
			})
		case av.PCM_ALAW:
			duration = time.Duration(len(nalu)) * time.Second / time.Duration(s.AudioTimeScale)
			s.AudioTimeLine += duration
			data := make([]byte, len(nalu))
			copy(data, nalu)
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
			data := make([]byte, len(nalu))
			copy(data, nalu)
			retmap = append(retmap, &av.Packet{
				Data:            data,
				CompositionTime: time.Duration(1) * time.Millisecond,
				Duration:        duration,
				Idx:             s.audioIDX,
				IsKeyFrame:      false,
				Time:            s.AudioTimeLine,
			})
		case av.AAC:
			auHeadersLength := uint16(0) | (uint16(nalu[0]) << 8) | uint16(nalu[1])
			auHeadersCount := auHeadersLength >> 4
			framesPayloadOffset := 2 + int(auHeadersCount)<<1
			auHeaders := nalu[2:framesPayloadOffset]
			framesPayload := nalu[framesPayloadOffset:]
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
