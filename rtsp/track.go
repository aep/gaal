package rtsp

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/aep/gaal"
)

const (
	maxDropout    = 3000 // forward sequence jump treated as a stream restart
	maxLateRun    = 8    // consecutive old packets that mean the sender restarted
	clockHintSpan = 10   // seconds an RTP-Info rtptime may be from the first packet
)

type trackCounters struct {
	packets, bytes, lost, late, duplicate, reordered, malformed atomic.Uint64
	units, unitsDropped, queueDrops                             atomic.Uint64
	jitterNS, unitJitterNS                                      atomic.Int64
	highest                                                     atomic.Uint64 // extended sequence number
	lastSSRC                                                    atomic.Uint32
}

// rtpClock turns RTP timestamps into a continuous duration.
type rtpClock struct {
	rate    int64
	started bool
	last    uint32
	ext     int64
	base    int64
	offset  time.Duration
	maxPTS  time.Duration
	hint    uint32
	hasHint bool
}

func scaleTicks(d, rate int64) time.Duration {
	return time.Duration(d/rate)*time.Second + time.Duration(d%rate)*time.Second/time.Duration(rate)
}

func (c *rtpClock) pts(ts uint32) time.Duration {
	if !c.started {
		c.started, c.last, c.ext, c.base = true, ts, 0, 0
		if c.hasHint {
			if d := int64(int32(ts - c.hint)); d >= 0 && d < clockHintSpan*c.rate {
				c.base = -d
			}
		}
	}
	c.ext += int64(int32(ts - c.last))
	c.last = ts
	p := c.offset + scaleTicks(c.ext-c.base, c.rate)
	c.maxPTS = max(c.maxPTS, p)
	return p
}

// restart continues the timeline after a gap of the given length.
func (c *rtpClock) restart(gap time.Duration) {
	c.offset = c.maxPTS + max(gap, 0)
	c.started, c.hasHint = false, false
}

type reorderSlot struct {
	used    bool
	h       rtpHeader
	arrival time.Time
	data    []byte
}

// track holds the receive state of one stream. All fields below the counters
// belong to the goroutine reading RTP, except where noted.
type track struct {
	c     *Client
	index int
	info  Track
	build func(*track) depacketizer

	stats   trackCounters
	discont atomic.Bool

	depack     depacketizer
	clock      rtpClock
	seqStarted bool
	next       uint16
	cycles     uint64
	lastSeq    uint16
	ssrc       uint32
	hasSSRC    bool
	lateRun    int

	slots    []reorderSlot // empty for TCP
	held     int
	deadline time.Time
	delay    time.Duration

	// RFC 3550 interarrival jitter, in RTP units.
	jitter, unitJitter         float64
	lastTransit, lastUnitTrans float64
	hasTransit, hasUnitTrans   bool
	epoch                      time.Time
	lastUnitArrival            time.Time

	srMu      sync.Mutex
	srNTP     uint64
	srRTP     uint32
	srArrival time.Time
	hasSR     bool

	rr rrState // owned by the session housekeeping goroutine
}

type rrState struct {
	lastHighest, lastPackets, lastLost uint64
}

// startSession prepares for a new connection. Nothing is reading at this point.
func (t *track) startSession(window int, delay time.Duration, now time.Time) {
	if t.depack == nil {
		t.depack = t.build(t)
	}
	t.depack.reset()
	t.clock.rate = int64(t.info.ClockRate)
	if t.clock.started || t.clock.maxPTS > 0 {
		t.clock.restart(now.Sub(t.lastUnitArrival))
	}
	t.seqStarted, t.hasSSRC, t.lateRun, t.held = false, false, 0, 0
	t.hasTransit, t.hasUnitTrans = false, false
	t.jitter, t.unitJitter = 0, 0
	t.epoch = now
	t.delay = delay
	t.slots = nil
	if window > 0 {
		size := 1
		for size < window {
			size <<= 1
		}
		t.slots = make([]reorderSlot, size)
	}
	t.rr = rrState{}
	t.srMu.Lock()
	t.hasSR = false
	t.srMu.Unlock()
	t.discont.Store(true)
}

func (t *track) setClockHint(ts uint32) { t.clock.hint, t.clock.hasHint = ts, true }

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// receive handles one raw RTP packet.
func (t *track) receive(pkt []byte, now time.Time) {
	h, payload, ok := parseRTP(pkt)
	if !ok {
		t.stats.malformed.Add(1)
		return
	}
	if h.pt != t.info.PayloadType {
		return
	}
	t.stats.packets.Add(1)
	t.stats.bytes.Add(uint64(len(pkt)))
	t.c.lastRTP.Store(now.UnixNano())
	if !t.hasSSRC || h.ssrc != t.ssrc {
		if t.hasSSRC {
			t.resync()
		}
		t.ssrc, t.hasSSRC = h.ssrc, true
		t.stats.lastSSRC.Store(h.ssrc)
	}
	t.updateJitter(h.ts, now)
	if !t.seqStarted {
		t.seqStarted, t.next, t.lastSeq = true, h.seq, h.seq
		t.stats.highest.Store(t.cycles<<16 | uint64(h.seq))
	}
	if d := int16(h.seq - t.lastSeq); d > 0 {
		if h.seq < t.lastSeq {
			t.cycles++
		}
		t.lastSeq = h.seq
		t.stats.highest.Store(t.cycles<<16 | uint64(h.seq))
	}
	d := int(int16(h.seq - t.next))
	switch {
	case d == 0:
		t.lateRun = 0
		t.advance(h, payload, now)
		t.drain()
	case d < 0:
		t.old(h, payload, now)
	default:
		t.lateRun = 0
		t.ahead(h, payload, d, now)
	}
}

func (t *track) old(h rtpHeader, payload []byte, now time.Time) {
	t.stats.late.Add(1)
	t.lateRun++
	if t.lateRun >= maxLateRun {
		t.resync()
		t.seqStarted, t.next, t.lastSeq = true, h.seq, h.seq
		t.advance(h, payload, now)
	}
}

// resync forgets the sequence state after the sender restarted.
func (t *track) resync() {
	t.clearSlots()
	t.lateRun = 0
	t.seqStarted = false
	t.hasTransit, t.hasUnitTrans = false, false
	t.discont.Store(true)
	t.depack.lost()
}

func (t *track) clearSlots() {
	if t.held == 0 {
		return
	}
	for i := range t.slots {
		t.slots[i].used = false
	}
	t.held = 0
}

func (t *track) ahead(h rtpHeader, payload []byte, d int, now time.Time) {
	size := len(t.slots)
	if size == 0 || d >= maxDropout {
		// No reordering (TCP), or a jump too large to wait for.
		t.flushHeld()
		gap := int(int16(h.seq - t.next))
		t.noteLost(gap)
		t.next = h.seq
		t.advance(h, payload, now)
		t.drain()
		return
	}
	if d >= size {
		t.skipTo(h.seq - uint16(size) + 1)
		d = int(int16(h.seq - t.next))
		if d == 0 {
			t.advance(h, payload, now)
			t.drain()
			return
		}
	}
	s := &t.slots[int(h.seq)&(size-1)]
	if s.used {
		t.stats.duplicate.Add(1)
		return
	}
	s.used, s.h, s.arrival = true, h, now
	s.data = append(s.data[:0], payload...)
	t.stats.reordered.Add(1)
	if t.held == 0 {
		t.deadline = now.Add(t.delay)
	}
	t.held++
}

func (t *track) advance(h rtpHeader, payload []byte, arrival time.Time) {
	t.next = h.seq + 1
	t.depack.push(h, payload, arrival)
}

// drain delivers buffered packets that are now in order.
func (t *track) drain() {
	for t.held > 0 {
		s := &t.slots[int(t.next)&(len(t.slots)-1)]
		if !s.used || s.h.seq != t.next {
			return
		}
		s.used = false
		t.held--
		t.advance(s.h, s.data, s.arrival)
	}
}

func (t *track) noteLost(n int) {
	if n <= 0 {
		return
	}
	t.stats.lost.Add(uint64(n))
	t.discont.Store(true)
	t.depack.lost()
}

// skipTo gives up on everything before seq, delivering what was held.
func (t *track) skipTo(seq uint16) {
	missing := 0
	for int16(seq-t.next) > 0 {
		if t.held == 0 {
			missing += int(int16(seq - t.next))
			t.next = seq
			break
		}
		s := &t.slots[int(t.next)&(len(t.slots)-1)]
		if s.used && s.h.seq == t.next {
			t.noteLost(missing)
			missing = 0
			s.used = false
			t.held--
			t.advance(s.h, s.data, s.arrival)
			continue
		}
		missing++
		t.next++
	}
	t.noteLost(missing)
	t.drain()
}

// flushHeld delivers all held packets, counting the gaps between them.
func (t *track) flushHeld() {
	for t.held > 0 {
		t.skipOneGap()
	}
}

// skipOneGap moves to the oldest held packet and delivers from there.
func (t *track) skipOneGap() {
	var target uint16
	best := 1 << 30
	for i := range t.slots {
		if s := &t.slots[i]; s.used {
			if d := int(uint16(s.h.seq - t.next)); d < best {
				best, target = d, s.h.seq
			}
		}
	}
	if best == 1<<30 {
		t.held = 0
		return
	}
	t.noteLost(best)
	t.next = target
	t.drain()
}

// expire declares packets lost once they were waited for long enough.
func (t *track) expire(now time.Time) {
	for t.held > 0 && !now.Before(t.deadline) {
		t.skipOneGap()
		if t.held > 0 {
			oldest := now
			for i := range t.slots {
				if s := &t.slots[i]; s.used && s.arrival.Before(oldest) {
					oldest = s.arrival
				}
			}
			t.deadline = oldest.Add(t.delay)
		}
	}
}

func (t *track) updateJitter(ts uint32, now time.Time) {
	rate := float64(t.info.ClockRate)
	transit := now.Sub(t.epoch).Seconds()*rate - float64(ts)
	if t.hasTransit {
		d := wrapTicks(transit - t.lastTransit)
		t.jitter += (abs64(d) - t.jitter) / 16
		t.stats.jitterNS.Store(int64(t.jitter / rate * 1e9))
	}
	t.lastTransit, t.hasTransit = transit, true
}

// wrapTicks undoes a 32-bit timestamp wrap in a transit difference.
func wrapTicks(d float64) float64 {
	const span = 1 << 32
	switch {
	case d > span/2:
		return d - span
	case d < -span/2:
		return d + span
	}
	return d
}

func (t *track) noteSR(ev rtcpEvents, now time.Time) {
	t.srMu.Lock()
	t.srNTP, t.srRTP, t.srArrival, t.hasSR = ev.srNTP, ev.srRTP, now, true
	t.srMu.Unlock()
}

func (t *track) wallclock(ts uint32) time.Time {
	t.srMu.Lock()
	ntp, rtp, ok := t.srNTP, t.srRTP, t.hasSR
	t.srMu.Unlock()
	if !ok {
		return time.Time{}
	}
	return ntpToTime(ntp).Add(scaleSigned(int64(int32(ts-rtp)), int64(t.info.ClockRate)))
}

func scaleSigned(d, rate int64) time.Duration {
	if d < 0 {
		return -scaleTicks(-d, rate)
	}
	return scaleTicks(d, rate)
}

// emit finishes a unit and queues it.
func (t *track) emit(p *Packet, ts uint32, key bool, arrival time.Time) {
	p.PTS = t.clock.pts(ts)
	p.Keyframe = key
	p.Track, p.Codec = t.index, t.info.Codec
	p.Arrival, p.RTPTimestamp = arrival, ts
	p.Discontinuity = t.discont.Swap(false)
	p.Wallclock = t.wallclock(ts)
	t.lastUnitArrival = arrival
	t.stats.units.Add(1)
	rate := float64(t.info.ClockRate)
	transit := arrival.Sub(t.epoch).Seconds()*rate - float64(ts)
	if t.hasUnitTrans {
		d := wrapTicks(transit - t.lastUnitTrans)
		t.unitJitter += (abs64(d) - t.unitJitter) / 16
		t.stats.unitJitterNS.Store(int64(t.unitJitter / rate * 1e9))
	}
	t.lastUnitTrans, t.hasUnitTrans = transit, true
	t.c.enqueue(p)
}

// dropUnit records a unit that could not be delivered intact.
func (t *track) dropUnit() {
	t.stats.unitsDropped.Add(1)
	t.discont.Store(true)
}

// setSize records a picture size found in the stream.
func (t *track) setSize(w, h int) {
	t.c.mu.Lock()
	t.info.Width, t.info.Height = w, h
	t.c.mu.Unlock()
}

// MediaType separates video and audio tracks.
type MediaType int

// Media types.
const (
	Video MediaType = iota
	Audio
)

func (m MediaType) String() string {
	if m == Audio {
		return "audio"
	}
	return "video"
}

// Track describes one stream of the presentation.
type Track struct {
	Index       int
	Media       MediaType
	Codec       gaal.Codec
	PayloadType uint8
	ClockRate   int
	Channels    int // audio only
	// Width and Height come from SDP or the stream, and are 0 until known.
	Width, Height int
	FrameRate     float64 // from SDP, 0 if absent
	// Control is the track URL, without credentials.
	Control string
	// Format is the RTP encoding name, such as "H264" or "MPEG4-GENERIC".
	Format string
	// ParameterSets holds SPS, PPS and VPS NAL units from SDP, without start codes.
	ParameterSets [][]byte
	// Extra is the AudioSpecificConfig of an AAC track.
	Extra []byte
}

// VideoConfig returns the decoder configuration of a video track.
func (t Track) VideoConfig() gaal.VideoConfig { return gaal.VideoConfig{Codec: t.Codec} }

// AudioConfig returns the decoder configuration of an audio track.
func (t Track) AudioConfig() gaal.AudioConfig {
	rate := t.ClockRate
	if t.Codec == gaal.AAC {
		if r := aacConfigRate(t.Extra); r > 0 {
			rate = r
		}
	}
	return gaal.AudioConfig{Codec: t.Codec, SampleRate: rate, Channels: t.Channels, Extra: t.Extra}
}
