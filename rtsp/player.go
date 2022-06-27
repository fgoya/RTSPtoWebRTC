package rtsp

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deepch/vdk/format/rtsp/sdp"
	"github.com/pion/rtp"
)

// const defaultUserAgent = "go-rtsp-client/1.0"
const defaultUserAgent = "Lavf58.76.100"

type Player struct {
	DisableAudio  bool
	VideoMedia    *sdp.Media
	AudioMedia    *sdp.Media
	OnVideoPacket func(*Player, *rtp.Packet) error
	OnAudioPacket func(*Player, *rtp.Packet) error

	base    string
	session string

	media        []sdp.Media
	videoID      int
	audioID      int
	startVideoTS int64
	startAudioTS int64
}

func (s *Player) Run(c *Client, stop chan struct{}) error {

	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}

	s.videoID = -1
	s.audioID = -2

	s.base = c.URL.String()

	// Public: OPTIONS, DESCRIBE, SETUP, TEARDOWN, PLAY, PAUSE, GET_PARAMETER, SET_PARAMETER,USER_CMD_SET
	_, err := s.options(c)
	if err != nil {
		return err
	}

	select {
	case <-stop:
		return nil
	default:
	}

	s.media, err = s.describe(c)
	if err != nil {
		return err
	}

	select {
	case <-stop:
		return nil
	default:
	}

	var ch int
	for i := range s.media {
		m := &s.media[i]
		switch m.AVType {
		case "video":
			// Session: 4b7fbfdc;timeout=60
			// Transport: RTP/AVP;unicast;destination=XXX.XXX.XXX.XXX;source=XXX.XXX.XXX.XXX;interleaved=0-1
			tp, err := s.setup(c, m.Control, fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", ch, ch+1))
			if err != nil {
				return err
			}

			select {
			case <-stop:
				return s.teardown(c)
			default:
			}

			if lo, _, ok := parseInterleaved(tp); ok {
				ch = lo
			}
			s.VideoMedia = m
			s.videoID = ch
			ch += 2

		case "audio":
			if s.DisableAudio {
				continue
			}
			tp, err := s.setup(c, m.Control, fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", ch, ch+1))
			if err != nil {
				return err
			}

			select {
			case <-stop:
				return s.teardown(c)
			default:
			}

			if lo, _, ok := parseInterleaved(tp); ok {
				ch = lo
			}
			s.AudioMedia = m
			s.audioID = ch
			ch += 2
		}
	}

	// Session: e2d8313;timeout=60
	// RTP-Info: url=rtsp:XXX.XXX.XXX.XXX:554/onvif2/track1;seq=25744;rtptime=11262089160
	play, err := s.play(c, "")
	if err != nil {
		return err
	}

	select {
	case <-stop:
		return s.teardown(c)
	default:
	}

	timer := time.Now()
	for {
		if time.Since(timer) > 25*time.Second {
			_, err = c.Request(s.base, "OPTIONS", http.Header{"Require": {"implicit-play"}})
			if err != nil {
				return fmt.Errorf("RTSP Client RTP keep-alive", err)
			}
			timer = time.Now()
		}

		x, err := c.Receive()
		if err != nil {
			return fmt.Errorf("fail receive: %s", err)
		}
		switch r := x.(type) {
		case *StreamData:
			switch r.Channel {
			case s.videoID:
				if s.OnVideoPacket != nil {
					var p rtp.Packet
					err = r.RTPRead(&p)
					if err != nil {
						return fmt.Errorf("read rtp packet failed: %v", err)
					}
					err = s.OnVideoPacket(s, &p)
					if err != nil {
						log.Println("onVideoPacket", err)
						return err
					}
				}
			case s.audioID:
				if s.OnAudioPacket != nil {
					var p rtp.Packet
					err = r.RTPRead(&p)
					if err != nil {
						return fmt.Errorf("read rtp packet failed: %v", err)
					}
					err = s.OnAudioPacket(s, &p)
					if err != nil {
						log.Println("onAudioPacket")
						return err
					}
				}
			default:
				//log.Println("Unsuported Intervaled data packet", int(content[1]), content[offset:end])
				// See RFC2326 section 10.12:
				// When the transport choice is RTP, RTCP messages are also interleaved
				// by the server over the TCP connection. As a default, RTCP packets are
				// sent on the first available channel higher than the RTP channel. The
				// client MAY explicitly request RTCP packets on another channel. This
				// is done by specifying two channels in the interleaved parameter of
				// the Transport header(Section 12.39).
			}
		case *Response:
			if play > 0 {
				seq, err := strconv.Atoi(r.Header.Get("CSeq"))
				if err != nil {
					return fmt.Errorf("server returned invalid response, Cseg: %s", r.Header.Get("CSeq"))
				}
				if seq == play {
					for _, v := range strings.Split(r.Header.Get("RTP-Info"), ",") {
						splits2 := strings.Split(v, ";")
						for _, vs := range splits2 {
							if strings.Contains(vs, "rtptime") {
								splits3 := strings.Split(vs, "=")
								if len(splits3) == 2 {
									if s.startVideoTS == 0 {
										ts, _ := strconv.Atoi(strings.TrimSpace(splits3[1]))
										s.startVideoTS = int64(ts)
									} else {
										ts, _ := strconv.Atoi(strings.TrimSpace(splits3[1]))
										s.startAudioTS = int64(ts)
									}
								}
							}
						}
					}
					play = -1
				}
			}
			// Ignoring ping response
		default:
			return fmt.Errorf("RTSP Client RTP Read DeSync. Maybe incorrect, see rtsp/player.go:213")
		}
		if err := x.Close(); err != nil {
			return fmt.Errorf("failed close received data. %s", err)
		}

		select {
		case <-stop:
			return s.teardown(c)
		default:
		}
	}

	// return nil
}

func (s *Player) options(c *Client) (public string, err error) {
	r, err := c.RoundTrip(s.base, "OPTIONS", nil)
	if err != nil {
		return "", err
	}

	if err := r.Close(); err != nil {
		return "", fmt.Errorf("failed close response body. %s", err)
	}

	if r.StatusCode != 200 {
		return "", fmt.Errorf("unexpected response code %d", r.StatusCode)
	}

	return r.Header.Get("Public"), nil
}

func (s *Player) describe(c *Client) ([]sdp.Media, error) {
	r, err := c.RoundTrip(s.base, "DESCRIBE", http.Header{"Accept": {"application/sdp"}})
	if err != nil {
		return nil, err
	}

	if r.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response code %d", r.StatusCode)
	}

	if v := r.Header.Get("Content-Base"); v != "" {
		s.base = v
	}
	if v := r.Header.Get("Content-Type"); v != "" && v != "application/sdp" {
		return nil, fmt.Errorf("unkwon Content-Type: %s", v)
	}

	body, err := r.FullString()
	if err != nil {
		return nil, err
	}
	// os.WriteFile("/tmp/sdp.txt", []byte(body), 0666)
	// var d pionsdp.SessionDescription
	// if err = d.Unmarshal(body); err != nil {
	// 	log.Println(err)
	// 	return
	// }
	// s.description = &d
	_, medias := sdp.Parse(body)
	return medias, nil
}

func (s *Player) setup(c *Client, control, transport string) (string, error) {
	h := make(http.Header)
	if s.session != "" {
		h.Set("Session", s.session)
	}
	h.Set("Transport", transport)

	r, err := c.RoundTrip(track(s.base, control), "SETUP", h)
	if err != nil {
		return "", err
	}

	if err := r.Close(); err != nil {
		return "", fmt.Errorf("failed close response body. %s", err)
	}

	if r.StatusCode != 200 {
		return "", fmt.Errorf("unexpected response code %d", r.StatusCode)
	}

	if v := r.Header.Get("Session"); v != "" {
		value, _, _ := strings.Cut(v, ";")
		s.session = strings.TrimSpace(value)
	}

	return r.Header.Get("Transport"), nil
}

func (s *Player) play(c *Client, rang string) (cseq int, err error) {

	h := make(http.Header)
	if rang != "" {
		h.Set("Range", rang)
	}
	h.Set("Session", s.session)

	return c.Request(s.base, "PLAY", h)
}

func (s *Player) teardown(c *Client) error {

	h := make(http.Header)
	h.Set("Session", s.session)

	seq, err := c.Request(s.base, "TEARDOWN", h)
	if err != nil {
		return err
	}

NEXT:
	x, err := c.Receive()
	if err != nil {
		// After TEARDOWN the behavior seems erratic, so just ignore
		// log.Println("after TEARDOWN the behavior is erratic ", err)
		return nil
	}
	resp, ok := x.(*Response)
	if !ok {
		if err := x.Close(); err != nil {
			return fmt.Errorf("failed close %v: %s", x, err)
		}
		goto NEXT
	}

	v, err := strconv.Atoi(resp.Header.Get("CSeq"))
	if err != nil {
		return fmt.Errorf("invalid response CSeq")
	}
	if v != seq {
		return fmt.Errorf("mismatch response CSeq, got %d expecting %d", v, seq)
	}

	if err := resp.Close(); err != nil {
		return fmt.Errorf("failed close response body. %s", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected response code %d", resp.StatusCode)
	}

	s.session = ""
	return nil
}

func track(base, control string) string {
	if control == "" {
		return base
	}
	if strings.Contains(control, "rtsp://") || strings.Contains(control, "rtsps://") {
		return control
	}
	if !strings.HasSuffix(base, "/") {
		return strings.Join([]string{base, control}, "/")
	}
	return base + control
}

func parseInterleaved(transport string) (lo int, hi int, ok bool) {
	for _, param := range strings.Split(transport, ";") {
		name, value, ok := strings.Cut(param, "=")
		if !ok || strings.TrimSpace(name) != "interleaved" {
			continue
		}
		v1, v2, ok := strings.Cut(value, "-")
		if !ok {
			return 0, 0, false
		}
		lo, err := strconv.Atoi(strings.TrimSpace(v1))
		if err != nil {
			return 0, 0, false
		}
		hi, err := strconv.Atoi(strings.TrimSpace(v2))
		if err != nil {
			return 0, 0, false
		}
		return lo, hi, true
	}
	return 0, 0, false
}
