package rtsp

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	maxSDPMedia   = 32
	maxSDPFormats = 64
)

type sdpFormat struct {
	pt       uint8
	encoding string
	rate     int
	channels int
	fmtp     map[string]string
}

type sdpMedia struct {
	kind      string
	proto     string
	formats   []*sdpFormat
	control   string
	framerate float64
}

type sdpSession struct {
	name    string
	control string
	media   []*sdpMedia
}

func (m *sdpMedia) format(pt int) *sdpFormat {
	for _, f := range m.formats {
		if int(f.pt) == pt {
			return f
		}
	}
	return nil
}

// parseSDP reads a session description. It skips what it does not understand.
func parseSDP(b []byte) (*sdpSession, error) {
	if len(b) > maxBodySize {
		return nil, fmt.Errorf("%w: SDP too large", ErrProtocol)
	}
	s := &sdpSession{}
	var cur *sdpMedia
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r ")
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		val := line[2:]
		switch line[0] {
		case 's':
			if cur == nil {
				s.name = val
			}
		case 'm':
			f := strings.Fields(val)
			cur = nil
			if len(f) < 4 {
				continue
			}
			if len(s.media) >= maxSDPMedia {
				return nil, fmt.Errorf("%w: too many SDP media sections", ErrProtocol)
			}
			cur = &sdpMedia{kind: strings.ToLower(f[0]), proto: strings.ToUpper(f[2])}
			for _, pt := range f[3:] {
				n, err := strconv.Atoi(pt)
				if err != nil || n < 0 || n > 127 || len(cur.formats) >= maxSDPFormats {
					continue
				}
				cur.formats = append(cur.formats, &sdpFormat{pt: uint8(n)})
			}
			s.media = append(s.media, cur)
		case 'a':
			k, v, _ := strings.Cut(val, ":")
			k = strings.ToLower(k)
			if k == "control" {
				if cur != nil {
					cur.control = strings.TrimSpace(v)
				} else {
					s.control = strings.TrimSpace(v)
				}
				continue
			}
			if cur != nil {
				parseMediaAttr(cur, k, v)
			}
		}
	}
	return s, nil
}

func parseMediaAttr(m *sdpMedia, k, v string) {
	switch k {
	case "framerate":
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 && f < 1e4 {
			m.framerate = f
		}
	case "rtpmap":
		pt, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
		n, err := strconv.Atoi(pt)
		if !ok || err != nil {
			return
		}
		f := m.format(n)
		if f == nil {
			return
		}
		p := strings.Split(strings.TrimSpace(rest), "/")
		f.encoding = strings.ToUpper(p[0])
		if len(p) > 1 {
			if r, err := strconv.Atoi(p[1]); err == nil && r > 0 && r <= 10_000_000 {
				f.rate = r
			}
		}
		if len(p) > 2 {
			if c, err := strconv.Atoi(p[2]); err == nil && c > 0 && c <= 64 {
				f.channels = c
			}
		}
	case "fmtp":
		pt, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
		n, err := strconv.Atoi(pt)
		if !ok || err != nil {
			return
		}
		f := m.format(n)
		if f == nil {
			return
		}
		if f.fmtp == nil {
			f.fmtp = map[string]string{}
		}
		for _, kv := range strings.Split(rest, ";") {
			a, b, _ := strings.Cut(strings.TrimSpace(kv), "=")
			if a != "" && len(f.fmtp) < 64 {
				f.fmtp[strings.ToLower(a)] = b
			}
		}
	}
}
