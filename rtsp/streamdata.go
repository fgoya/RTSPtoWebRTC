package rtsp

import (
	"bufio"
	"fmt"

	"github.com/pion/rtp"
)

type StreamData struct {
	Channel int

	reader    *bufio.Reader
	length, r int // read bytes
}

// ReadRTP read the payload as a RTP packet
// the packet use the reader internal buffer and becomes
// invalid in the next read operation
func (r *StreamData) RTPRead(p *rtp.Packet) error {
	if r.r > 0 {
		return fmt.Errorf("message content has already been read")
	}

	var err error
	size := r.length
	if size > 65535 || size < 4 {
		return fmt.Errorf("incorrect RTP packet size %d", size)
	}
	// Using bufio.Reader internal buffer
	if size > r.reader.Size() {
		return bufio.ErrBufferFull
	}
	var b []byte
	for {
		b, err = r.reader.Peek(size)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return err
		}
		break
	}

	r.reader.Discard(size)
	r.r = size
	err = p.Unmarshal(b[:len(b):len(b)])
	if err != nil {
		return err
	}

	return nil
}

func (r *StreamData) Close() error {
	var err error

	size := r.length - r.r
	if size <= 0 {
		return nil
	}

	size, err = r.reader.Discard(size)
	r.r += size
	if err != nil {
		return err
	}
	return nil
}
