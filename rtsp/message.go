package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	maxLineLen  = 16 << 10
	maxHeaders  = 128
	maxBodySize = 1 << 20
)

type headerField struct{ name, value string }

// message is an RTSP response or, when method is set, a server request.
type message struct {
	method string // request only
	uri    string // request only
	code   int    // response only
	reason string // response only
	header []headerField
	body   []byte
}

func (m *message) get(name string) string {
	for _, h := range m.header {
		if strings.EqualFold(h.name, name) {
			return h.value
		}
	}
	return ""
}

func (m *message) values(name string) []string {
	var out []string
	for _, h := range m.header {
		if strings.EqualFold(h.name, name) {
			out = append(out, h.value)
		}
	}
	return out
}

func readLine(br *bufio.Reader) (string, error) {
	b, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(b) > maxLineLen {
		return "", fmt.Errorf("%w: line too long", ErrProtocol)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// readMessage reads one RTSP message. Memory use is bounded by the bytes that
// actually arrive, whatever Content-Length claims.
func readMessage(br *bufio.Reader) (*message, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	m := &message{}
	if strings.HasPrefix(line, "RTSP/") {
		f := strings.SplitN(line, " ", 3)
		if len(f) < 2 {
			return nil, fmt.Errorf("%w: bad status line", ErrProtocol)
		}
		if m.code, err = strconv.Atoi(f[1]); err != nil || m.code < 100 || m.code > 999 {
			return nil, fmt.Errorf("%w: bad status code", ErrProtocol)
		}
		if len(f) == 3 {
			m.reason = f[2]
		}
	} else {
		f := strings.Split(line, " ")
		if len(f) != 3 || !strings.HasPrefix(f[2], "RTSP/") || f[0] == "" {
			return nil, fmt.Errorf("%w: bad start line", ErrProtocol)
		}
		m.method, m.uri = f[0], f[1]
	}
	for {
		line, err = readLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if len(m.header) >= maxHeaders {
			return nil, fmt.Errorf("%w: too many headers", ErrProtocol)
		}
		if (line[0] == ' ' || line[0] == '\t') && len(m.header) > 0 {
			last := &m.header[len(m.header)-1]
			if len(last.value)+len(line) > maxLineLen {
				return nil, fmt.Errorf("%w: header too long", ErrProtocol)
			}
			last.value += " " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m.header = append(m.header, headerField{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	if cl := m.get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 || n > maxBodySize {
			return nil, fmt.Errorf("%w: bad Content-Length", ErrProtocol)
		}
		if n > 0 {
			m.body, err = io.ReadAll(io.LimitReader(br, int64(n)))
			if err != nil {
				return nil, err
			}
			if len(m.body) != n {
				return nil, io.ErrUnexpectedEOF
			}
		}
	}
	return m, nil
}

// transportSpec is a parsed Transport header.
type transportSpec struct {
	tcp         bool
	multicast   bool
	interleaved [2]int
	clientPort  [2]int
	serverPort  [2]int
	source      string
	ssrc        uint32
	hasSSRC     bool
}

func parsePortPair(v string) ([2]int, bool) {
	a, b, two := strings.Cut(v, "-")
	x, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil || x < 0 || x > 65535 {
		return [2]int{}, false
	}
	y := x + 1
	if two {
		if y, err = strconv.Atoi(strings.TrimSpace(b)); err != nil || y < 0 || y > 65535 {
			return [2]int{}, false
		}
	}
	return [2]int{x, y}, true
}

func parseTransport(v string) (transportSpec, error) {
	t := transportSpec{interleaved: [2]int{-1, -1}}
	spec, _, _ := strings.Cut(v, ",")
	parts := strings.Split(spec, ";")
	proto := strings.ToUpper(strings.TrimSpace(parts[0]))
	if !strings.HasPrefix(proto, "RTP/AVP") {
		return t, fmt.Errorf("%w: unsupported transport", ErrProtocol)
	}
	t.tcp = strings.HasSuffix(proto, "/TCP")
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch strings.ToLower(k) {
		case "multicast":
			t.multicast = true
		case "interleaved":
			if pr, ok := parsePortPair(val); ok && pr[0] < 256 && pr[1] < 256 {
				t.interleaved = pr
			}
		case "client_port":
			t.clientPort, _ = parsePortPair(val)
		case "server_port":
			t.serverPort, _ = parsePortPair(val)
		case "source":
			t.source = val
		case "ssrc":
			if s, err := strconv.ParseUint(strings.TrimSpace(val), 16, 32); err == nil {
				t.ssrc, t.hasSSRC = uint32(s), true
			}
		}
	}
	return t, nil
}

type rtpInfo struct {
	url     string
	seq     uint16
	hasSeq  bool
	rtpTime uint32
	hasTime bool
}

func parseRTPInfo(v string) []rtpInfo {
	var out []rtpInfo
	for _, item := range strings.Split(v, ",") {
		var in rtpInfo
		found := false
		for _, p := range strings.Split(item, ";") {
			k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
			switch strings.ToLower(k) {
			case "url":
				in.url, found = val, true
			case "seq":
				if n, err := strconv.ParseUint(val, 10, 16); err == nil {
					in.seq, in.hasSeq, found = uint16(n), true, true
				}
			case "rtptime":
				if n, err := strconv.ParseUint(val, 10, 32); err == nil {
					in.rtpTime, in.hasTime, found = uint32(n), true, true
				}
			}
		}
		if found && len(out) < 64 {
			out = append(out, in)
		}
	}
	return out
}

// parseSessionHeader splits "id;timeout=60".
func parseSessionHeader(v string) (id string, timeout int) {
	parts := strings.Split(v, ";")
	id = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(k, "timeout") {
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 && n < 86400 {
				timeout = n
			}
		}
	}
	return id, timeout
}
