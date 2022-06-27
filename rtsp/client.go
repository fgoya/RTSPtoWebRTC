package rtsp

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A Client represents a textual network protocol connection.
// It consists of a Reader and Writer to manage I/O
// and a Pipeline to sequence concurrent requests on the connection.
// These embedded types carry methods with them;
// see the documentation of those types for details.
type Client struct {
	r    *bufio.Reader
	w    *bufio.Writer
	conn net.Conn
	seq  int
	// Header        http.Header
	username string
	password string
	// base          string
	authorization func(method string, uri string) string
	URL           url.URL
	UserAgent     string
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
}

// Dial returns a new Client connected to an SMTP server at addr.
// The addr must include a port, as in "mail.example.com:smtp".
func Open(uri string, timeout time.Duration, config *tls.Config) (*Client, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	username := u.User.Username()
	password, _ := u.User.Password()
	u.User = nil
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Host, "554")
	}
	if u.Scheme != "rtsp" && u.Scheme != "rtsps" {
		u.Scheme = "rtsp"
	}

	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "rtsps" {
		if config == nil {
			config = &tls.Config{}
		}
		if config.ServerName == "" {
			config.ServerName = u.Hostname()
		}
		tlsConn := tls.Client(conn, config)
		err = tlsConn.Handshake()
		if err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	c := NewClient(conn, username, password)
	c.URL = *u
	return c, nil
}

// NewClient returns a new Client using an existing connection and host as a
// server name to be used when authenticating.
func NewClient(conn net.Conn, username, password string) *Client {
	return &Client{
		r:        bufio.NewReaderSize(conn, 4096), // TODO: allow to specify buffer size
		w:        bufio.NewWriterSize(conn, 4096),
		conn:     conn,
		username: username,
		password: password,
	}
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) RoundTrip(uri, method string, header http.Header) (*Response, error) {

	got401 := false
RETRY:
	seq, err := c.Request(uri, method, header)
	if err != nil {
		return nil, err
	}

	x, err := c.Receive()
	if err != nil {
		return nil, err
	}
	resp, ok := x.(*Response)
	if !ok {
		if err := x.Close(); err != nil {
			log.Printf("failed close %#v: %s", x, err)
		}
		return nil, fmt.Errorf("server sent mismatch data while waiting response data: %#v", x)
	}

	v, err := strconv.Atoi(resp.Header.Get("CSeq"))
	if err != nil {
		return nil, fmt.Errorf("invalid response CSeq")
	}
	if v != seq {
		return nil, fmt.Errorf("mismatch response CSeq, got %d expecting %d", v, seq)
	}

	switch resp.StatusCode {
	case 401:
		if err := resp.Close(); err != nil {
			return nil, fmt.Errorf("failed close response body. %v", err)
		}
		if got401 {
			return nil, fmt.Errorf("RTSP Client Unauthorized 401")
		}
		v := resp.Header.Get("WWW-Authenticate")
		if v == "" {
			return nil, fmt.Errorf("missing WWW-Authenticate")
		}
		if v, ok := prefixEqualFold(v, "Digest "); ok {
			f := parseHeaderFields(v)
			c.authorization = generateDigestFunc(c.username, c.password, f["realm"], f["nonce"], f["qop"])
		} else if _, ok := prefixEqualFold(v, "Basic "); ok {
			authorization := fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", c.username, c.password))))
			c.authorization = func(string, string) string {
				return authorization
			}
		} else {
			return nil, fmt.Errorf("unknown www-authenticate: %s", v)
		}
		got401 = true
		goto RETRY
	}

	return resp, nil
}

func generateDigestFunc(username, password, realm, nonce, qop string) func(string, string) string {
	return func(method string, uri string) string {
		ha1 := md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", username, realm, password)))
		ha2 := md5.Sum([]byte(fmt.Sprintf("%s:%s", method, uri)))
		response := md5.Sum([]byte(fmt.Sprintf("%x:%s:%x", ha1, nonce, ha2)))
		return fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%x\"", username, realm, nonce, uri, response)
	}
}

func (c *Client) Request(uri, method string, headers http.Header) (seq int, err error) {

	seq = -1
	if c.WriteTimeout > 0 {
		err = c.conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
		if err != nil {
			return seq, err
		}
	}

	_, err = fmt.Fprintf(c.w, "%s %s RTSP/1.0\r\n", method, uri)
	if err != nil {
		return seq, err
	}

	c.seq++
	seq = c.seq
	// Header lines
	_, err = fmt.Fprintf(c.w, "CSeq: %d\r\n", seq)
	if err != nil {
		return seq, err
	}

	if c.authorization != nil {
		_, err = fmt.Fprintf(c.w, "Authorization: %s\r\n", c.authorization(method, uri))
		if err != nil {
			return seq, err
		}
	}

	if c.UserAgent != "" {
		_, err = fmt.Fprintf(c.w, "User-Agent: %s\r\n", c.UserAgent)
		if err != nil {
			return seq, err
		}
	}

	err = headers.Write(c.w)
	if err != nil {
		return seq, err
	}

	_, err = io.WriteString(c.w, "\r\n")
	if err != nil {
		return seq, err
	}

	err = c.w.Flush()
	if err != nil {
		return seq, err
	}

	return seq, nil
}

func (c *Client) Receive() (io.Closer, error) {
	var err error

	if c.ReadTimeout > 0 {
		err = c.conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))
		if err != nil {
			return nil, err
		}
	}

	b, err := c.r.Peek(1)
	if err != nil {
		return nil, err
	}

	if b[0] == '$' {
		var h [4]byte
		_, err = io.ReadFull(c.r, h[:])
		if err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(h[2:]))
		return &StreamData{
			Channel: int(h[1]),
			reader:  c.r,
			length:  n,
			r:       0,
		}, nil
	}

	// This is not enough, but reduce false positives
	if 'A' > b[0] || b[0] > 'Z' {
		return nil, fmt.Errorf("desync rtsp.Client read")
	}

	tp := textproto.NewReader(c.r)

	// Parse the first line of the response.
	line, err := tp.ReadLine()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	proto, status, ok := strings.Cut(line, " ")
	if !ok {
		return nil, badStringError("malformed RTSP message", line)
	}

	if major, minor, ok := ParseRTSPVersion(proto); ok {

		resp := &Response{reader: c.r}
		resp.Proto = proto
		resp.ProtoMajor = major
		resp.ProtoMinor = minor
		resp.Status = strings.TrimLeft(status, " ")

		statusCode, _, _ := strings.Cut(resp.Status, " ")
		if len(statusCode) != 3 {
			return nil, badStringError("malformed RTSP status code", statusCode)
		}
		resp.StatusCode, err = strconv.Atoi(statusCode)
		if err != nil || resp.StatusCode < 0 {
			return nil, badStringError("malformed RTSP status code", statusCode)
		}

		// Parse the response headers.
		mimeHeader, err := tp.ReadMIMEHeader()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		resp.Header = http.Header(mimeHeader)

		// fixPragmaCacheControl(resp.Header)

		// err = readTransfer(resp, r)
		// if err != nil {
		// 	return nil, err
		// }
		return resp, nil

	}

	req := &Request{reader: c.r}
	req.Method = proto
	uri, proto, ok := strings.Cut(status, " ")
	if !ok {
		return nil, badStringError("malformed RTSP response/request", line)
	}
	if req.ProtoMajor, req.ProtoMinor, ok = ParseRTSPVersion(proto); !ok {
		return nil, badStringError("malformed RTSP version", proto)
	}
	req.Proto = proto
	req.URL, err = url.Parse(uri)
	if err != nil {
		return nil, badStringError("malformed RTSP uri", err.Error())
	}

	// Parse the request headers.
	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	req.Header = http.Header(mimeHeader)

	// fixPragmaCacheControl(resp.Header)

	// err = readTransfer(resp, r)
	// if err != nil {
	// 	return nil, err
	// }
	return req, nil
}

func prefixEqualFold(v, prefix string) (r string, ok bool) {
	if strings.EqualFold(v[:len(prefix)], prefix) {
		return v[len(prefix):], true
	}
	return v, false
}

func parseHeaderFields(value string) map[string]string {
	values := make(map[string]string)

NEXT:
	for value != "" {
		var i int
		var key string
		for i < len(value) {
			switch value[i] {
			case '=':
				key = textproto.TrimString(value[:i])
				value = value[i+1:]
				i = 0
			case ',':
				key = textproto.TrimString(value[:i])
				if key != "" {
					values[key] = ""
				}
				value = value[i+1:]
				continue NEXT
			default:
				i++
				continue
			}
			break
		}
		// EOF: missing '='
		if i > 0 {
			key = textproto.TrimString(value)
			if key != "" {
				values[key] = ""
			}
			value = ""
			continue NEXT
		}
		for ; i < len(value); i++ {
			switch value[i] {
			case '"':
				value = value[i+1:]
				i = 0
				for ; i < len(value); i++ {
					switch value[i] {
					case '"':
						if key != "" {
							values[key] = value[:i]
						}
						value = value[i+1:]
						// might be necessary to deal with content until
						// next comma or EOF, for now we consider empty
						// key and ignore
						continue NEXT
					}
				}
				// EOF: missing final '"'
				if key != "" {
					values[key] = value
				}
				value = ""
				continue NEXT
			case ',':
				if key != "" {
					values[key] = textproto.TrimString(value[:i])
				}
				value = value[i+1:]
				continue NEXT
			}
		}
		if key != "" {
			values[key] = textproto.TrimString(value)
		}
		value = ""
	}
	return values
}
