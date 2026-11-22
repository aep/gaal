package rtsp

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/aep/gaal"
)

// Packet is one access unit or audio frame. The embedded gaal.Packet can be
// passed directly to a decoder: H.264 and H.265 are Annex B with one access
// unit per packet, and parameter sets precede every keyframe. Call Release when
// finished, so the receive path does not allocate.
type Packet struct {
	gaal.Packet

	Track int
	Codec gaal.Codec
	// Discontinuity says data before this packet was lost, or that this is the
	// first packet of a connection. A video decoder should skip packets until
	// one has Keyframe set.
	Discontinuity bool
	// Arrival is when the last RTP packet of this unit was read from the network.
	Arrival time.Time
	// Wallclock is the sender's capture time from RTCP. It is zero until the
	// first sender report, and only as accurate as the sender's clock.
	Wallclock    time.Time
	RTPTimestamp uint32

	refs  atomic.Int32
	pool  *packetPool
	store []byte
}

// Retain adds a reference, which needs a matching Release.
func (p *Packet) Retain() { p.refs.Add(1) }

// Release drops a reference. The packet must not be used after its last one.
func (p *Packet) Release() {
	if p.refs.Add(-1) == 0 && p.pool != nil {
		p.pool.put(p)
	}
}

const (
	maxPooledPackets = 512
	maxPooledLarge   = 8
	largeBuffer      = 4 << 20
)

type packetPool struct {
	mu    sync.Mutex
	free  []*Packet
	large int
}

func (l *packetPool) get() *Packet {
	l.mu.Lock()
	var p *Packet
	if n := len(l.free); n > 0 {
		p = l.free[n-1]
		l.free[n-1] = nil
		l.free = l.free[:n-1]
		if cap(p.store) > largeBuffer {
			l.large--
		}
	}
	l.mu.Unlock()
	if p == nil {
		p = &Packet{pool: l}
	}
	p.Packet = gaal.Packet{}
	p.Discontinuity = false
	p.Wallclock = time.Time{}
	p.refs.Store(1)
	return p
}

func (l *packetPool) put(p *Packet) {
	l.mu.Lock()
	big := cap(p.store) > largeBuffer
	if len(l.free) < maxPooledPackets && (!big || l.large < maxPooledLarge) {
		if l.free == nil {
			l.free = make([]*Packet, 0, maxPooledPackets)
		}
		l.free = append(l.free, p)
		if big {
			l.large++
		}
	}
	l.mu.Unlock()
}

// unitBuilder assembles one output unit in a pooled packet. It keeps free room
// in front, so parameter sets can be inserted without moving the unit.
type unitBuilder struct {
	t        *track
	p        *Packet
	off, end int
}

func (b *unitBuilder) begin(headroom int) {
	if b.p == nil {
		b.p = b.t.c.pool.get()
	}
	if len(b.p.store) < headroom {
		b.p.store = make([]byte, max(headroom, 4096))
	}
	b.off, b.end = headroom, headroom
}

func (b *unitBuilder) size() int { return b.end - b.off }

// room makes space for n more bytes. It fails past the unit size limit.
func (b *unitBuilder) room(n int) bool {
	need := b.end + n
	if need-b.off > b.t.c.cfg.MaxUnitSize {
		return false
	}
	if need > len(b.p.store) {
		grown := make([]byte, need+need/2+1024)
		copy(grown[b.off:], b.p.store[b.off:b.end])
		b.p.store = grown
	}
	return true
}

func (b *unitBuilder) write(d []byte) bool {
	if !b.room(len(d)) {
		return false
	}
	b.end += copy(b.p.store[b.end:], d)
	return true
}

func (b *unitBuilder) bytes() []byte { return b.p.store[b.off:b.end] }

// prepend puts the parts in front of the unit.
func (b *unitBuilder) prepend(parts *[3][]byte) bool {
	n := len(parts[0]) + len(parts[1]) + len(parts[2])
	if n > b.off {
		if !b.room(n) {
			return false
		}
		size := b.size()
		copy(b.p.store[n:], b.p.store[b.off:b.end])
		b.off, b.end = n, n+size
	}
	at := b.off - n
	b.off = at
	for _, part := range parts {
		at += copy(b.p.store[at:], part)
	}
	return true
}

// take hands over the finished packet.
func (b *unitBuilder) take() *Packet {
	p := b.p
	b.p = nil
	p.Data = p.store[b.off:b.end:b.end]
	return p
}

// discard forgets the unit and keeps its buffer for the next one.
func (b *unitBuilder) discard() { b.off, b.end = 0, 0 }
