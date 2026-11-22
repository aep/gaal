package display

import (
	"image"
	"sync"
	"time"

	"github.com/aep/gaal"
)

// headless keeps frames in memory. It has the same presentation behaviour as a
// real backend, with a simulated refresh, so it is useful in tests.
type headless struct {
	d      *Display
	mu     sync.Mutex
	l      layout
	bufs   [3]*gaal.VideoFrame
	front  int
	manual bool // tests call vblank themselves
	stop   chan struct{}
	stops  sync.Once
	cur    int
	has    bool
	flight int64
	gen    int // counts prepare calls
	curGen int // generation of the frame in cur
}

func openHeadless(d *Display, o Options) (backend, error) {
	h := &headless{d: d, stop: make(chan struct{}), cur: -1}
	if o.Refresh > 0 {
		go h.run(o.Refresh)
	}
	return h, nil
}

func (h *headless) run(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.vblank()
		}
	}
}

// vblank completes the present under way.
func (h *headless) vblank() {
	h.mu.Lock()
	i, at := h.cur, h.flight
	h.cur = -1
	if i >= 0 && h.curGen == h.gen {
		h.front, h.has = i, true
	}
	h.mu.Unlock()
	if i >= 0 {
		h.d.e.presented(at, now())
		h.d.e.frameDone()
	}
}

func (h *headless) prepare(l layout) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.l, h.has = l, false
	h.gen++
	for i := range h.bufs {
		h.bufs[i] = gaal.NewVideoFrame(l.width, l.height, l.format)
	}
	return len(h.bufs), nil
}

func (h *headless) fill(i int, f *gaal.VideoFrame) {
	if err := f.CopyTo(h.bufs[i]); err != nil {
		panic(err) // prepare made the buffer from this layout
	}
}

func (h *headless) submit(i int) error {
	at := h.d.e.submittedAt()
	h.mu.Lock()
	h.cur, h.flight, h.curGen = i, at, h.gen
	h.mu.Unlock()
	if h.d.e.opts.Refresh <= 0 && !h.manual {
		go h.vblank() // not inline: the engine lock is held here
	}
	return nil
}

func (h *headless) info() Info {
	return Info{Backend: "headless", Path: "memory", LatencySource: "submit"}
}

func (h *headless) readback() (*image.RGBA, ReadbackSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.has {
		return nil, "", ErrNoFrame
	}
	f := h.bufs[h.front]
	buf := make([]byte, 4*f.Width*f.Height)
	toBGRX(buf, 4*f.Width, f, h.l.coefs())
	return bgrxToRGBA(buf, 4*f.Width, f.Width, f.Height), ReadbackBuffer, nil
}

func (h *headless) close() error {
	h.stops.Do(func() { close(h.stop) })
	return nil
}
