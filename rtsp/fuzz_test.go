package rtsp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func FuzzParseSDP(f *testing.F) {
	f.Add([]byte(h264SDP()))
	f.Add([]byte(h265SDP()))
	f.Add([]byte(aacSDP))
	f.Add([]byte("m=video 0 RTP/AVP 96\na=fmtp:96 sprop-parameter-sets=,,,\na=rtpmap:96 H264/0/0\n"))
	base, _ := url.Parse("rtsp://h/x")
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parseSDP(b)
		if err != nil {
			return
		}
		c := newClientForTest(Config{})
		for _, p := range planTracks(s, base) {
			if p.info.ClockRate <= 0 {
				t.Fatalf("clock rate %d", p.info.ClockRate)
			}
			tr := &track{c: c, info: p.info, build: p.build}
			tr.depack = p.build(tr)
			_ = p.info.AudioConfig()
		}
	})
}

func FuzzReadMessage(f *testing.F) {
	f.Add([]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 2\r\n\r\nab"))
	f.Add([]byte("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n folded\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		br := bufio.NewReaderSize(bytes.NewReader(b), 1024)
		for i := 0; i < 16; i++ {
			m, err := readMessage(br)
			if err != nil {
				return
			}
			if len(m.body) > len(b) {
				t.Fatal("body larger than input")
			}
		}
	})
}

func FuzzHeaders(f *testing.F) {
	f.Add(`Digest realm="a", nonce="b", qop="auth", algorithm=SHA-256, stale=true`)
	f.Add(`Basic realm="a\"`)
	f.Add("RTP/AVP/TCP;unicast;interleaved=0-1;ssrc=ffffffff")
	f.Add("url=x;seq=1;rtptime=2,url=y")
	f.Fuzz(func(t *testing.T, s string) {
		cs := parseChallenges([]string{s})
		if c, ok := chooseChallenge(cs, true); ok {
			a := authenticator{user: "u", pass: "p", ch: c}
			if h := a.header("OPTIONS", "rtsp://h/"); strings.ContainsAny(h, "\r\n") {
				t.Fatalf("header injection: %q", h)
			}
		}
		parseTransport(s)
		parseRTPInfo(s)
		parseSessionHeader(s)
		parseURL(s)
		resolveControl(&url.URL{Scheme: "rtsp", Host: "h", Path: "/x"}, s)
	})
}

func FuzzRTP(f *testing.F) {
	f.Add(rtpPacket(1, 2, 96, true, []byte{1}))
	f.Add(appendRR(nil, receiverReport{}, "x"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, p, ok := parseRTP(b); ok && len(p) > len(b) {
			t.Fatal("payload larger than packet")
		}
		parseRTCP(b)
		h264SPSSize(b)
		if out, ok := h264LowDelaySPS(b); ok {
			if s, ok := parseH264SPS(out); !ok || !s.hasVUI {
				t.Fatalf("rewritten SPS does not parse: %x", out)
			}
		}
		h265SPSSize(b)
		parseASC(b)
		latmConfig(b)
	})
}

// FuzzDepacketize splits the input into length-prefixed packets and feeds them
// to every depacketizer, with loss events mixed in.
func FuzzDepacketize(f *testing.F) {
	f.Add(uint8(0), append([]byte{0, 3}, 0x65, 0x88, 0))
	f.Add(uint8(1), []byte{0, 4, 0x62, 1, 0x80 | 19, 1})
	f.Add(uint8(2), append([]byte{0, 10}, 0, 0, 0, 0, 1, 80, 8, 8, 1, 2))
	f.Add(uint8(3), []byte{0, 4, 0, 16, 0, 0x50})
	f.Add(uint8(4), []byte{0, 3, 255, 1, 7})
	sdps := []string{h264SDP(), h265SDP(), "v=0\nm=video 0 RTP/AVP 26\n", aacSDP,
		"v=0\nm=audio 0 RTP/AVP 100\na=rtpmap:100 MP4A-LATM/44100/2\na=fmtp:100 cpresent=0;config=400024203FC0\n",
		"v=0\nm=audio 0 RTP/AVP 0\n"}
	const maxUnit = 1 << 16
	f.Fuzz(func(t *testing.T, which uint8, b []byte) {
		h := newHarness(t, sdps[int(which)%len(sdps)], 8, Config{MaxUnitSize: maxUnit, QueueSize: 16})
		ts := uint32(0)
		total, packets := 0, 0
		for len(b) >= 2 {
			n := min(int(binary.BigEndian.Uint16(b)), len(b)-2)
			flags := byte(0)
			if n > 0 {
				flags = b[2]
			}
			body := b[2 : 2+n]
			b = b[2+n:]
			if flags&1 != 0 {
				ts += 3000
			}
			if flags&2 != 0 {
				h.skip(int(flags >> 5))
			}
			h.send(ts, flags&4 != 0, body)
			total += n
			packets++
			for _, p := range h.out() {
				if len(p.Data) > maxUnit+1<<12 {
					t.Fatalf("unit of %d bytes", len(p.Data))
				}
				p.Release()
			}
		}
		h.tr.expire(h.now.Add(time.Hour))
		var held int
		if d, ok := h.tr.depack.(*nalDepacketizer); ok && d.b.p != nil {
			held = len(d.b.p.store)
		}
		if held > 3*(total+maxUnit/8)+1<<16 {
			t.Fatalf("buffer of %d bytes for %d input bytes", held, total)
		}
	})
}

type scriptConn struct {
	net.Conn
	r io.Reader
}

func (c *scriptConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c *scriptConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *scriptConn) Close() error                     { return nil }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }

// FuzzConnection runs the connection reader over arbitrary bytes.
func FuzzConnection(f *testing.F) {
	f.Add(append([]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n$\x00\x00\x0d"), rtpPacket(1, 2, 96, true, []byte{0x41})...))
	f.Add([]byte("$\x01\x00\x08\x80\xc8\x00\x01\x00\x00\x00\x00"))
	f.Add([]byte("garbage\x00\xffPLAY x RTSP/1.0\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := newClientForTest(Config{})
		s, _ := parseSDP([]byte(h264SDP()))
		base, _ := url.Parse("rtsp://h/x")
		p := planTracks(s, base)[0]
		tr := &track{c: c, info: p.info, build: p.build}
		c.tracks = []*track{tr}
		tr.startSession(0, time.Millisecond, time.Now())
		sess := &session{
			c: c, conn: &scriptConn{r: bytes.NewReader(b)}, transport: TransportTCP,
			br: bufio.NewReaderSize(&scriptConn{r: bytes.NewReader(b)}, 4096), sem: make(chan struct{}, 1),
			readerDone: make(chan struct{}), failed: make(chan struct{}), stop: make(chan struct{}),
			media: []*sessionTrack{{t: tr}},
		}
		sess.targets[0].Store(&channelTarget{t: tr})
		sess.targets[1].Store(&channelTarget{t: tr, rtcp: true})
		sess.wg.Add(1)
		sess.readLoop()
		select {
		case <-sess.failed:
		default:
			t.Fatal("reader returned without reporting why")
		}
		if errors.Is(sess.failErr, nil) {
			t.Fatal("nil failure")
		}
	})
}

func FuzzRedact(f *testing.F) {
	f.Add("admin", "hunter22")
	f.Fuzz(func(t *testing.T, user, pass string) {
		if len(pass) < 5 {
			return
		}
		for _, r := range pass {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return
			}
		}
		if strings.Contains("rtsp://host/x?password=***&a=1", pass) {
			return
		}
		u := url.UserPassword(user, pass).String()
		raw := "rtsp://" + u + "@host/x?password=" + pass + "&a=1"
		if got := RedactURL(raw); strings.Contains(got, pass) {
			t.Fatalf("%q still contains the password", got)
		}
	})
}
