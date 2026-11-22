package rtsp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRedirects     = 5
	teardownTimeout  = 2 * time.Second
	housekeepEvery   = 250 * time.Millisecond
	reportEvery      = 5 * time.Second
	lossCheckEvery   = 5 * time.Second
	lossCheckMinimum = 300
	lossFallbackPct  = 10
	maxGarbageBytes  = 1 << 20
)

var errFallbackTCP = errors.New("rtsp: too much UDP loss, switching to TCP")

type channelTarget struct {
	t    *track
	rtcp bool
}

// session is one RTSP connection and its media sockets.
type session struct {
	c         *Client
	transport Transport
	probeUDP  bool

	conn net.Conn
	br   *bufio.Reader
	peer netip.Addr
	wmu  sync.Mutex
	sem  chan struct{} // one request at a time

	cseq     int
	auth     *authenticator
	id       string
	timeout  time.Duration
	base     *url.URL
	playURL  string
	useParam bool

	waitMu   sync.Mutex
	waitSeq  int
	waitCh   chan *message
	waitHook func(*message)

	targets [256]atomic.Pointer[channelTarget]
	media   []*sessionTrack

	readerDone chan struct{}
	failed     chan struct{}
	failOnce   sync.Once
	failErr    error
	stop       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
	byes       atomic.Int32
	sawUDP     atomic.Bool // any datagram arrived from the server
}

// sessionTrack is one track's transport inside a session.
type sessionTrack struct {
	t        *track
	rtp      *net.UDPConn
	rtcp     *net.UDPConn
	rtpAddr  netip.AddrPort
	rtcpAddr netip.AddrPort
	source   netip.Addr
	channel  int
	gotBye   atomic.Bool
}

func (s *session) fail(err error) {
	s.failOnce.Do(func() {
		s.failErr = err
		close(s.failed)
	})
}

func (s *session) wait(ctx context.Context) error {
	select {
	case <-s.failed:
		return s.failErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// openSession connects and follows redirects until the stream plays.
func (c *Client) openSession(ctx context.Context, tr Transport, probe, first bool) (*session, error) {
	c.mu.Lock()
	c.url = c.origin
	c.mu.Unlock()
	for hop := 0; ; hop++ {
		s, redirect, err := c.tryURL(ctx, c.currentURL(), tr, probe, first)
		if err != nil {
			return nil, err
		}
		if redirect == nil {
			return s, nil
		}
		if hop >= maxRedirects {
			return nil, ErrRedirects
		}
		c.logf("rtsp: redirected", "to", RedactURL(redirect.String()))
		c.mu.Lock()
		c.url = redirect
		c.mu.Unlock()
	}
}

func (c *Client) tryURL(ctx context.Context, u *url.URL, tr Transport, probe, first bool) (*session, *url.URL, error) {
	host := u.Host
	if u.Port() == "" {
		port := "554"
		if secure(u) {
			port = "322"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	dctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	var conn net.Conn
	var err error
	if secure(u) {
		cfg := c.cfg.TLSConfig.Clone()
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		d := tls.Dialer{Config: cfg}
		conn, err = d.DialContext(dctx, "tcp", host)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(dctx, "tcp", host)
	}
	if err != nil {
		return nil, nil, err
	}
	s := &session{
		c: c, transport: tr, probeUDP: probe, conn: conn,
		br:         bufio.NewReaderSize(conn, 128<<10),
		sem:        make(chan struct{}, 1),
		base:       u,
		timeout:    60 * time.Second,
		readerDone: make(chan struct{}),
		failed:     make(chan struct{}),
		stop:       make(chan struct{}),
	}
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		s.peer = a.AddrPort().Addr().Unmap()
	}
	s.wg.Add(1)
	go s.readLoop()
	redirect, err := s.negotiate(ctx, first)
	if err != nil || redirect != nil {
		s.shutdown()
		if err != nil && ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, redirect, err
	}
	s.wg.Add(1)
	go s.housekeep()
	return s, nil, nil
}

func (s *session) negotiate(ctx context.Context, first bool) (*url.URL, error) {
	c := s.c
	uri := s.base.String()
	m, err := s.do(ctx, "OPTIONS", uri, "")
	if err != nil {
		var se *StatusError
		if !errors.As(err, &se) && !errors.Is(err, ErrProtocol) {
			return nil, err
		}
	} else {
		if to := s.redirectTarget(m); to != nil {
			return to, nil
		}
		s.useParam = strings.Contains(strings.ToUpper(m.get("Public")), "GET_PARAMETER")
	}
	m, err = s.do(ctx, "DESCRIBE", uri, "Accept: application/sdp\r\n")
	if err != nil {
		return nil, err
	}
	if to := s.redirectTarget(m); to != nil {
		return to, nil
	}
	if err := checkStatus("DESCRIBE", m); err != nil {
		return nil, err
	}
	sdp, err := parseSDP(m.body)
	if err != nil {
		return nil, err
	}
	base := s.base
	for _, name := range []string{"Content-Base", "Content-Location"} {
		if v := m.get(name); v != "" {
			if u, _, _, err := parseURL(v); err == nil {
				base = u
				break
			}
		}
	}
	plans := planTracks(sdp, base)
	if err := c.bindTracks(plans, first); err != nil {
		return nil, err
	}
	s.playURL = resolveControl(base, sdp.control)
	c.mu.Lock()
	tracks := append([]*track(nil), c.tracks...)
	c.mu.Unlock()
	now := time.Now()
	for i, t := range tracks {
		st, err := s.setupTrack(ctx, t, i)
		if err != nil {
			s.closeMedia()
			return nil, err
		}
		window := 0
		if s.transport == TransportUDP && c.cfg.ReorderWindow > 0 {
			window = c.cfg.ReorderWindow
		}
		t.startSession(window, c.cfg.ReorderDelay, now)
		s.media = append(s.media, st)
	}
	// Count the session before PLAY: packets, including the Discontinuity one
	// after a reconnect, can reach Read before negotiate returns.
	c.addSessions(1)
	if err := s.play(ctx); err != nil {
		c.addSessions(-1)
		return nil, err
	}
	if s.transport == TransportUDP {
		s.startUDP()
		if s.probeUDP {
			if err := s.awaitFirstPacket(ctx, now); err != nil {
				c.addSessions(-1)
				return nil, err
			}
		}
	}
	return nil, nil
}

func checkStatus(method string, m *message) error {
	if m.code >= 200 && m.code < 300 {
		return nil
	}
	return &StatusError{Method: method, Code: m.code, Reason: m.reason}
}

func (s *session) redirectTarget(m *message) *url.URL {
	if m.code != 301 && m.code != 302 && m.code != 303 && m.code != 305 && m.code != 307 {
		return nil
	}
	u, user, pass, err := parseURL(m.get("Location"))
	if err != nil {
		return nil
	}
	if user != "" || pass != "" {
		return nil // never take credentials from a server
	}
	return u
}

// bindTracks creates tracks on the first connection and checks them afterwards.
func (c *Client) bindTracks(plans []trackPlan, first bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !first || len(c.tracks) > 0 {
		return c.rebindLocked(plans)
	}
	for _, p := range plans {
		info := p.info
		if c.cfg.Select != nil && !c.cfg.Select(info) {
			continue
		}
		info.Index = len(c.tracks)
		c.tracks = append(c.tracks, &track{c: c, index: info.Index, info: info, build: p.build})
	}
	if len(c.tracks) == 0 {
		return ErrNoTracks
	}
	return nil
}

func (c *Client) rebindLocked(plans []trackPlan) error {
	for _, t := range c.tracks {
		found := false
		for _, p := range plans {
			if p.info.Media == t.info.Media && p.info.Codec == t.info.Codec && p.info.ClockRate == t.info.ClockRate {
				t.info.PayloadType, t.info.Control = p.info.PayloadType, p.info.Control
				found = true
				break
			}
		}
		if !found {
			return ErrStreamChanged
		}
	}
	return nil
}

func (s *session) setupTrack(ctx context.Context, t *track, i int) (*sessionTrack, error) {
	st := &sessionTrack{t: t, channel: i * 2}
	var hdr string
	if s.transport == TransportTCP {
		hdr = fmt.Sprintf("Transport: RTP/AVP/TCP;unicast;interleaved=%d-%d\r\n", i*2, i*2+1)
	} else {
		if err := s.openUDP(st); err != nil {
			return nil, err
		}
		a := st.rtp.LocalAddr().(*net.UDPAddr).Port
		hdr = fmt.Sprintf("Transport: RTP/AVP;unicast;client_port=%d-%d\r\n", a, a+1)
	}
	m, err := s.do(ctx, "SETUP", t.info.Control, hdr)
	if err == nil {
		err = checkStatus("SETUP", m)
	}
	if err != nil {
		st.close()
		return nil, err
	}
	spec, err := parseTransport(m.get("Transport"))
	if err != nil {
		st.close()
		return nil, err
	}
	if id, to := parseSessionHeader(m.get("Session")); id != "" {
		s.id = id
		if to > 0 {
			s.timeout = time.Duration(to) * time.Second
		}
	}
	if s.id == "" {
		st.close()
		return nil, fmt.Errorf("%w: SETUP without Session", ErrProtocol)
	}
	if s.transport == TransportTCP {
		if !spec.tcp {
			return nil, fmt.Errorf("%w: server refused TCP interleaving", ErrProtocol)
		}
		if spec.interleaved[0] >= 0 {
			st.channel = spec.interleaved[0]
		}
		return st, nil
	}
	if spec.tcp || spec.multicast || spec.serverPort[0] == 0 {
		st.close()
		return nil, fmt.Errorf("%w: server refused unicast UDP", ErrProtocol)
	}
	st.rtpAddr = netip.AddrPortFrom(s.peer, uint16(spec.serverPort[0]))
	st.rtcpAddr = netip.AddrPortFrom(s.peer, uint16(spec.serverPort[1]))
	if a, err := netip.ParseAddr(spec.source); err == nil {
		st.source = a.Unmap()
	}
	return st, nil
}

func (s *session) openUDP(st *sessionTrack) error {
	local := s.conn.LocalAddr().(*net.TCPAddr).IP
	for try := 0; try < 64; try++ {
		rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: local})
		if err != nil {
			return err
		}
		port := rtp.LocalAddr().(*net.UDPAddr).Port
		if port%2 != 0 || port >= 65535 {
			rtp.Close()
			continue
		}
		rtcp, err := net.ListenUDP("udp", &net.UDPAddr{IP: local, Port: port + 1})
		if err != nil {
			rtp.Close()
			continue
		}
		rtp.SetReadBuffer(s.c.cfg.UDPReadBuffer)
		st.rtp, st.rtcp = rtp, rtcp
		return nil
	}
	return errors.New("rtsp: no free UDP port pair")
}

func (st *sessionTrack) close() {
	if st.rtp != nil {
		st.rtp.Close()
		st.rtcp.Close()
	}
}

func (s *session) closeMedia() {
	for _, st := range s.media {
		st.close()
	}
}

func (s *session) play(ctx context.Context) error {
	m, err := s.do(ctx, "PLAY", s.playURL, "Range: npt=0.000-\r\n")
	if err == nil {
		err = checkStatus("PLAY", m)
	}
	return err
}

// activate runs on the reader goroutine as soon as the PLAY response is read,
// so that no media frame following it is missed.
func (s *session) activate(m *message) {
	if m.code < 200 || m.code >= 300 {
		return
	}
	if id, to := parseSessionHeader(m.get("Session")); id != "" && to > 0 {
		s.timeout = time.Duration(to) * time.Second
	}
	infos := parseRTPInfo(m.get("RTP-Info"))
	for i, st := range s.media {
		var in *rtpInfo
		for j := range infos {
			if e := lastPathElement(infos[j].url); e != "" && strings.HasSuffix(st.t.info.Control, e) {
				in = &infos[j]
				break
			}
		}
		if in == nil && len(infos) == len(s.media) {
			in = &infos[i]
		}
		if in != nil && in.hasTime {
			st.t.setClockHint(in.rtpTime)
		}
		if s.transport == TransportTCP {
			s.targets[st.channel&0xFF].Store(&channelTarget{t: st.t})
			s.targets[(st.channel+1)&0xFF].Store(&channelTarget{t: st.t, rtcp: true})
		}
	}
}

func lastPathElement(u string) string {
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		return u[i+1:]
	}
	return u
}

func (s *session) startUDP() {
	for _, st := range s.media {
		s.wg.Add(2)
		go s.udpRTP(st)
		go s.udpRTCP(st)
		// Open any stateful firewall towards the server.
		punch := [12]byte{0x80, st.t.info.PayloadType & 0x7F}
		st.rtp.WriteToUDPAddrPort(punch[:], st.rtpAddr)
		var rr [64]byte
		st.rtcp.WriteToUDPAddrPort(appendRR(rr[:0], receiverReport{}, "gaal"), st.rtcpAddr)
	}
}

func (s *session) awaitFirstPacket(ctx context.Context, since time.Time) error {
	deadline := time.NewTimer(s.c.cfg.UDPProbe)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.sawUDP.Load() || s.c.lastRTP.Load() >= since.UnixNano() {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return fmt.Errorf("%w: no UDP media received", ErrTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-s.failed:
			return s.failErr
		}
	}
}

// do performs a request, answering one authentication challenge.
func (s *session) do(ctx context.Context, method, uri, extra string) (*message, error) {
	if strings.ContainsAny(uri, " \r\n\t") {
		return nil, fmt.Errorf("%w: invalid request URI", ErrProtocol)
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readerDone:
		return nil, s.readerError()
	}
	for attempt := 0; ; attempt++ {
		m, err := s.exchange(ctx, method, uri, extra)
		if err != nil {
			return nil, err
		}
		if m.code != 401 {
			return m, nil
		}
		c := s.c
		// Credentials belong to the host they were given for, not to a redirect target.
		if (c.user == "" && c.pass == "") || !strings.EqualFold(s.base.Hostname(), c.origin.Hostname()) {
			return nil, fmt.Errorf("%w: server requires credentials", ErrAuth)
		}
		ch, ok := chooseChallenge(parseChallenges(m.values("WWW-Authenticate")), !c.cfg.DisableBasicAuth)
		if !ok {
			return nil, fmt.Errorf("%w: no usable authentication scheme offered", ErrAuth)
		}
		if attempt > 1 || (attempt == 1 && !ch.stale()) {
			return nil, fmt.Errorf("%w: credentials rejected", ErrAuth)
		}
		s.auth = &authenticator{user: c.user, pass: c.pass, ch: ch}
	}
}

func (s *session) readerError() error {
	return fmt.Errorf("rtsp: connection closed: %w", io.ErrUnexpectedEOF)
}

func (s *session) exchange(ctx context.Context, method, uri, extra string) (*message, error) {
	s.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: %s\r\n", method, uri, s.cseq, s.c.cfg.UserAgent)
	if s.auth != nil {
		fmt.Fprintf(&b, "Authorization: %s\r\n", s.auth.header(method, uri))
	}
	if s.id != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", s.id)
	}
	b.WriteString(extra)
	b.WriteString("\r\n")

	ch := make(chan *message, 1)
	s.waitMu.Lock()
	s.waitSeq, s.waitCh, s.waitHook = s.cseq, ch, nil
	if method == "PLAY" {
		s.waitHook = s.activate
	}
	s.waitMu.Unlock()
	defer func() {
		s.waitMu.Lock()
		s.waitCh, s.waitHook = nil, nil
		s.waitMu.Unlock()
	}()
	if err := s.write([]byte(b.String())); err != nil {
		return nil, err
	}
	timer := time.NewTimer(s.c.cfg.RequestTimeout)
	defer timer.Stop()
	select {
	case m := <-ch:
		return m, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: no response to %s", ErrTimeout, method)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readerDone:
		select {
		case m := <-ch:
			return m, nil
		default:
		}
		return nil, s.readerError()
	}
}

func (s *session) write(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(s.c.cfg.RequestTimeout))
	_, err := s.conn.Write(b)
	return err
}

// readLoop reads the TCP connection: RTSP messages and interleaved media.
func (s *session) readLoop() {
	defer s.wg.Done()
	defer close(s.readerDone)
	frame := make([]byte, 65536)
	garbage := 0
	for {
		b, err := s.br.Peek(1)
		if err != nil {
			s.fail(fmt.Errorf("rtsp: connection lost: %w", err))
			return
		}
		switch {
		case b[0] == '$':
			hdr, err := s.br.Peek(4)
			if err != nil {
				s.fail(fmt.Errorf("rtsp: connection lost: %w", err))
				return
			}
			ch, n := hdr[1], int(binary.BigEndian.Uint16(hdr[2:]))
			s.br.Discard(4)
			if _, err := io.ReadFull(s.br, frame[:n]); err != nil {
				s.fail(fmt.Errorf("rtsp: connection lost: %w", err))
				return
			}
			s.dispatch(ch, frame[:n], time.Now())
		case b[0] >= 'A' && b[0] <= 'Z':
			m, err := readMessage(s.br)
			if errors.Is(err, ErrProtocol) {
				if garbage += 256; garbage > maxGarbageBytes {
					s.fail(err)
					return
				}
				continue
			}
			if err != nil {
				s.fail(fmt.Errorf("rtsp: connection lost: %w", err))
				return
			}
			s.handleMessage(m)
		default:
			s.br.Discard(1)
			if garbage++; garbage > maxGarbageBytes {
				s.fail(fmt.Errorf("%w: unrecognized data on the connection", ErrProtocol))
				return
			}
		}
	}
}

func (s *session) dispatch(ch byte, data []byte, now time.Time) {
	target := s.targets[ch].Load()
	if target == nil {
		return
	}
	if target.rtcp {
		s.handleRTCP(target.t, data, now)
		return
	}
	target.t.receive(data, now)
}

func (s *session) handleRTCP(t *track, data []byte, now time.Time) {
	ev, ok := parseRTCP(data)
	if !ok {
		return
	}
	if ev.hasSR {
		t.noteSR(ev, now)
	}
	if ev.bye {
		for _, st := range s.media {
			if st.t == t && !st.gotBye.Swap(true) {
				if int(s.byes.Add(1)) == len(s.media) {
					s.fail(errEOF)
				}
			}
		}
	}
}

func (s *session) handleMessage(m *message) {
	if m.method != "" {
		// Requests from the server are politely declined.
		reply := "RTSP/1.0 501 Not Implemented\r\nCSeq: " + sanitizeHeader(m.get("CSeq")) + "\r\n\r\n"
		s.write([]byte(reply))
		return
	}
	seq := -1
	if v := m.get("CSeq"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			seq = n
		}
	}
	s.waitMu.Lock()
	ch, hook := s.waitCh, s.waitHook
	match := ch != nil && (seq < 0 || seq == s.waitSeq)
	if match {
		s.waitCh, s.waitHook = nil, nil
	}
	s.waitMu.Unlock()
	if !match {
		return
	}
	if hook != nil {
		hook(m)
	}
	ch <- m
}

func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, v)
}

func (s *session) udpRTP(st *sessionTrack) {
	defer s.wg.Done()
	buf := make([]byte, 65536)
	t := st.t
	var deadline time.Time
	for {
		now := time.Now()
		next := now.Add(500 * time.Millisecond)
		if t.held > 0 && t.deadline.Before(next) {
			next = t.deadline
		}
		if t.held > 0 || next.Sub(deadline) > 200*time.Millisecond || deadline.Before(now) {
			st.rtp.SetReadDeadline(next)
			deadline = next
		}
		n, from, err := st.rtp.ReadFromUDPAddrPort(buf)
		now = time.Now()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.expire(now)
				select {
				case <-s.stop:
					return
				default:
				}
				continue
			}
			select {
			case <-s.stop:
			default:
				s.fail(fmt.Errorf("rtsp: UDP receive: %w", err))
			}
			return
		}
		if !s.validSource(st, from) {
			continue
		}
		t.receive(buf[:n], now)
		if t.held > 0 {
			t.expire(now)
		}
	}
}

func (s *session) validSource(st *sessionTrack, from netip.AddrPort) bool {
	a := from.Addr().Unmap()
	return a == s.peer || (st.source.IsValid() && a == st.source)
}

func (s *session) udpRTCP(st *sessionTrack) {
	defer s.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, from, err := st.rtcp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if s.validSource(st, from) {
			s.sawUDP.Store(true) // RTCP alone proves that the path works
			s.handleRTCP(st.t, buf[:n], time.Now())
		}
	}
}

// housekeep sends keepalives and receiver reports and watches for stalls.
func (s *session) housekeep() {
	defer s.wg.Done()
	tick := time.NewTicker(housekeepEvery)
	defer tick.Stop()
	c := s.c
	start := time.Now()
	lastKeep, lastReport, lastLoss := start, start, start
	var lossPackets, lossLost uint64
	keepEvery := max(s.timeout/2, time.Second)
	var report []byte
	for {
		select {
		case <-s.stop:
			return
		case <-s.failed:
			return
		case <-tick.C:
		}
		now := time.Now()
		last := time.Unix(0, c.lastRTP.Load())
		if last.Before(start) {
			last = start
		}
		if now.Sub(last) > c.cfg.ReadTimeout {
			s.fail(fmt.Errorf("%w: no media for %v", ErrTimeout, c.cfg.ReadTimeout))
			return
		}
		if now.Sub(lastReport) >= reportEvery {
			lastReport = now
			for _, st := range s.media {
				report = s.sendReport(st, report[:0], now)
			}
		}
		if s.transport == TransportUDP && s.probeUDP && now.Sub(lastLoss) >= lossCheckEvery {
			lastLoss = now
			var p, l uint64
			for _, st := range s.media {
				p += st.t.stats.packets.Load()
				l += st.t.stats.lost.Load()
			}
			dp, dl := p-lossPackets, l-lossLost
			lossPackets, lossLost = p, l
			if dp+dl >= lossCheckMinimum && dl*100 >= (dp+dl)*lossFallbackPct {
				s.fail(errFallbackTCP)
				return
			}
		}
		if now.Sub(lastKeep) >= keepEvery {
			lastKeep = now
			method := "OPTIONS"
			uri := s.base.String()
			if s.useParam {
				method, uri = "GET_PARAMETER", s.playURL
			}
			ctx, cancel := context.WithTimeout(c.ctx, c.cfg.RequestTimeout)
			m, err := s.do(ctx, method, uri, "")
			cancel()
			if err == nil && m.code >= 400 && s.useParam {
				s.useParam = false // the next keepalive uses OPTIONS
				continue
			}
			if err != nil && c.ctx.Err() == nil {
				s.fail(fmt.Errorf("rtsp: keepalive: %w", err))
				return
			}
		}
	}
}

func (s *session) sendReport(st *sessionTrack, b []byte, now time.Time) []byte {
	t := st.t
	stats := &t.stats
	if stats.packets.Load() == 0 {
		return b
	}
	high, pk, lost := stats.highest.Load(), stats.packets.Load(), stats.lost.Load()
	rr := &t.rr
	var fraction uint8
	if expected := high - rr.lastHighest; rr.lastHighest != 0 && expected > 0 {
		if dl := lost - rr.lastLost; dl > 0 {
			fraction = uint8(min(dl*256/expected, 255))
		}
	}
	rr.lastHighest, rr.lastPackets, rr.lastLost = high, pk, lost
	r := receiverReport{
		ssrc: 0x67616C00 | uint32(t.index), mediaSSRC: stats.lastSSRC.Load(), fraction: fraction,
		lost: uint32(min(lost, 0x7FFFFF)), highest: uint32(high),
		jitter: uint32(float64(stats.jitterNS.Load()) * float64(t.info.ClockRate) / 1e9),
	}
	t.srMu.Lock()
	if t.hasSR {
		r.lsr = uint32(t.srNTP >> 16)
		r.dlsr = uint32(now.Sub(t.srArrival).Seconds() * 65536)
	}
	t.srMu.Unlock()
	b = appendRR(b, r, "gaal")
	if s.transport == TransportUDP {
		st.rtcp.WriteToUDPAddrPort(b, st.rtcpAddr)
		return b
	}
	head := []byte{'$', byte(st.channel + 1), byte(len(b) >> 8), byte(len(b))}
	s.write(append(head, b...))
	return b
}

// shutdown sends TEARDOWN, closes everything and waits for all goroutines.
func (s *session) shutdown() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.id != "" {
		select {
		case <-s.readerDone:
		default:
			ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
			s.do(ctx, "TEARDOWN", s.playURL, "")
			cancel()
		}
	}
	s.fail(ErrClosed)
	s.conn.Close()
	s.closeMedia()
	s.wg.Wait()
}
