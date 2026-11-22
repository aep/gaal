// Package display shows gaal.VideoFrame pictures in a native window with as
// little latency and CPU as the platform allows.
//
// Open picks the best backend available at run time:
//
//	Linux    Wayland (dmabuf or wl_shm NV12, else XRGB8888), then X11
//	         (XVideo, else MIT-SHM with RENDER scaling)
//	Android  ANativeWindow supplied by the app (Options.NativeWindow)
//	macOS    AppKit with a CoreAnimation layer showing NV12 IOSurfaces
//	Windows  Direct3D 11 video processor, else GDI
//	any      "headless", selected only by Options.Backend
//
// All protocols are spoken from Go. No library code uses cgo, except that Go
// itself requires it on Android and iOS.
//
// # Presentation model
//
// Show copies the frame into a free display buffer and returns without waiting
// for the display, so the caller keeps ownership of the frame. At most one frame
// is pending. If the display is not ready for it, the next Show replaces it and
// the replaced frame counts as dropped, so the picture never falls behind.
// Show does not allocate once the frame layout is stable.
//
// # macOS
//
// AppKit only runs on the main thread. A macOS program must call Main from its
// main goroutine, normally as the first thing in func main, and do all of its
// work in the function it passes. Main does nothing special on other platforms,
// so portable programs can always use it.
package display

import (
	"errors"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/aep/gaal"
)

// Matrix selects the YCbCr to RGB conversion.
type Matrix uint8

// Matrices.
const (
	// MatrixAuto uses BT.709 for frames wider than 1024 or taller than 576 and
	// BT.601 for the others.
	MatrixAuto Matrix = iota
	BT601
	BT709
)

func (m Matrix) String() string {
	switch m {
	case BT601:
		return "bt601"
	case BT709:
		return "bt709"
	}
	return "auto"
}

// ScaleMode says how a frame maps onto the window.
type ScaleMode uint8

// Scale modes.
const (
	ScaleFit     ScaleMode = iota // keep the aspect ratio and add black bars
	ScaleFill                     // keep the aspect ratio and crop
	ScaleStretch                  // fill the window and ignore the aspect ratio
)

// Options configures Open.
type Options struct {
	Title string
	// Width and Height are the initial window size in window-system units.
	// The default is 1280x720. The window system can choose another size, which
	// is reported by an EventResize.
	Width, Height int
	Fullscreen    bool
	Matrix        Matrix
	Scale         ScaleMode
	// Backend forces a backend: "wayland", "x11", "android", "macos", "windows"
	// or "headless". Empty means the best one available.
	Backend string
	// NativeWindow is the ANativeWindow pointer on Android. The display takes
	// its own reference. It is ignored on other platforms.
	NativeWindow uintptr
	// Refresh is the simulated refresh interval of the headless backend. Zero
	// presents each frame at once.
	Refresh time.Duration
}

// EventKind identifies an Event.
type EventKind uint8

// Event kinds.
const (
	EventClose  EventKind = iota + 1 // the user asked to close the window
	EventResize                      // the window has a new size
)

// Event is a window event.
type Event struct {
	Kind          EventKind
	Width, Height int
}

// Info describes the active presentation path.
type Info struct {
	Backend string // for example "wayland"
	// Path is how frames currently reach the display, for example
	// "dmabuf-nv12" or "shm-xrgb8888". It can change when the frame layout does.
	Path string
	// Converted is true if Show converts to RGB on the CPU.
	Converted bool
	// LatencySource says what Stats latency measures: "scanout" is the
	// compositor's presentation time, "server" is when the window system
	// finished the request, and "submit" is when the frame was handed over.
	LatencySource string
}

// Stats are cumulative timing figures. Durations are zero until measured.
type Stats struct {
	Shown     uint64 // frames accepted by Show
	Presented uint64 // frames confirmed by the display
	// Dropped counts frames that were replaced or discarded before reaching
	// the screen.
	Dropped uint64

	ShowLast, ShowAvg, ShowMax          time.Duration // time spent inside Show
	LatencyLast, LatencyAvg, LatencyMax time.Duration // from Show to presentation
	// Refresh is the display refresh interval, if the display reports it.
	Refresh time.Duration
}

// ReadbackSource says what a Readback returned.
type ReadbackSource string

// Readback sources.
const (
	// ReadbackWindow is the window content, read from the window system.
	ReadbackWindow ReadbackSource = "window"
	// ReadbackBuffer is the last buffer handed to the display. It proves what
	// was submitted, not what the screen shows.
	ReadbackBuffer ReadbackSource = "buffer"
)

// Errors returned by this package.
var (
	ErrClosed      = errors.New("display: closed")
	ErrNoBackend   = errors.New("display: no usable backend")
	ErrUnsupported = errors.New("display: not supported by this backend")
	ErrNoFrame     = errors.New("display: no frame has been shown")
)

// backend is one window system. The engine serializes prepare and fill. It
// calls submit with engine.mu held, so submit must not wait for the display.
type backend interface {
	// prepare makes buffers for frames described by l and returns how many
	// there are, at most maxBuffers.
	prepare(l layout) (int, error)
	// fill writes f into buffer i, which the caller owns.
	fill(i int, f *gaal.VideoFrame)
	// submit starts presenting buffer i.
	submit(i int) error
	info() Info
	close() error
}

// windowChanger is implemented by backends that must be told about changes to a
// window they do not own.
type windowChanger interface{ windowChanged() }

// readbacker is implemented by backends that can return pixels.
type readbacker interface {
	readback() (*image.RGBA, ReadbackSource, error)
}

// Display is an open window. Its methods are safe for concurrent use.
type Display struct {
	e    engine
	b    backend
	once sync.Once
	cerr error
}

type opener struct {
	name string
	open func(*Display, Options) (backend, error)
}

// Open creates a window with the best available backend, or the one named by
// opts.Backend.
func Open(opts Options) (*Display, error) {
	if opts.Width <= 0 {
		opts.Width = 1280
	}
	if opts.Height <= 0 {
		opts.Height = 720
	}
	if opts.Title == "" {
		opts.Title = "gaal"
	}
	list := append(platformBackends(), opener{"headless", openHeadless})
	var errs []error
	for _, o := range list {
		if opts.Backend == "" && o.name == "headless" {
			continue
		}
		if opts.Backend != "" && opts.Backend != o.name {
			continue
		}
		d := &Display{}
		d.e.init(opts)
		b, err := o.open(d, opts)
		if err == nil {
			d.b = b
			d.e.b = b
			return d, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", o.name, err))
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("%w: %q is not available on this platform", ErrNoBackend, opts.Backend)
	}
	return nil, fmt.Errorf("%w: %w", ErrNoBackend, errors.Join(errs...))
}

// Show presents f, replacing any frame that is still waiting. It does not keep
// f and does not wait for the display. A frame that arrives while every buffer
// is held by the display is dropped, which Stats reports.
func (d *Display) Show(f *gaal.VideoFrame) error { return d.e.show(f) }

// PollEvent returns the next window event, if there is one.
func (d *Display) PollEvent() (Event, bool) { return d.e.events.pop() }

// EventReady receives a value when PollEvent may have something to return.
func (d *Display) EventReady() <-chan struct{} { return d.e.events.ready }

// CloseRequested reports whether the user asked to close the window.
func (d *Display) CloseRequested() bool { return d.e.events.closeRequested() }

// Size returns the current window size.
func (d *Display) Size() (width, height int) { return d.e.size() }

// Stats returns the timing figures collected so far.
func (d *Display) Stats() Stats { return d.e.stats() }

// ResetStats clears the timing figures, for example after startup.
func (d *Display) ResetStats() { d.e.resetStats() }

// WindowChanged tells the display that a window supplied by the application
// changed size. On Android, call it from onNativeWindowResized. It does nothing
// on backends that own their window.
func (d *Display) WindowChanged() {
	if c, ok := d.b.(windowChanger); ok {
		c.windowChanged()
	}
}

// Info describes the active backend and path.
func (d *Display) Info() Info { return d.b.info() }

// Readback returns the displayed picture as far as the backend can observe it.
// It is slow and meant for tests and diagnostics. It returns ErrUnsupported if
// the backend cannot read pixels.
func (d *Display) Readback() (*image.RGBA, ReadbackSource, error) {
	r, ok := d.b.(readbacker)
	if !ok {
		return nil, "", ErrUnsupported
	}
	if d.e.isClosed() {
		return nil, "", ErrClosed
	}
	return r.readback()
}

// Close destroys the window. Later calls to Show return ErrClosed.
func (d *Display) Close() error {
	d.once.Do(func() {
		d.e.shutdown()
		d.cerr = d.b.close()
	})
	return d.cerr
}

type rect struct{ x, y, w, h int }

// place computes which part of a sw by sh frame is drawn where in a ww by wh
// window.
func place(mode ScaleMode, sw, sh, ww, wh int) (src, dst rect) {
	src, dst = rect{0, 0, sw, sh}, rect{0, 0, ww, wh}
	if sw <= 0 || sh <= 0 || ww <= 0 || wh <= 0 {
		return src, dst
	}
	wider := ww*sh > wh*sw // the window is wider than the frame
	switch mode {
	case ScaleFit:
		if wider {
			dst.w = max(1, (wh*sw+sh/2)/sh)
			dst.x = (ww - dst.w) / 2
		} else {
			dst.h = max(1, (ww*sh+sw/2)/sw)
			dst.y = (wh - dst.h) / 2
		}
	case ScaleFill:
		if wider {
			src.h = max(1, (sw*wh+ww/2)/ww)
			src.y = (sh - src.h) / 2
		} else {
			src.w = max(1, (sh*ww+wh/2)/wh)
			src.x = (sw - src.w) / 2
		}
	}
	return src, dst
}
