package rtsp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/aep/gaal"
)

func TestDigestVectors(t *testing.T) {
	const params = `realm="testrealm@host.com", qop="auth", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"`
	for _, tc := range []struct{ alg, want string }{
		{"", "6629fae49393a05397450978507c4ef1"}, // the example in RFC 2617 section 3.5
		// The same inputs, computed separately with Python's hashlib.
		{", algorithm=SHA-256", "5abdd07184ba512a22c53f41470e5eea7dcaa3a93a59b630c13dfe0a5dc6e38b"},
	} {
		ch := parseChallenges([]string{"Digest " + params + tc.alg})
		a := authenticator{user: "Mufasa", pass: "Circle Of Life", ch: ch[0]}
		h := a.digest("GET", "/dir/index.html", "0a4f113b")
		if !strings.Contains(h, `response="`+tc.want+`"`) || !strings.Contains(h, "nc=00000001") {
			t.Errorf("digest%s: %s", tc.alg, h)
		}
	}
}

func TestChooseStrongestChallenge(t *testing.T) {
	cs := parseChallenges([]string{`Basic realm="x", Digest realm="x", nonce="1"`, `Digest realm="x", nonce="2", algorithm=SHA-256`, `Digest realm="x", nonce="3", algorithm=FOO`})
	if len(cs) != 4 {
		t.Fatalf("got %d challenges", len(cs))
	}
	c, ok := chooseChallenge(cs, true)
	if !ok || c.param("nonce") != "2" {
		t.Fatalf("picked %+v", c)
	}
	if _, ok := chooseChallenge(parseChallenges([]string{`Basic realm="x"`}), false); ok {
		t.Fatal("Basic used although disabled")
	}
}

func TestSPSSizes(t *testing.T) {
	for _, s := range [][2]int{{1280, 720}, {1920, 1080}, {640, 360}, {2560, 1440}} {
		if w, h, ok := h264SPSSize(testH264SPS(s[0], s[1])); !ok || w != s[0] || h != s[1] {
			t.Errorf("H.264 %v: got %dx%d ok=%v", s, w, h, ok)
		}
		if w, h, ok := h265SPSSize(testH265SPS(s[0], s[1])); !ok || w != s[0] || h != s[1] {
			t.Errorf("H.265 %v: got %dx%d ok=%v", s, w, h, ok)
		}
	}
}

func TestLowDelaySPS(t *testing.T) {
	// The SPS of a real high-level camera stream: POC type 0, one reference.
	nal, _ := base64.StdEncoding.DecodeString("Z2QAM6wVFKAoAPGQ")
	out, ok := h264LowDelaySPS(nal)
	if !ok {
		t.Fatal("not rewritten")
	}
	s, ok := parseH264SPS(out)
	if !ok || !s.hasVUI || s.width != 2560 || s.height != 1920 || s.numRef != 1 {
		t.Fatalf("%+v %v", s, ok)
	}
	r := spsReader{b: s.rbsp, pos: s.vuiPos + 1}
	for i := 0; i < 8; i++ {
		if r.bit() != 0 {
			t.Fatalf("VUI flag %d set", i)
		}
	}
	if r.bit() != 1 || r.bit() != 1 {
		t.Fatal("bitstream restriction missing")
	}
	r.ue()
	r.ue()
	r.ue()
	r.ue()
	if reorder, buffering := r.ue(), r.ue(); reorder != 0 || buffering != 1 || r.bad {
		t.Fatalf("reorder %d buffering %d", reorder, buffering)
	}
	if gaal.StreamRange(gaal.H264, annexB(out, idrNAL(8))) != gaal.RangeUnspecified {
		t.Fatal("gaal cannot read the rewritten SPS")
	}
	// An SPS that already has VUI, with timing information, keeps it.
	withVUI, _ := hex.DecodeString("67640033ac1514a0a03da1000004f6000045740404")
	before, ok := parseH264SPS(withVUI)
	if !ok || !before.hasVUI || !before.vuiOK || before.restricted {
		t.Fatalf("%+v %v", before, ok)
	}
	out, ok = h264LowDelaySPS(withVUI)
	if !ok {
		t.Fatal("SPS with VUI not rewritten")
	}
	after, ok := parseH264SPS(out)
	if !ok || !after.restricted || after.width != 640 || after.height != 480 {
		t.Fatalf("%+v %v", after, ok)
	}
	if !bytes.Equal(after.rbsp[:before.restrictPos/8], before.rbsp[:before.restrictPos/8]) {
		t.Fatal("existing VUI fields changed")
	}
	if again, ok := h264LowDelaySPS(out); ok {
		t.Fatalf("rewrote a restricted SPS again: %x", again)
	}
	if _, ok := h264LowDelaySPS(testH264SPS(1280, 720)); !ok {
		t.Fatal("single reference frame stream not rewritten")
	}
	h := newHarness(t, h264SDP(), 0, Config{LowDelay: true})
	h.send(0, true, idrNAL(20))
	h.send(3000, true, append([]byte(nil), testH264SPS(1280, 720)...))
	h.send(3000, true, idrNAL(21))
	for _, p := range h.out() {
		i := bytes.Index(p.Data, []byte{0, 0, 0, 1, 0x67})
		if i < 0 {
			t.Fatal("no SPS in keyframe")
		}
		end := i + 5 + bytes.Index(p.Data[i+5:], []byte{0, 0, 0, 1})
		if s, ok := parseH264SPS(p.Data[i+4 : end]); !ok || !s.hasVUI {
			t.Fatal("SPS in the stream was not rewritten")
		}
	}
}

func TestPlanTracks(t *testing.T) {
	sdp := "v=0\ns=x\na=control:rtsp://u:pw@10.0.0.1/all\n" +
		"m=video 0 RTP/AVP 96\na=rtpmap:96 H264/90000\na=control:trackID=1\n" +
		"m=audio 0 RTP/AVP 97\na=rtpmap:97 MPEG4-GENERIC/16000/1\n" +
		"a=fmtp:97 streamtype=5;mode=AAC-hbr;config=1408;sizelength=13;indexlength=3;indexdeltalength=3\na=control:trackID=2\n" +
		"m=audio 0 RTP/AVP 0\na=control:rtsp://u:pw@10.0.0.1/a3\n" +
		"m=audio 0 RTP/AVP 111\na=rtpmap:111 opus/48000/2\n" +
		"m=audio 0 RTP/AVP 100\na=rtpmap:100 MP4A-LATM/44100/2\na=fmtp:100 cpresent=0;config=400024203FC0\n" +
		"m=video 0 RTP/AVP 26\n" +
		"m=video 0 RTP/AVP 99\na=rtpmap:99 H264/90000\na=fmtp:99 packetization-mode=2\n" +
		"m=application 0 RTP/AVP 107\na=rtpmap:107 vnd.onvif.metadata/90000\n"
	s, err := parseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("rtsp://10.0.0.1/stream")
	p := planTracks(s, base)
	want := []gaal.Codec{gaal.H264, gaal.AAC, gaal.PCMU, gaal.Opus, gaal.AAC, gaal.MJPEG}
	if len(p) != len(want) {
		t.Fatalf("got %d tracks: %+v", len(p), p)
	}
	for i, c := range want {
		if p[i].info.Codec != c {
			t.Errorf("track %d: %v, want %v", i, p[i].info.Codec, c)
		}
	}
	if p[0].info.Control != "rtsp://10.0.0.1/stream/trackID=1" {
		t.Errorf("control %q", p[0].info.Control)
	}
	if strings.Contains(p[2].info.Control, "pw") {
		t.Errorf("credentials kept in control URL: %q", p[2].info.Control)
	}
	if hex.EncodeToString(p[1].info.Extra) != "1408" || p[1].info.AudioConfig().SampleRate != 16000 {
		t.Errorf("AAC config %+v", p[1].info)
	}
	if hex.EncodeToString(p[4].info.Extra) != "1210" {
		t.Errorf("LATM config %x", p[4].info.Extra)
	}
}

func TestReadMessageLimits(t *testing.T) {
	br := func(s string) *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 32<<10) }
	m, err := readMessage(br("RTSP/1.0 200 OK\nCSeq: 1\nContent-Length: 3\n\nabc"))
	if err != nil || string(m.body) != "abc" || m.code != 200 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := readMessage(br("RTSP/1.0 200 OK\nContent-Length: 99999999\n\n")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized body: %v", err)
	}
	var before, after runtime.MemStats
	r := br("RTSP/1.0 200 OK\nContent-Length: 1000000\n\nshort")
	runtime.ReadMemStats(&before)
	_, err = readMessage(r)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("truncated body accepted")
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 128<<10 {
		t.Fatalf("claimed length made it allocate %d bytes", n)
	}
	if _, err := readMessage(br("RTSP/1.0 200 OK\n" + strings.Repeat("A: b\n", maxHeaders+1) + "\n")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("too many headers: %v", err)
	}
}

func TestTransportHeaders(t *testing.T) {
	s, err := parseTransport("RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001;ssrc=0A0B0C0D;source=10.0.0.2")
	if err != nil || s.tcp || s.serverPort != [2]int{6000, 6001} || s.ssrc != 0x0A0B0C0D || s.source != "10.0.0.2" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = parseTransport("RTP/AVP/TCP;unicast;interleaved=2-3")
	if err != nil || !s.tcp || s.interleaved != [2]int{2, 3} {
		t.Fatalf("%+v %v", s, err)
	}
	in := parseRTPInfo("url=rtsp://h/a/track1;seq=5;rtptime=99,url=rtsp://h/a/track2;seq=6;rtptime=100")
	if len(in) != 2 || in[1].rtpTime != 100 || in[0].seq != 5 {
		t.Fatalf("%+v", in)
	}
	if id, to := parseSessionHeader("abc;timeout=30"); id != "abc" || to != 30 {
		t.Fatal(id, to)
	}
}

func TestRTPParsing(t *testing.T) {
	p := rtpPacket(7, 9, 96, true, []byte{1, 2, 3})
	h, pl, ok := parseRTP(p)
	if !ok || !h.marker || h.seq != 7 || h.ts != 9 || len(pl) != 3 {
		t.Fatal(h, pl, ok)
	}
	withExt := append([]byte{0x90}, p[1:12]...)
	withExt = append(withExt, 0xBE, 0xDE, 0, 1, 1, 2, 3, 4, 9)
	if _, pl, ok := parseRTP(withExt); !ok || len(pl) != 1 || pl[0] != 9 {
		t.Fatal("extension", pl, ok)
	}
	padded := append([]byte{0xA0}, p[1:]...)
	padded = append(padded, 0, 2)
	if _, pl, ok := parseRTP(padded); !ok || len(pl) != 3 {
		t.Fatal("padding", pl, ok)
	}
	bad := append([]byte{0xA0}, p[1:12]...)
	if _, _, ok := parseRTP(append(bad, 200)); ok {
		t.Fatal("oversized padding accepted")
	}
	rr := appendRR(nil, receiverReport{ssrc: 1, mediaSSRC: 2}, "gaal")
	if ev, ok := parseRTCP(rr); !ok || ev.packets != 2 {
		t.Fatalf("own RTCP does not parse: %+v %v", ev, ok)
	}
}

func TestResolveControl(t *testing.T) {
	for _, c := range []struct{ base, control, want string }{
		{"rtsp://h/x", "trackID=0", "rtsp://h/x/trackID=0"},
		{"rtsp://h/x/", "trackID=0", "rtsp://h/x/trackID=0"},
		{"rtsp://h/x?a=1", "trackID=0", "rtsp://h/x?a=1/trackID=0"},
		{"rtsp://h/x", "?ctype=video", "rtsp://h/x?ctype=video"},
		{"rtsp://h/x", "*", "rtsp://h/x"},
		{"rtsp://h/x", "rtsp://other/y/track1", "rtsp://other/y/track1"},
		{"rtsps://h/x", "rtsps://u:p@h/y", "rtsps://h/y"},
	} {
		base, err := url.Parse(c.base)
		if err != nil {
			t.Fatal(err)
		}
		if got := resolveControl(base, c.control); got != c.want {
			t.Errorf("%s + %s = %s, want %s", c.base, c.control, got, c.want)
		}
	}
}
