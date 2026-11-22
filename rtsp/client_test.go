package rtsp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aep/gaal/rtsp/rtsptest"
)

const (
	testUser = "viewer"
	testPass = "s3cret-Pw"
)

// playFrames writes an IDR and P frames until the client leaves.
func playFrames(st *rtsptest.Stream) {
	seq, ts := uint16(100), uint32(5000)
	send := func(marker bool, b []byte) bool {
		err := st.WriteRTP(0, rtpPacket(seq, ts, 96, marker, b))
		seq++
		return err == nil
	}
	for n := 0; ; n++ {
		if n%10 == 0 {
			frags := fuA(idrNAL(3000), 1000)
			for i, f := range frags {
				if !send(i == len(frags)-1, f) {
					return
				}
			}
		} else if !send(true, pNAL(200)) {
			return
		}
		ts += 3600
		select {
		case <-st.Done():
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func startServer(t *testing.T, s *rtsptest.Server) *rtsptest.Server {
	t.Helper()
	if s.SDP == "" {
		s.SDP = h264SDP()
	}
	if s.OnPlay == nil {
		s.OnPlay = playFrames
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readN(t *testing.T, c *Client, n int) []*Packet {
	t.Helper()
	var out []*Packet
	for len(out) < n {
		p, err := c.Read(testCtx(t))
		if err != nil {
			t.Fatalf("after %d packets: %v", len(out), err)
		}
		out = append(out, p)
	}
	return out
}

func TestPlayTransports(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportUDP, TransportAuto} {
		t.Run(tr.String(), func(t *testing.T) {
			srv := startServer(t, &rtsptest.Server{})
			c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: tr})
			if err != nil {
				t.Fatal(err)
			}
			ps := readN(t, c, 25)
			if !ps[0].Keyframe || !ps[0].Discontinuity || !bytes.HasPrefix(ps[0].Data, []byte{0, 0, 0, 1, 0x67}) {
				t.Fatalf("first packet key=%v disc=%v", ps[0].Keyframe, ps[0].Discontinuity)
			}
			if ps[1].PTS-ps[0].PTS != 40*time.Millisecond {
				t.Fatalf("PTS step %v", ps[1].PTS-ps[0].PTS)
			}
			if ps[0].Arrival.IsZero() {
				t.Fatal("no arrival time")
			}
			for _, p := range ps {
				p.Release()
			}
			want := TransportUDP
			if tr == TransportTCP {
				want = TransportTCP
			}
			st := c.Stats()
			if st.Transport != want || st.Tracks[0].Units < 25 || st.Tracks[0].PacketsLost != 0 {
				t.Fatalf("%+v", st)
			}
			c.Close()
			deadline := time.Now().Add(2 * time.Second)
			for srv.Count("TEARDOWN") != 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if srv.Count("TEARDOWN") != 1 {
				t.Fatal("no TEARDOWN")
			}
			if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
				t.Fatalf("read after close: %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthSchemes(t *testing.T) {
	for _, auth := range []string{"basic", "md5", "sha256"} {
		t.Run(auth, func(t *testing.T) {
			srv := startServer(t, &rtsptest.Server{User: testUser, Pass: testPass, Auth: auth})
			c, err := Dial(testCtx(t), Config{URL: srv.URL(), Username: testUser, Password: testPass, Transport: TransportTCP})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			readN(t, c, 1)[0].Release()
			if srv.AuthFailures() != 0 {
				t.Fatalf("%d rejected attempts", srv.AuthFailures())
			}
		})
	}
}

func TestCredentialsInURL(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{User: testUser, Pass: testPass, Auth: "md5"})
	u := "rtsp://" + testUser + ":" + testPass + "@" + srv.Addr() + "/stream"
	c, err := Dial(testCtx(t), Config{URL: u, Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 1)[0].Release()
	if strings.Contains(c.URL(), testPass) || strings.Contains(c.Stats().URL, testUser) {
		t.Fatal("credentials in URL accessors")
	}
}

func TestWrongPasswordStopsAfterOneAttempt(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{User: testUser, Pass: testPass, Auth: "md5"})
	_, err := Dial(testCtx(t), Config{
		URL: srv.URL(), Username: testUser, Password: "wrong-" + testPass,
		Reconnect: &Reconnect{MinDelay: time.Millisecond},
	})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	if n := srv.AuthFailures(); n != 1 {
		t.Fatalf("%d password attempts, want 1", n)
	}
}

func TestRedirect(t *testing.T) {
	target := startServer(t, &rtsptest.Server{})
	front := startServer(t, &rtsptest.Server{Redirect: target.URL()})
	c, err := Dial(testCtx(t), Config{URL: front.URL(), Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 1)[0].Release()
	if target.Count("PLAY") != 1 {
		t.Fatal("did not reach redirect target")
	}
}

func TestRedirectLoopStops(t *testing.T) {
	srv := &rtsptest.Server{}
	startServer(t, srv)
	srv.Redirect = srv.URL()
	if _, err := Dial(testCtx(t), Config{URL: srv.URL()}); !errors.Is(err, ErrRedirects) {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialsNotSentToRedirectHost(t *testing.T) {
	target := startServer(t, &rtsptest.Server{User: testUser, Pass: testPass, Auth: "basic"})
	other := strings.Replace(target.URL(), "127.0.0.1", "localhost", 1)
	front := startServer(t, &rtsptest.Server{Redirect: other})
	_, err := Dial(testCtx(t), Config{URL: front.URL(), Username: testUser, Password: testPass, Transport: TransportTCP})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	if target.AuthFailures() != 0 {
		t.Fatal("credentials were offered to another host")
	}
}

func TestAutoFallsBackWhenUDPSilent(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{DropUDPMedia: true})
	var events atomic.Int32
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), UDPProbe: 150 * time.Millisecond,
		OnEvent: func(e Event) {
			if e.Kind == EventTransportFallback {
				events.Add(1)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 3)
	if c.Stats().Transport != TransportTCP || events.Load() != 1 {
		t.Fatalf("transport %v, %d events", c.Stats().Transport, events.Load())
	}
	if srv.Count("TEARDOWN") != 1 {
		t.Fatalf("the abandoned UDP session was not torn down: %d", srv.Count("TEARDOWN"))
	}
}

func TestAutoFallsBackWhenUDPRefused(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{DisableUDP: true})
	c, err := Dial(testCtx(t), Config{URL: srv.URL()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 1)
	if c.Stats().Transport != TransportTCP {
		t.Fatal("not on TCP")
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP,
		Reconnect: &Reconnect{MinDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	last := readN(t, c, 5)[4]
	srv.DropConnections()
	ctx := testCtx(t)
	for {
		p, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.Discontinuity {
			if !p.Keyframe {
				t.Fatal("reconnected stream should start at the keyframe")
			}
			if p.PTS <= last.PTS {
				t.Fatalf("PTS went backwards over reconnect: %v after %v", p.PTS, last.PTS)
			}
			break
		}
	}
	if c.Stats().Sessions != 2 {
		t.Fatalf("sessions %d", c.Stats().Sessions)
	}
}

func TestNoReconnectByDefault(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 1)
	srv.DropConnections()
	ctx := testCtx(t)
	for {
		p, err := c.Read(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("no error after the connection dropped")
			}
			return
		}
		p.Release()
	}
}

func TestKeepalive(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{SessionTimeout: 2})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(4 * time.Second)
	for srv.Count("GET_PARAMETER") == 0 && time.Now().Before(deadline) {
		if p, err := c.ReadTimeout(100 * time.Millisecond); err == nil {
			p.Release()
		}
	}
	if srv.Count("GET_PARAMETER") == 0 {
		t.Fatal("no keepalive")
	}
}

func TestReadTimeoutWhenSilent(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{OnPlay: func(st *rtsptest.Stream) { <-st.Done() }})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP, ReadTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ReadTimeout(20 * time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("ReadTimeout: %v", err)
	}
	if _, err := c.Read(testCtx(t)); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Read: %v", err)
	}
	if srv.Count("TEARDOWN") != 1 {
		t.Fatal("no TEARDOWN after timeout")
	}
}

func TestContextCancelsDial(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{DropUDPMedia: true})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := Dial(ctx, Config{URL: srv.URL(), Transport: TransportAuto, UDPProbe: 10 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancel took too long")
	}
	if srv.Count("TEARDOWN") != 1 {
		t.Fatalf("no TEARDOWN after cancel")
	}
}

func TestReadHonoursContext(t *testing.T) {
	srv := startServer(t, &rtsptest.Server{OnPlay: func(st *rtsptest.Stream) { <-st.Done() }})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestSelectTracks(t *testing.T) {
	sdp := h264SDP() + "m=audio 0 RTP/AVP 0\r\na=control:track2\r\n"
	srv := startServer(t, &rtsptest.Server{SDP: sdp})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP, Select: func(tr Track) bool { return tr.Media == Video }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := len(c.Tracks()); n != 1 || srv.Count("SETUP") != 1 {
		t.Fatalf("%d tracks, %d SETUP", n, srv.Count("SETUP"))
	}
}

func TestRTCPSenderReportAndBye(t *testing.T) {
	sr := make([]byte, 28)
	sr[0], sr[1], sr[3] = 0x80, 200, 6
	bye := []byte{0x81, 203, 0, 1, 0, 0, 0, 1}
	srv := startServer(t, &rtsptest.Server{OnPlay: func(st *rtsptest.Stream) {
		st.WriteRTCP(0, sr)
		st.WriteRTP(0, rtpPacket(0, 0, 96, true, pNAL(20)))
		st.WriteRTCP(0, bye)
		<-st.Done()
	}})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportTCP})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readN(t, c, 1)
	if _, err := c.Read(testCtx(t)); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorsAndLogsHideCredentials(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	check := func(what, s string) {
		t.Helper()
		if strings.Contains(s, testPass) || strings.Contains(s, testUser) {
			t.Errorf("%s leaks credentials: %s", what, s)
		}
	}
	// Unparsable URL: net/url would quote it.
	_, err := Dial(testCtx(t), Config{URL: "rtsp://" + testUser + ":" + testPass + "@host:bad port/x"})
	if err == nil {
		t.Fatal("bad URL accepted")
	}
	check("parse error", err.Error())
	// Connection refused.
	_, err = Dial(testCtx(t), Config{URL: "rtsp://" + testUser + ":" + testPass + "@127.0.0.1:1/x", Logger: logger})
	if err == nil {
		t.Fatal("dial succeeded")
	}
	check("dial error", err.Error())
	// A server that echoes the password in its status text.
	srv := startServer(t, &rtsptest.Server{User: testUser, Pass: "other", Auth: "basic", RejectReason: "bad password " + testPass})
	_, err = Dial(testCtx(t), Config{URL: "rtsp://" + testUser + ":" + testPass + "@" + srv.Addr() + "/s", Logger: logger})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	check("auth error", err.Error())

	// A status error quoting a URL with credentials, through the scrubber.
	s := newScrubber(testUser, testPass)
	wrapped := s.wrap(&StatusError{Method: "SETUP", Code: 500, Reason: "rtsp://" + testUser + ":" + testPass + "@h/x " + testPass})
	check("status error", wrapped.Error())
	var se *StatusError
	if !errors.As(wrapped, &se) || se.Code != 500 {
		t.Fatal("StatusError not reachable")
	}
	if errors.Unwrap(wrapped) != nil {
		t.Fatal("the unredacted error is reachable through Unwrap")
	}

	// A working session with logging, whose SDP also carries credentials.
	sdp := strings.Replace(h264SDP(), "a=control:track1", "a=control:rtsp://"+testUser+":"+testPass+"@127.0.0.1/track1", 1)
	srv2 := startServer(t, &rtsptest.Server{SDP: sdp, User: testUser, Pass: testPass, Auth: "md5"})
	c, err := Dial(testCtx(t), Config{URL: "rtsp://" + testUser + ":" + testPass + "@" + srv2.Addr() + "/s", Transport: TransportTCP, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	readN(t, c, 1)
	check("track", c.Tracks()[0].Control)
	check("track", c.Tracks()[0].String())
	c.Close()
	check("log", logs.String())
	if logs.Len() == 0 {
		t.Fatal("expected some log output")
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://u:p@h:554/a?b=1":                     "rtsp://h:554/a?b=1",
		"rtsp://u:p@ss/w@h/a":                        "rtsp://h/a",
		"rtsp://h/a?user=admin&password=x&channel=1": "rtsp://h/a?user=***&password=***&channel=1",
		"http://h/onvif?token=abc":                   "http://h/onvif?token=***",
		"rtsp://h/a":                                 "rtsp://h/a",
		"not a url":                                  "not a url",
		"rtsp://u:p@h/a#frag":                        "rtsp://h/a#***",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedactText(`dial "rtsp://u:pw@h/a" failed`); strings.Contains(got, "pw") {
		t.Errorf("RedactText: %s", got)
	}
}

// rtsps:// speaks TLS and carries media over it, whatever Transport says.
func TestRTSPS(t *testing.T) {
	cert, pool := selfSigned(t)
	srv := startServer(t, &rtsptest.Server{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}})
	c, err := Dial(testCtx(t), Config{URL: srv.URL(), Transport: TransportUDP, TLSConfig: &tls.Config{RootCAs: pool}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range readN(t, c, 5) {
		p.Release()
	}
	if st := c.Stats(); st.Transport != TransportTCP {
		t.Fatalf("transport %v, want TCP", st.Transport)
	}
	if _, err := Dial(testCtx(t), Config{URL: srv.URL(), DialTimeout: time.Second}); err == nil {
		t.Fatal("an unverified certificate was accepted")
	}
}

// selfSigned is a certificate for 127.0.0.1 and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rtsptest"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
