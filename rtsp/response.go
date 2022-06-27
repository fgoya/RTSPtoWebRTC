package rtsp

import (
	"bufio"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type Response struct {
	Status     string // e.g. "200 OK"
	StatusCode int    // e.g. 200
	Proto      string // e.g. "RTSP/1.0"
	ProtoMajor int    // e.g. 1
	ProtoMinor int    // e.g. 0

	// Header maps header keys to values. If the response had multiple
	// headers with the same key, they may be concatenated, with comma
	// delimiters.  (RFC 7230, section 3.2.2 requires that multiple headers
	// be semantically equivalent to a comma-delimited sequence.) When
	// Header values are duplicated by other fields in this struct (e.g.,
	// ContentLength, TransferEncoding, Trailer), the field values are
	// authoritative.
	//
	// Keys in the map are canonicalized (see CanonicalHeaderKey).
	Header http.Header

	reader *bufio.Reader
}

func (r *Response) Close() error {
	var err error
	var n int
	if v := r.Header.Get("Content-Length"); v != "" {
		n, err = strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return fmt.Errorf("not valid Content-Length: %s", v)
		}
	} else {
		return nil
	}
	_, err = r.reader.Discard(n)
	if err != nil {
		return err
	}
	return nil
}

func (r *Response) FullString() (string, error) {
	var err error
	var n int
	if v := r.Header.Get("Content-Length"); v != "" {
		n, err = strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return "", fmt.Errorf("not valid Content-Length: %s", v)
		}
	} else {
		return "", nil
	}
	// Using bufio.Reader internal buffer
	if n > r.reader.Size() {
		return "", bufio.ErrBufferFull
	}
	for {
		b, err := r.reader.Peek(n)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		r.reader.Discard(n)
		return string(b), nil
	}
}

func badStringError(what, val string) error { return fmt.Errorf("%s %q", what, val) }

// ParseRTSPVersion parses an RTSP version string according to RFC 7230, section 2.6.
// "RTSP/1.0" returns (1, 0, true). Note that strings without
// a minor version, such as "RTSP/2", are not valid.
func ParseRTSPVersion(vers string) (major, minor int, ok bool) {
	switch vers {
	case "RTSP/1.1":
		return 1, 1, true
	case "RTSP/1.0":
		return 1, 0, true
	}
	if !strings.HasPrefix(vers, "RTSP/") {
		return 0, 0, false
	}
	if len(vers) != len("RTSP/X.Y") {
		return 0, 0, false
	}
	if vers[6] != '.' {
		return 0, 0, false
	}
	maj, err := strconv.ParseUint(vers[5:6], 10, 0)
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.ParseUint(vers[7:8], 10, 0)
	if err != nil {
		return 0, 0, false
	}
	return int(maj), int(min), true
}
