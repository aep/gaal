// Package rtsp is a client for live RTSP cameras, written in pure Go.
//
// Dial connects, negotiates a transport and starts playing. Read then returns
// pooled packets that can be passed straight to a gaal decoder:
//
//	c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Username: user, Password: pass})
//	if err != nil { ... }
//	defer c.Close()
//	for {
//		p, err := c.Read(ctx)
//		if err != nil { ... }
//		// decode p.Packet, skipping to a keyframe after p.Discontinuity
//		p.Release()
//	}
//
// UDP is tried first because it has the lowest latency. TCP interleaving is
// used when UDP is refused, delivers nothing, or loses too much.
//
// Credentials never appear in errors, logs, Track.Control or Stats. After an
// authentication failure the client stops: it does not guess and does not
// retry, so it cannot lock an account.
package rtsp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aep/gaal"
)

// Errors returned by this package. Match them with errors.Is.
var (
	ErrAuth          = errors.New("rtsp: authentication failed")
	ErrClosed        = errors.New("rtsp: client closed")
	ErrProtocol      = errors.New("rtsp: protocol error")
	ErrTimeout       = errors.New("rtsp: timed out")
	ErrNoTracks      = errors.New("rtsp: no usable tracks")
	ErrInvalidURL    = errors.New("rtsp: invalid URL")
	ErrRedirects     = errors.New("rtsp: too many redirects")
	ErrStreamChanged = errors.New("rtsp: stream layout changed on reconnect")
)

// StatusError is a non-success RTSP response.
type StatusError struct {
	Method string
	Code   int
	Reason string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rtsp: %s: %d %s", e.Method, e.Code, e.Reason)
}

// Transport selects how RTP travels.
type Transport int

// Transports.
const (
	// TransportAuto tries UDP and falls back to TCP.
	TransportAuto Transport = iota
	TransportUDP
	TransportTCP
)

func (t Transport) String() string {
	switch t {
	case TransportUDP:
		return "udp"
	case TransportTCP:
		return "tcp"
	}
	return "auto"
}

// Reconnect turns on automatic reconnection. A wrong password or a changed
// stream layout is never retried.
type Reconnect struct {
	MinDelay    time.Duration // default 1 s, also the wait before the first retry
	MaxDelay    time.Duration // default 30 s
	MaxAttempts int           // per outage, 0 means unlimited
}

// EventKind identifies an Event.
type EventKind int

// Event kinds.
const (
	EventConnected EventKind = iota
	EventDisconnected
	EventTransportFallback
)

func (k EventKind) String() string {
	switch k {
	case EventConnected:
		return "connected"
	case EventDisconnected:
		return "disconnected"
	case EventTransportFallback:
		return "transport-fallback"
	}
	return "unknown"
}

// Event reports a change of connection state.
type Event struct {
	Kind      EventKind
	Transport Transport
	Err       error // redacted, may be nil
}

// Config configures a Client. Only URL is required.
type Config struct {
	// URL is rtsp://host[:port]/path. It may contain user:password.
	URL string
	// Username and Password override credentials in URL.
	Username, Password string

	Transport Transport
	// Select chooses tracks. Nil selects every supported track. Fewer tracks
	// mean less work for the camera.
	Select func(Track) bool

	DialTimeout    time.Duration // default 5 s
	RequestTimeout time.Duration // default 5 s
	// ReadTimeout ends the session when no RTP arrives for this long. Default 10 s.
	ReadTimeout time.Duration

	// ReorderWindow is the UDP reorder capacity in packets. Default 128;
	// negative turns reordering off.
	ReorderWindow int
	// ReorderDelay is the longest wait for a missing UDP packet. It only adds
	// latency while a packet is actually missing. Default 40 ms.
	ReorderDelay time.Duration
	// UDPProbe is how long TransportAuto waits for the first UDP packet. Many
	// cameras start sending at the next keyframe, so it should exceed the
	// keyframe interval. Default 5 s.
	UDPProbe time.Duration
	// UDPReadBuffer is the socket receive buffer. Default 4 MiB, subject to the
	// operating system limit.
	UDPReadBuffer int

	// QueueSize is the number of packets waiting for Read. When it is full the
	// oldest packet is dropped and the next one of its track is marked as a
	// Discontinuity. Default 128.
	QueueSize int
	// MaxUnitSize limits one access unit. Default 16 MiB.
	MaxUnitSize int

	// LowDelay tells decoders that an H.264 stream never reorders pictures, by
	// adding that statement to an SPS without VUI. It is applied only to streams
	// with picture order type 2 or a single reference frame, as cameras produce.
	// Without it a decoder may hold back up to 16 frames. A stream that reorders
	// anyway would be shown out of order, so it is off by default. Prefer
	// gaal.VideoConfig.LowDelay, which needs no rewriting and detects reordering;
	// camera.OpenRTSP uses that.
	LowDelay bool

	Reconnect *Reconnect // nil disables reconnection
	// DisableBasicAuth refuses to send the password in Basic form.
	DisableBasicAuth bool
	// TLSConfig is used for rtsps:// URLs. Nil verifies the server against the
	// system roots, with the URL's host as the server name. rtsps:// always
	// carries media interleaved over the TLS connection.
	TLSConfig *tls.Config
	UserAgent string       // default "gaal-rtsp"
	Logger    *slog.Logger // nil disables logging
	// OnEvent is called from an internal goroutine and must not block.
	OnEvent func(Event)
}

func (c Config) withDefaults() Config {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.DialTimeout, 5*time.Second)
	def(&c.RequestTimeout, 5*time.Second)
	def(&c.ReadTimeout, 10*time.Second)
	def(&c.ReorderDelay, 40*time.Millisecond)
	def(&c.UDPProbe, 5*time.Second)
	if c.ReorderWindow == 0 {
		c.ReorderWindow = 128
	}
	c.ReorderWindow = min(c.ReorderWindow, 4096)
	if c.UDPReadBuffer <= 0 {
		c.UDPReadBuffer = 4 << 20
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 128
	}
	if c.MaxUnitSize <= 0 {
		c.MaxUnitSize = 16 << 20
	}
	if c.UserAgent == "" {
		c.UserAgent = "gaal-rtsp"
	}
	if r := c.Reconnect; r != nil {
		copied := *r
		def(&copied.MinDelay, time.Second)
		def(&copied.MaxDelay, 30*time.Second)
		c.Reconnect = &copied
	}
	return c
}

// secure says whether u is rtsps://.
func secure(u *url.URL) bool { return u != nil && u.Scheme == "rtsps" }

// parseURL splits credentials from a URL. Parse errors are not wrapped, since
// they quote the input.
func parseURL(raw string) (*url.URL, string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: cannot parse", ErrInvalidURL)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "rtsp" && scheme != "rtsps" {
		return nil, "", "", fmt.Errorf("%w: scheme must be rtsp or rtsps", ErrInvalidURL)
	}
	if u.Hostname() == "" {
		return nil, "", "", fmt.Errorf("%w: no host", ErrInvalidURL)
	}
	u.Scheme = scheme
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
		u.User = nil
	}
	u.Fragment, u.RawFragment = "", ""
	return u, user, pass, nil
}

// Client receives one presentation. Read and Stats may be called concurrently
// with each other and with Close.
type Client struct {
	cfg        Config
	scrub      *scrubber
	user, pass string
	log        *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	origin    *url.URL // as configured; redirects start over from here
	url       *url.URL
	tracks    []*track
	transport Transport
	sessions  int
	closeErr  error

	queue    chan *Packet
	pool     packetPool
	lastRTP  atomic.Int64
	finished chan struct{}
	timer    *time.Timer
	timerMu  sync.Mutex
	once     sync.Once
}

// Dial connects and starts playing. ctx bounds only this call; after it returns
// use Close to stop.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	u, user, pass, err := parseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.Username != "" || cfg.Password != "" {
		user, pass = cfg.Username, cfg.Password
	}
	cfg.URL = ""
	cfg.Username, cfg.Password = "", ""
	c := &Client{
		cfg:      cfg.withDefaults(),
		scrub:    newScrubber(user, pass),
		user:     user,
		pass:     pass,
		origin:   u,
		url:      u,
		finished: make(chan struct{}),
	}
	c.log = c.cfg.Logger
	c.queue = make(chan *Packet, c.cfg.QueueSize)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s, err := c.open(ctx, c.cfg.Transport, true)
	if err != nil {
		c.cancel()
		close(c.finished)
		return nil, c.scrub.wrap(err)
	}
	go c.supervise(s)
	return c, nil
}

func (c *Client) logf(msg string, args ...any) {
	if c.log != nil {
		for i, a := range args {
			switch v := a.(type) {
			case error:
				args[i] = c.scrub.text(v.Error())
			case string:
				args[i] = c.scrub.text(v)
			}
		}
		c.log.Info(msg, args...)
	}
}

func (c *Client) event(k EventKind, t Transport, err error) {
	if c.cfg.OnEvent != nil {
		c.cfg.OnEvent(Event{Kind: k, Transport: t, Err: c.scrub.wrap(err)})
	}
}

// open starts a session, trying the transports the mode allows.
func (c *Client) open(ctx context.Context, mode Transport, first bool) (*session, error) {
	if secure(c.currentURL()) {
		mode = TransportTCP // RTP over UDP would bypass TLS
	}
	order := []Transport{mode}
	if mode == TransportAuto {
		order = []Transport{TransportUDP, TransportTCP}
	}
	var last error
	for i, tr := range order {
		s, err := c.openSession(ctx, tr, mode == TransportAuto, first)
		if err == nil {
			c.mu.Lock()
			c.transport = tr
			c.mu.Unlock()
			c.logf("rtsp: playing", "url", RedactURL(c.currentURL().String()), "transport", tr.String())
			c.event(EventConnected, tr, nil)
			return s, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fatalForFallback(err) {
			return nil, err
		}
		if i+1 < len(order) {
			c.logf("rtsp: transport failed, falling back", "transport", tr.String(), "error", err)
			c.event(EventTransportFallback, order[i+1], err)
		}
	}
	return nil, last
}

// addSessions adjusts the count of successful connections.
func (c *Client) addSessions(n int) {
	c.mu.Lock()
	c.sessions += n
	c.mu.Unlock()
}

func fatalForFallback(err error) bool {
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrInvalidURL) || errors.Is(err, ErrRedirects) ||
		errors.Is(err, ErrNoTracks) || errors.Is(err, ErrStreamChanged) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Method == "DESCRIBE"
	}
	return false
}

func (c *Client) currentURL() *url.URL {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

func (c *Client) supervise(s *session) {
	defer close(c.finished)
	mode := c.cfg.Transport
	for {
		err := s.wait(c.ctx)
		closing := c.ctx.Err() != nil
		s.shutdown()
		if closing {
			c.finish(ErrClosed)
			return
		}
		fallback := errors.Is(err, errFallbackTCP)
		c.logf("rtsp: session ended", "error", err)
		c.event(EventDisconnected, s.transport, err)
		if fallback {
			mode = TransportTCP
			c.event(EventTransportFallback, TransportTCP, nil)
		} else if c.cfg.Reconnect == nil || !retryable(err) {
			c.finish(err)
			return
		}
		next, err := c.reconnect(mode, fallback)
		if err != nil {
			c.finish(err)
			return
		}
		s = next
	}
}

func retryable(err error) bool {
	return !errors.Is(err, ErrAuth) && !errors.Is(err, ErrStreamChanged) &&
		!errors.Is(err, ErrInvalidURL) && !errors.Is(err, ErrRedirects)
}

func (c *Client) reconnect(mode Transport, immediate bool) (*session, error) {
	r := c.cfg.Reconnect
	if r == nil {
		r = &Reconnect{MinDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 1}
	}
	delay := r.MinDelay
	for attempt := 1; ; attempt++ {
		if !immediate || attempt > 1 {
			wait := delay/2 + rand.N(delay/2+1)
			select {
			case <-time.After(wait):
			case <-c.ctx.Done():
				return nil, ErrClosed
			}
			delay = min(delay*2, r.MaxDelay)
		}
		s, err := c.open(c.ctx, mode, false)
		if err == nil {
			return s, nil
		}
		if c.ctx.Err() != nil {
			return nil, ErrClosed
		}
		c.logf("rtsp: reconnect failed", "attempt", attempt, "error", err)
		if !retryable(err) || (r.MaxAttempts > 0 && attempt >= r.MaxAttempts) {
			return nil, err
		}
	}
}

func (c *Client) finish(err error) {
	c.mu.Lock()
	c.closeErr = c.scrub.wrap(err)
	c.mu.Unlock()
}

func (c *Client) enqueue(p *Packet) {
	for {
		select {
		case c.queue <- p:
			return
		default:
		}
		select {
		case old := <-c.queue:
			t := c.tracks[old.Track]
			t.stats.queueDrops.Add(1)
			t.discont.Store(true)
			old.Release()
		default:
		}
	}
}

// Read returns the next packet. The caller must Release it. After the session
// ends, queued packets are returned first and then the terminal error.
func (c *Client) Read(ctx context.Context) (*Packet, error) {
	select {
	case p := <-c.queue:
		return p, nil
	default:
	}
	select {
	case p := <-c.queue:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.finished:
		return c.drained()
	}
}

// TryRead returns a queued packet without waiting, or nil.
func (c *Client) TryRead() *Packet {
	select {
	case p := <-c.queue:
		return p
	default:
		return nil
	}
}

// ReadTimeout is Read with a timeout, and does not allocate. It returns an
// error matching ErrTimeout when nothing arrived.
func (c *Client) ReadTimeout(d time.Duration) (*Packet, error) {
	select {
	case p := <-c.queue:
		return p, nil
	default:
	}
	c.timerMu.Lock()
	defer c.timerMu.Unlock()
	if c.timer == nil {
		c.timer = time.NewTimer(d)
	} else {
		c.timer.Reset(d)
	}
	defer c.timer.Stop()
	select {
	case p := <-c.queue:
		return p, nil
	case <-c.timer.C:
		return nil, ErrTimeout
	case <-c.finished:
		return c.drained()
	}
}

func (c *Client) drained() (*Packet, error) {
	select {
	case p := <-c.queue:
		return p, nil
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr == nil {
		return nil, ErrClosed
	}
	return nil, c.closeErr
}

// Tracks describes the tracks being received.
func (c *Client) Tracks() []Track {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Track, len(c.tracks))
	for i, t := range c.tracks {
		out[i] = t.info
	}
	return out
}

// URL returns the current server URL without credentials.
func (c *Client) URL() string { return RedactURL(c.currentURL().String()) }

// Close sends TEARDOWN, stops all goroutines and releases queued packets.
// Packets the caller still holds stay valid until released.
func (c *Client) Close() error {
	c.cancel()
	<-c.finished
	c.once.Do(func() {
		for {
			select {
			case p := <-c.queue:
				p.Release()
			default:
				return
			}
		}
	})
	return nil
}

// TrackStats counts what one track received.
type TrackStats struct {
	Track            int
	Codec            gaal.Codec
	SSRC             uint32
	PacketsReceived  uint64
	BytesReceived    uint64
	PacketsLost      uint64 // never arrived in time
	PacketsLate      uint64 // arrived after being given up on
	PacketsDuplicate uint64
	PacketsReordered uint64 // arrived ahead of a missing packet
	PacketsMalformed uint64
	Units            uint64 // access units or audio frames delivered
	UnitsDropped     uint64 // incomplete units discarded
	QueueDrops       uint64 // units discarded because Read fell behind
	// Jitter is the RFC 3550 interarrival jitter over packets. Video packets of
	// one frame share a timestamp, so it includes the frame's send time.
	Jitter time.Duration
	// UnitJitter is the same estimate over completed units.
	UnitJitter time.Duration
}

// Stats is a snapshot of the client.
type Stats struct {
	URL       string // without credentials
	Transport Transport
	Sessions  int // successful connections so far
	Tracks    []TrackStats
}

// Stats returns current counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Stats{URL: RedactURL(c.url.String()), Transport: c.transport, Sessions: c.sessions}
	for _, t := range c.tracks {
		s := &t.stats
		out.Tracks = append(out.Tracks, TrackStats{
			Track: t.index, Codec: t.info.Codec, SSRC: s.lastSSRC.Load(),
			PacketsReceived: s.packets.Load(), BytesReceived: s.bytes.Load(), PacketsLost: s.lost.Load(),
			PacketsLate: s.late.Load(), PacketsDuplicate: s.duplicate.Load(), PacketsReordered: s.reordered.Load(),
			PacketsMalformed: s.malformed.Load(), Units: s.units.Load(), UnitsDropped: s.unitsDropped.Load(),
			QueueDrops: s.queueDrops.Load(), Jitter: time.Duration(s.jitterNS.Load()),
			UnitJitter: time.Duration(s.unitJitterNS.Load()),
		})
	}
	return out
}

var errEOF = fmt.Errorf("rtsp: server ended the stream: %w", io.EOF)
