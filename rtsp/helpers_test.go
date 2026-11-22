package rtsp

import (
	"encoding/base64"
	"encoding/binary"
	"net/url"
	"testing"
	"time"
)

func rtpPacket(seq uint16, ts uint32, pt uint8, marker bool, payload []byte) []byte {
	b := make([]byte, 12, 12+len(payload))
	b[0] = 0x80
	b[1] = pt
	if marker {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], ts)
	binary.BigEndian.PutUint32(b[8:], 0x11223344)
	return append(b, payload...)
}

type bitWriter struct {
	b []byte
	n int
}

func (w *bitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>uint(i)&1) << (7 - w.n%8)
		w.n++
	}
}

func (w *bitWriter) ue(v uint32) {
	v++
	bits := 0
	for x := v; x > 1; x >>= 1 {
		bits++
	}
	w.u(bits, 0)
	w.u(bits+1, v)
}

func escapeRBSP(b []byte) []byte {
	var out []byte
	zeros := 0
	for _, v := range b {
		if zeros >= 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

func testH264SPS(width, height int) []byte {
	w := &bitWriter{}
	w.u(8, 66)
	w.u(8, 0)
	w.u(8, 31)
	w.ue(0) // sps id
	w.ue(0) // log2_max_frame_num
	w.ue(2) // poc type
	w.ue(1) // refs
	w.u(1, 0)
	mbW, mbH := (width+15)/16, (height+15)/16
	w.ue(uint32(mbW - 1))
	w.ue(uint32(mbH - 1))
	w.u(1, 1) // frame_mbs_only
	w.u(1, 1) // direct_8x8
	cropR, cropB := (mbW*16-width)/2, (mbH*16-height)/2
	if cropR+cropB > 0 {
		w.u(1, 1)
		w.ue(0)
		w.ue(uint32(cropR))
		w.ue(0)
		w.ue(uint32(cropB))
	} else {
		w.u(1, 0)
	}
	w.u(1, 0) // vui
	w.u(1, 1) // stop bit
	return append([]byte{0x67}, escapeRBSP(w.b)...)
}

func testH265SPS(width, height int) []byte {
	w := &bitWriter{}
	w.u(4, 0)
	w.u(3, 0)
	w.u(1, 1)
	w.u(8, 1) // profile space, tier, profile idc
	for i := 0; i < 11; i++ {
		w.u(8, 0x11)
	}
	w.ue(0) // sps id
	w.ue(1) // 4:2:0
	w.ue(uint32(width))
	w.ue(uint32(height))
	w.u(1, 0)
	w.u(1, 1)
	return append([]byte{0x42, 0x01}, escapeRBSP(w.b)...)
}

var (
	testPPS  = []byte{0x68, 0xCE, 0x3C, 0x80}
	testVPS5 = []byte{0x40, 0x01, 0x0C, 0x01}
	testPPS5 = []byte{0x44, 0x01, 0xC0, 0xF7}
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func h264SDP() string {
	return "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=test\r\nt=0 0\r\na=control:*\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=framerate:25\r\n" +
		"a=fmtp:96 packetization-mode=1;profile-level-id=42001f;sprop-parameter-sets=" +
		b64(testH264SPS(1280, 720)) + "," + b64(testPPS) + "\r\na=control:track1\r\n"
}

func h265SDP() string {
	return "v=0\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 98\r\na=rtpmap:98 H265/90000\r\n" +
		"a=fmtp:98 sprop-vps=" + b64(testVPS5) + ";sprop-sps=" + b64(testH265SPS(1920, 1080)) + ";sprop-pps=" + b64(testPPS5) + "\r\n" +
		"a=control:track1\r\n"
}

// harness runs one depacketizer without any network.
type harness struct {
	t   *testing.T
	c   *Client
	tr  *track
	seq uint16
	now time.Time
}

func newClientForTest(cfg Config) *Client {
	c := &Client{cfg: cfg.withDefaults(), scrub: newScrubber("", ""), finished: make(chan struct{})}
	c.queue = make(chan *Packet, c.cfg.QueueSize)
	c.url, _ = url.Parse("rtsp://127.0.0.1/x")
	c.origin = c.url
	return c
}

func newHarness(t testing.TB, sdp string, window int, cfg Config) *harness {
	s, err := parseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("rtsp://127.0.0.1/x")
	plans := planTracks(s, base)
	if len(plans) == 0 {
		t.Fatal("no tracks in SDP")
	}
	c := newClientForTest(cfg)
	tr := &track{c: c, info: plans[0].info, build: plans[0].build}
	c.tracks = []*track{tr}
	h := &harness{c: c, tr: tr, now: time.Unix(1000, 0)}
	tr.startSession(window, 40*time.Millisecond, h.now)
	return h
}

func (h *harness) send(ts uint32, marker bool, payload []byte) {
	h.tr.receive(rtpPacket(h.seq, ts, h.tr.info.PayloadType, marker, payload), h.now)
	h.seq++
}

// skip pretends n packets were lost.
func (h *harness) skip(n int) { h.seq += uint16(n) }

func (h *harness) out() []*Packet {
	var ps []*Packet
	for {
		p := h.c.TryRead()
		if p == nil {
			return ps
		}
		ps = append(ps, p)
	}
}

func fuA(nal []byte, chunk int) [][]byte {
	var out [][]byte
	hdr, body := nal[0], nal[1:]
	for first := true; len(body) > 0; first = false {
		n := min(chunk, len(body))
		fu := hdr & 0x1F
		if first {
			fu |= 0x80
		}
		if n == len(body) {
			fu |= 0x40
		}
		out = append(out, append([]byte{hdr&0xE0 | 28, fu}, body[:n]...))
		body = body[n:]
	}
	return out
}

func fuH265(nal []byte, chunk int) [][]byte {
	var out [][]byte
	typ := nal[0] >> 1 & 0x3F
	body := nal[2:]
	for first := true; len(body) > 0; first = false {
		n := min(chunk, len(body))
		fu := typ
		if first {
			fu |= 0x80
		}
		if n == len(body) {
			fu |= 0x40
		}
		out = append(out, append([]byte{nal[0]&0x81 | 49<<1, nal[1], fu}, body[:n]...))
		body = body[n:]
	}
	return out
}

func stapA(nals ...[]byte) []byte {
	out := []byte{0x78}
	for _, n := range nals {
		out = append(out, byte(len(n)>>8), byte(len(n)))
		out = append(out, n...)
	}
	return out
}

func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func filled(first byte, n int) []byte {
	b := make([]byte, n)
	b[0] = first
	for i := 1; i < n; i++ {
		b[i] = byte(i%200 + 8)
	}
	return b
}

func idrNAL(n int) []byte {
	b := filled(0x65, n)
	b[1] = 0x88 // first_mb_in_slice = 0
	return b
}

func pNAL(n int) []byte {
	b := filled(0x41, n)
	b[1] = 0x9A
	return b
}
