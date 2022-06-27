package main

import (
	"bytes"
	"errors"
	"log"
	"time"

	"github.com/deepch/RTSPtoWebRTC/rtsp"
)

var (
	ErrorStreamExitNoVideoOnStream = errors.New("Stream Exit No Video On Stream")
	ErrorStreamExitRtspDisconnect  = errors.New("Stream Exit Rtsp Disconnect")
	ErrorStreamExitNoViewer        = errors.New("Stream Exit On Demand No Viewer")
)

func serveStreams() {
	for k, v := range Config.Streams {
		if !v.OnDemand {
			go RTSPWorkerLoop(k, v.URL, v.OnDemand, v.DisableAudio, v.Debug)
		}
	}
}
func RTSPWorkerLoop(name, url string, OnDemand, DisableAudio, Debug bool) {
	defer Config.RunUnlock(name)
	for {
		log.Println("Stream Try Connect", name)
		err := RTSPWorker(name, url, OnDemand, DisableAudio, Debug)
		if err != nil {
			log.Println(err)
			Config.LastError = err
		}
		if OnDemand && !Config.HasViewer(name) {
			log.Println(ErrorStreamExitNoViewer)
			return
		}
		time.Sleep(1 * time.Second)
	}
}

func RTSPWorker(name, url string, OnDemand, DisableAudio, Debug bool) error {
	s := RTSPStream{
		name:            name,
		BufferRtpPacket: bytes.NewBuffer([]byte{}),
		videoIDX:        -1,
		audioIDX:        -2,
		AudioTimeScale:  8000,
	}

	c, err := rtsp.Open(url, 6*time.Second, nil)
	if err != nil {
		return err
	}
	// Increased timeout, since my camera seems
	// to be too cheap
	c.ReadTimeout = 6 * time.Second
	c.WriteTimeout = 3 * time.Second

	stop := make(chan struct{})
	done := make(chan struct{})
	errC := make(chan error, 2)

	s.keyTest = time.NewTimer(20 * time.Second)
	clientTest := time.NewTimer(20 * time.Second)
	if !OnDemand {
		if !clientTest.Stop() {
			<-clientTest.C
		}
	}
	go func() {
		for {
			select {
			case <-s.keyTest.C:
				log.Print("Enter Keyframe timeout")
				errC <- ErrorStreamExitNoVideoOnStream
				close(stop)
				return
			case <-clientTest.C:
				if !Config.HasViewer(name) {
					errC <- ErrorStreamExitNoViewer
					close(stop)
					return
				}
				clientTest.Reset(20 * time.Second)
			case <-done:
				return
			}
		}
	}()

	p := rtsp.Player{
		DisableAudio:  DisableAudio,
		OnVideoPacket: s.onVideoPacket,
		OnAudioPacket: s.onAudioPacket,
	}

	errC <- p.Run(c, stop)

	s.keyTest.Stop()
	clientTest.Stop()
	close(done)
	err = c.Close()
	if err != nil {
		log.Print("closing rtsp client:", err)
	}

	err = <-errC
	// For debug purposes, we can log the second error
	// select {
	// case err := <-errC:
	// 	if err != nil {
	// 		log.Printf("%#v", err)
	// 	}
	// default:
	// }
	if err != nil {
		return err
	}

	return nil
}

// func RTSPWorker(name, url string, OnDemand, DisableAudio, Debug bool) error {
// 	keyTest := time.NewTimer(20 * time.Second)
// 	clientTest := time.NewTimer(20 * time.Second)
// 	//add next TimeOut
// 	RTSPClient, err := rtspv2.Dial(rtspv2.RTSPClientOptions{URL: url, DisableAudio: DisableAudio, DialTimeout: 3 * time.Second, ReadWriteTimeout: 3 * time.Second, Debug: Debug})
// 	if err != nil {
// 		return err
// 	}
// 	defer RTSPClient.Close()
// 	if RTSPClient.CodecData != nil {
// 		Config.coAd(name, RTSPClient.CodecData)
// 	}
// 	var AudioOnly bool
// 	if len(RTSPClient.CodecData) == 1 && RTSPClient.CodecData[0].Type().IsAudio() {
// 		AudioOnly = true
// 	}
// 	first := true
// 	for {
// 		select {
// 		case <-clientTest.C:
// 			if OnDemand {
// 				if !Config.HasViewer(name) {
// 					return ErrorStreamExitNoViewer
// 				} else {
// 					clientTest.Reset(20 * time.Second)
// 				}
// 			}
// 		case <-keyTest.C:
// 			return ErrorStreamExitNoVideoOnStream
// 		case signals := <-RTSPClient.Signals:
// 			switch signals {
// 			case rtspv2.SignalCodecUpdate:
// 				Config.coAd(name, RTSPClient.CodecData)
// 			case rtspv2.SignalStreamRTPStop:
// 				return ErrorStreamExitRtspDisconnect
// 			}
// 		case packetAV := <-RTSPClient.OutgoingPacketQueue:
// 			if first {
// 				log.Println("First Packet:", packetAV.IsKeyFrame)
// 				first = false
// 			}
// 			if AudioOnly || packetAV.IsKeyFrame {
// 				keyTest.Reset(20 * time.Second)
// 			}
// 			Config.cast(name, *packetAV)
// 		}
// 	}
// }
