// Package rtsptest is a small scriptable RTSP server for tests. It listens on
// loopback only.
package rtsptest

import (
	"bufio"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server answers RTSP requests from a script. Set fields before Start.
type Server struct {
	SDP string
	// User and Pass turn on authentication using Auth: "basic", "md5" or "sha256".
	User, Pass, Auth string
	// Offer lists extra WWW-Authenticate values, sent before the real one.
	Offer []string

	DisableUDP, DisableTCP bool
	// TLS serves rtsps:// with this configuration.
	TLS *tls.Config
	// DropUDPMedia accepts UDP but discards what the script writes to it.
	DropUDPMedia bool
	// Redirect makes DESCRIBE answer 302 with this Location.
	Redirect string
	// SessionTimeout is announced in the Session header, if above zero.
	SessionTimeout int
	// RejectReason replaces the 401 reason phrase.
	RejectReason string
	// RTPInfo is sent in the PLAY response when set.
	RTPInfo string
	// OnPlay runs on its own goroutine after each PLAY.
	OnPlay func(*Stream)

	ln     net.Listener
	mu     sync.Mutex
	counts map[string]int
	authNo int
	conns  map[net.Conn]bool
	wg     sync.WaitGroup
	closed bool
}

// Start begins listening on a free loopback port.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	if s.TLS != nil {
		ln = tls.NewListener(ln, s.TLS)
	}
	s.ln = ln
	s.counts = map[string]int{}
	s.conns = map[net.Conn]bool{}
	s.wg.Add(1)
	go s.accept()
	return nil
}

// Addr returns host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// URL returns the stream URL.
func (s *Server) URL() string {
	if s.TLS != nil {
		return "rtsps://" + s.Addr() + "/stream"
	}
	return "rtsp://" + s.Addr() + "/stream"
}

// Count returns how many requests with this method arrived.
func (s *Server) Count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[method]
}

// AuthFailures counts requests that carried wrong credentials.
func (s *Server) AuthFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authNo
}

// DropConnections closes all client connections and keeps listening.
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.ln.Close()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			continue
		}
		s.conns[c] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c)
	}
}

// Stream is the media side of one client session.
type Stream struct {
	// TCP says the client chose interleaved transport.
	TCP  bool
	srv  *Server
	conn net.Conn
	wmu  *sync.Mutex
	done chan struct{}
	tr   []*streamTrack
	buf  []byte
}

type streamTrack struct {
	channel  int
	rtp      *net.UDPConn
	rtcp     *net.UDPConn
	peerRTP  netip.AddrPort
	peerRTCP netip.AddrPort
}

// Done closes when the client tore down or disconnected.
func (st *Stream) Done() <-chan struct{} { return st.done }

// Tracks returns the number of tracks set up.
func (st *Stream) Tracks() int { return len(st.tr) }

func (st *Stream) send(track int, rtcp bool, pkt []byte) error {
	t := st.tr[track]
	if st.TCP {
		ch := t.channel
		if rtcp {
			ch++
		}
		st.wmu.Lock()
		defer st.wmu.Unlock()
		st.buf = append(st.buf[:0], '$', byte(ch), byte(len(pkt)>>8), byte(len(pkt)))
		st.buf = append(st.buf, pkt...)
		st.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := st.conn.Write(st.buf)
		return err
	}
	if st.srv.DropUDPMedia {
		return nil
	}
	var err error
	if rtcp {
		_, err = t.rtcp.WriteToUDPAddrPort(pkt, t.peerRTCP)
	} else {
		_, err = t.rtp.WriteToUDPAddrPort(pkt, t.peerRTP)
	}
	return err
}

// WriteRTP sends one RTP packet on a track. It does not allocate.
func (st *Stream) WriteRTP(track int, pkt []byte) error { return st.send(track, false, pkt) }

// WriteRTCP sends one RTCP packet on a track.
func (st *Stream) WriteRTCP(track int, pkt []byte) error { return st.send(track, true, pkt) }

// Abort drops the connection without a teardown.
func (st *Stream) Abort() { st.conn.Close() }

var clientPorts = regexp.MustCompile(`client_port=(\d+)-(\d+)`)
var interleaved = regexp.MustCompile(`interleaved=(\d+)-(\d+)`)

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	st := &Stream{srv: s, conn: c, wmu: &sync.Mutex{}, done: make(chan struct{})}
	nonce := fmt.Sprintf("n%x", time.Now().UnixNano())
	played := false
	defer func() {
		close(st.done)
		c.Close()
		for _, t := range st.tr {
			if t.rtp != nil {
				t.rtp.Close()
				t.rtcp.Close()
			}
		}
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	br := bufio.NewReader(c)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return
		}
		if b[0] == '$' { // interleaved RTCP from the client
			h := make([]byte, 4)
			if _, err := fullRead(br, h); err != nil {
				return
			}
			if _, err := br.Discard(int(binary.BigEndian.Uint16(h[2:]))); err != nil {
				return
			}
			continue
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return
		}
		method, uri := f[0], f[1]
		hdr := map[string]string{}
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return
			}
			l = strings.TrimSpace(l)
			if l == "" {
				break
			}
			k, v, _ := strings.Cut(l, ":")
			hdr[strings.ToLower(k)] = strings.TrimSpace(v)
		}
		s.mu.Lock()
		s.counts[method]++
		s.mu.Unlock()
		cseq := hdr["cseq"]
		reply := func(status string, lines []string, body string) {
			var w strings.Builder
			fmt.Fprintf(&w, "RTSP/1.0 %s\r\nCSeq: %s\r\n", status, cseq)
			for _, l := range lines {
				w.WriteString(l + "\r\n")
			}
			if body != "" {
				fmt.Fprintf(&w, "Content-Length: %d\r\n", len(body))
			}
			w.WriteString("\r\n" + body)
			st.wmu.Lock()
			defer st.wmu.Unlock()
			c.Write([]byte(w.String()))
		}
		if s.Auth != "" && !s.authorized(method, uri, hdr["authorization"], nonce) {
			if hdr["authorization"] != "" {
				s.mu.Lock()
				s.authNo++
				s.mu.Unlock()
			}
			var lines []string
			for _, o := range s.Offer {
				lines = append(lines, "WWW-Authenticate: "+o)
			}
			lines = append(lines, "WWW-Authenticate: "+s.challenge(nonce))
			reason := "401 Unauthorized"
			if s.RejectReason != "" {
				reason = "401 " + s.RejectReason
			}
			reply(reason, lines, "")
			continue
		}
		session := "Session: 4242"
		if s.SessionTimeout > 0 {
			session += ";timeout=" + strconv.Itoa(s.SessionTimeout)
		}
		switch method {
		case "OPTIONS":
			reply("200 OK", []string{"Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER"}, "")
		case "DESCRIBE":
			if s.Redirect != "" {
				reply("302 Found", []string{"Location: " + s.Redirect}, "")
				continue
			}
			reply("200 OK", []string{"Content-Type: application/sdp", "Content-Base: rtsp://" + s.Addr() + "/stream/"}, s.SDP)
		case "SETUP":
			tr := hdr["transport"]
			if strings.Contains(tr, "TCP") {
				m := interleaved.FindStringSubmatch(tr)
				if s.DisableTCP || m == nil {
					reply("461 Unsupported Transport", nil, "")
					continue
				}
				ch, _ := strconv.Atoi(m[1])
				st.TCP = true
				st.tr = append(st.tr, &streamTrack{channel: ch})
				reply("200 OK", []string{session, "Transport: " + tr}, "")
				continue
			}
			m := clientPorts.FindStringSubmatch(tr)
			if s.DisableUDP || m == nil {
				reply("461 Unsupported Transport", nil, "")
				continue
			}
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			rtp, rtcp, err := udpPair()
			if err != nil {
				reply("500 Internal Server Error", nil, "")
				continue
			}
			ip := netip.MustParseAddr("127.0.0.1")
			st.tr = append(st.tr, &streamTrack{rtp: rtp, rtcp: rtcp,
				peerRTP: netip.AddrPortFrom(ip, uint16(a)), peerRTCP: netip.AddrPortFrom(ip, uint16(b))})
			sp := rtp.LocalAddr().(*net.UDPAddr).Port
			reply("200 OK", []string{session, fmt.Sprintf("Transport: RTP/AVP;unicast;client_port=%d-%d;server_port=%d-%d", a, b, sp, sp+1)}, "")
		case "PLAY":
			lines := []string{session}
			if s.RTPInfo != "" {
				lines = append(lines, "RTP-Info: "+s.RTPInfo)
			}
			reply("200 OK", lines, "")
			if !played && s.OnPlay != nil {
				played = true
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.OnPlay(st)
				}()
			}
		case "GET_PARAMETER":
			reply("200 OK", []string{session}, "")
		case "TEARDOWN":
			reply("200 OK", []string{session}, "")
			return
		default:
			reply("501 Not Implemented", nil, "")
		}
	}
}

func fullRead(br *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := br.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func udpPair() (*net.UDPConn, *net.UDPConn, error) {
	lo := net.IPv4(127, 0, 0, 1)
	for i := 0; i < 64; i++ {
		a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
		if err != nil {
			return nil, nil, err
		}
		p := a.LocalAddr().(*net.UDPAddr).Port
		if p%2 == 0 && p < 65535 {
			if b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lo, Port: p + 1}); err == nil {
				return a, b, nil
			}
		}
		a.Close()
	}
	return nil, nil, fmt.Errorf("rtsptest: no UDP port pair")
}

func (s *Server) algorithm() (string, func() hash.Hash) {
	if s.Auth == "sha256" {
		return "SHA-256", sha256.New
	}
	return "MD5", md5.New
}

func (s *Server) challenge(nonce string) string {
	if s.Auth == "basic" {
		return `Basic realm="test"`
	}
	alg, _ := s.algorithm()
	return fmt.Sprintf(`Digest realm="test", nonce="%s", algorithm=%s, qop="auth"`, nonce, alg)
}

var digestParam = regexp.MustCompile(`(\w+)=(?:"([^"]*)"|([^,\s]+))`)

func (s *Server) authorized(method, uri, header, nonce string) bool {
	if header == "" {
		return false
	}
	if s.Auth == "basic" {
		return header == "Basic "+base64.StdEncoding.EncodeToString([]byte(s.User+":"+s.Pass))
	}
	p := map[string]string{}
	for _, m := range digestParam.FindAllStringSubmatch(header, -1) {
		p[m[1]] = m[2] + m[3]
	}
	_, h := s.algorithm()
	sum := func(v string) string {
		d := h()
		d.Write([]byte(v))
		return hex.EncodeToString(d.Sum(nil))
	}
	ha1 := sum(s.User + ":test:" + s.Pass)
	ha2 := sum(method + ":" + uri)
	want := sum(ha1 + ":" + nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	return p["nonce"] == nonce && p["response"] == want && p["username"] == s.User
}
