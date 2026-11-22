package display

import (
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aep/gaal"
)

const maxBuffers = 8

// layout is everything about a frame that decides how it is displayed.
type layout struct {
	width, height int
	format        gaal.PixelFormat
	matrix        Matrix // BT601 or BT709, never MatrixAuto
	full          bool   // full range
}

func (l layout) coefs() *coefs { return coefsFor(l.matrix, l.full) }

var clockBase = time.Now()

// now is the time in nanoseconds on the clock used for all statistics.
func now() int64 { return int64(time.Since(clockBase)) }

// engine implements latest-wins presentation over a fixed set of buffers. A
// buffer is free, being written by Show, pending, on its way to the display, or
// on screen.
type engine struct {
	b      backend
	opts   Options
	events eventQueue

	showMu sync.Mutex // serializes Show, so one buffer is written at a time
	cur    layout
	ready  bool        // cur is valid
	stale  atomic.Bool // the backend wants prepare called again

	mu        sync.Mutex
	closed    atomic.Bool
	free      uint32
	next      uint32 // where acquire looks first
	pending   int
	pendAt    int64 // when the pending frame entered Show
	inFlight  int   // buffer being presented, or -1
	flightAt  int64
	front     int // buffer on screen, or -1
	busy      bool
	preparing bool
	selfFree  bool // the backend frees buffers itself with release
	gen       uint32
	winW      int
	winH      int
	s         Stats
	showTotal time.Duration
	showCount uint64
	latTotal  time.Duration
	latCount  uint64
	subErr    error
}

func (e *engine) init(o Options) {
	e.opts = o
	e.pending, e.inFlight, e.front = -1, -1, -1
	e.winW, e.winH = o.Width, o.Height
	e.events.ready = make(chan struct{}, 1)
}

func checkFrame(f *gaal.VideoFrame) error {
	if f == nil {
		return fmt.Errorf("%w: nil frame", gaal.ErrInvalid)
	}
	w, h := f.Width, f.Height
	cw, ch := (w+1)/2, (h+1)/2
	bad := w <= 0 || h <= 0 || f.YStride < w || len(f.Y) < f.YStride*(h-1)+w
	if !bad {
		switch f.Format {
		case gaal.NV12:
			bad = f.UVStride < 2*cw || len(f.U) < f.UVStride*(ch-1)+2*cw
		case gaal.I420:
			bad = f.UVStride < cw || len(f.U) < f.UVStride*(ch-1)+cw || len(f.V) < f.UVStride*(ch-1)+cw
		default:
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("%w: malformed %dx%d frame", gaal.ErrInvalid, w, h)
	}
	return nil
}

func (e *engine) layoutOf(f *gaal.VideoFrame) layout {
	m := e.opts.Matrix
	if m == MatrixAuto {
		m = BT601
		if f.Width > 1024 || f.Height > 576 {
			m = BT709
		}
	}
	return layout{f.Width, f.Height, f.Format, m, f.Range == gaal.RangeFull}
}

func (e *engine) show(f *gaal.VideoFrame) error {
	start := now()
	if err := checkFrame(f); err != nil {
		return err
	}
	e.showMu.Lock()
	defer e.showMu.Unlock()
	if e.closed.Load() {
		return ErrClosed
	}
	if l := e.layoutOf(f); !e.ready || l != e.cur || e.stale.Swap(false) {
		// Nothing may be submitted while the backend replaces its buffers.
		e.mu.Lock()
		e.preparing = true
		e.mu.Unlock()
		n, err := e.b.prepare(l)
		e.mu.Lock()
		e.preparing = false
		if err != nil {
			e.ready = false
			e.pending = -1
			e.mu.Unlock()
			return err
		}
		e.resetBuffers(n)
		e.mu.Unlock()
		e.cur, e.ready = l, true
	}
	e.mu.Lock()
	i := e.acquire()
	err := e.subErr
	e.subErr = nil
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if i < 0 {
		e.mu.Lock()
		e.s.Dropped++
		e.s.Shown++
		e.noteShow(start)
		e.mu.Unlock()
		return nil
	}
	e.b.fill(i, f)
	e.mu.Lock()
	if e.pending >= 0 { // a concurrent release cannot add one, but be safe
		e.free |= 1 << e.pending
		e.s.Dropped++
	}
	e.pending, e.pendAt = i, start
	e.s.Shown++
	e.pump()
	err = e.subErr
	e.subErr = nil
	e.noteShow(start)
	e.mu.Unlock()
	return err
}

func (e *engine) noteShow(start int64) {
	d := time.Duration(now() - start)
	e.s.ShowLast = d
	e.s.ShowMax = max(e.s.ShowMax, d)
	e.showTotal += d
	e.showCount++
}

// resetBuffers forgets all buffers after prepare replaced them. The present
// that is under way, if any, still completes through frameDone.
func (e *engine) resetBuffers(n int) {
	if e.pending >= 0 {
		e.s.Dropped++
	}
	e.free = uint32(1)<<n - 1
	e.pending, e.inFlight, e.front = -1, -1, -1
	e.next = 0
	e.gen++
}

// acquire returns a buffer to write. With none free it takes back the pending
// frame, which the new one would replace anyway.
func (e *engine) acquire() int {
	if e.free != 0 {
		// Take buffers in turn, so the one used longest ago is reused first.
		i := bits.TrailingZeros32(e.free >> e.next << e.next)
		if i >= 32 {
			i = bits.TrailingZeros32(e.free)
		}
		e.free &^= 1 << i
		e.next = uint32(i+1) % maxBuffers
		return i
	}
	if i := e.pending; i >= 0 {
		e.pending = -1
		e.s.Dropped++
		return i
	}
	return -1
}

// pump hands the pending frame to the backend if the display can take one.
func (e *engine) pump() {
	if e.busy || e.preparing || e.pending < 0 || e.closed.Load() {
		return
	}
	i := e.pending
	e.pending = -1
	e.inFlight, e.flightAt = i, e.pendAt
	e.busy = true
	if err := e.b.submit(i); err != nil {
		e.busy = false
		e.inFlight = -1
		e.free |= 1 << i
		e.subErr = err
	}
}

// invalidate makes the next Show call prepare again.
func (e *engine) invalidate() { e.stale.Store(true) }

// fail makes the next Show return err.
func (e *engine) fail(err error) {
	e.mu.Lock()
	e.subErr = err
	e.mu.Unlock()
}

// generation identifies the current buffer set. Backends pass it to release so
// late notifications about replaced buffers are ignored.
func (e *engine) generation() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gen
}

// frameDone tells the engine the display can take another frame. Unless the
// backend frees buffers itself, the previous front buffer becomes free and the
// presented one becomes the front.
func (e *engine) frameDone() {
	e.mu.Lock()
	e.busy = false
	if i := e.inFlight; i >= 0 {
		e.inFlight = -1
		if !e.selfFree {
			if e.front >= 0 && e.front != i {
				e.free |= 1 << e.front
			}
			e.front = i
		}
	}
	e.pump()
	e.mu.Unlock()
}

// release returns a buffer the display has stopped using.
func (e *engine) release(gen uint32, i int) {
	e.mu.Lock()
	if gen == e.gen {
		e.free |= 1 << i
	}
	e.mu.Unlock()
}

// redraw presents the front buffer again, after the window changed.
func (e *engine) redraw() {
	e.mu.Lock()
	if !e.busy && !e.preparing && e.pending < 0 && e.front >= 0 && !e.closed.Load() {
		e.busy = true
		e.inFlight, e.flightAt = e.front, -1
		if err := e.b.submit(e.front); err != nil {
			e.busy = false
			e.inFlight = -1
		}
	}
	e.mu.Unlock()
}

// submittedAt returns when the frame now being submitted entered Show, or -1
// for a redraw. It must be called from submit.
func (e *engine) submittedAt() int64 { return e.flightAt }

// presented records that the frame which entered Show at shownAt reached the
// display at time at. A negative shownAt is a redraw and is not counted.
func (e *engine) presented(shownAt, at int64) {
	if shownAt < 0 {
		return
	}
	e.mu.Lock()
	e.s.Presented++
	d := time.Duration(max(at-shownAt, 0))
	e.s.LatencyLast = d
	e.s.LatencyMax = max(e.s.LatencyMax, d)
	e.latTotal += d
	e.latCount++
	e.mu.Unlock()
}

// discarded records a submitted frame that never reached the screen.
func (e *engine) discarded() {
	e.mu.Lock()
	e.s.Dropped++
	e.mu.Unlock()
}

func (e *engine) setRefresh(d time.Duration) {
	e.mu.Lock()
	e.s.Refresh = d
	e.mu.Unlock()
}

func (e *engine) stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.s
	if e.showCount > 0 {
		s.ShowAvg = e.showTotal / time.Duration(e.showCount)
	}
	if e.latCount > 0 {
		s.LatencyAvg = e.latTotal / time.Duration(e.latCount)
	}
	return s
}

func (e *engine) resetStats() {
	e.mu.Lock()
	e.s = Stats{Refresh: e.s.Refresh}
	e.showTotal, e.showCount, e.latTotal, e.latCount = 0, 0, 0, 0
	e.mu.Unlock()
}

func (e *engine) size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.winW, e.winH
}

// resized records a new window size and queues an event if it changed.
func (e *engine) resized(w, h int) bool {
	e.mu.Lock()
	changed := w != e.winW || h != e.winH
	e.winW, e.winH = w, h
	e.mu.Unlock()
	if changed {
		e.events.push(Event{Kind: EventResize, Width: w, Height: h})
	}
	return changed
}

func (e *engine) isClosed() bool { return e.closed.Load() }

// lockFrames stops Show from writing buffers until the returned func is called.
func (e *engine) lockFrames() func() {
	e.showMu.Lock()
	return e.showMu.Unlock
}

func (e *engine) shutdown() {
	e.showMu.Lock()
	e.mu.Lock()
	e.closed.Store(true)
	e.mu.Unlock()
	e.showMu.Unlock()
}

// eventQueue is a small ring. Resizes coalesce, so it cannot fill up with them.
type eventQueue struct {
	mu    sync.Mutex
	items [8]Event
	head  int
	n     int
	close bool
	ready chan struct{}
}

func (q *eventQueue) push(ev Event) {
	q.mu.Lock()
	if ev.Kind == EventClose {
		q.close = true
	}
	last := (q.head + q.n - 1) % len(q.items)
	switch {
	case q.n > 0 && q.items[last].Kind == ev.Kind:
		q.items[last] = ev
	case q.n < len(q.items):
		q.items[(q.head+q.n)%len(q.items)] = ev
		q.n++
	}
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *eventQueue) pop() (Event, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return Event{}, false
	}
	ev := q.items[q.head]
	q.head = (q.head + 1) % len(q.items)
	q.n--
	return ev, true
}

func (q *eventQueue) closeRequested() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.close
}
