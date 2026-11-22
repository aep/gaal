//go:build linux && !android

package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/aep/gaal"
)

var ne = binary.NativeEndian

// Features tests can switch off to exercise the fallback paths.
const (
	wlNoDmabuf = 1 << iota
	wlNoShmYUV
	wlNoViewporter
)

var wlDisable int

const (
	wlFormatXRGB8888 = 1
	wlFormatNV12     = 0x3231564E
	drmFormatNV12    = 0x3231564E
	drmModLinear     = 0

	wlBuffers = 4
)

type wlKind uint8

const (
	wkNone wlKind = iota
	wkDisplay
	wkRegistry
	wkCallback
	wkFrame
	wkBuffer
	wkShm
	wkXdgBase
	wkXdgSurface
	wkToplevel
	wkPresentation
	wkFeedback
	wkDmabuf
	wkParams
	wkDmaFeedback
	wkColorMgr
	wkOther
)

type wlObj struct {
	kind wlKind
	aux  int
}

type wlBufRec struct {
	id   uint32
	pool *wlPool
	idx  int
	gen  uint32
	held bool // the compositor has it
	dead bool // replaced: destroy it when released
}

// wlPool is the memory behind one generation of buffers.
type wlPool struct {
	mem  *shmem
	live int
}

type wlFeedback struct {
	id      uint32
	shownAt int64
}

type wlPath int

const (
	wlPathNone wlPath = iota
	wlPathDmabuf
	wlPathShmNV12
	wlPathShmRGB
	wlPathShmScaled
)

type wayland struct {
	d    *Display
	conn *net.UnixConn

	wmu     sync.Mutex
	out     []byte
	objs    []wlObj
	freeIDs []uint32
	nextID  uint32
	server  map[uint32]int // server-created buffers: id to record
	syncs   map[uint32]chan struct{}
	err     error

	// Globals.
	compositor, subcompositor, shm, xdgBase, viewporter uint32
	presentation, dmabuf, decoManager, colorMgr         uint32
	nv12Linear, shmNV12                                 bool
	clockID                                             int32
	clockOffset                                         int64 // compositor clock minus now()
	colorOK                                             map[[2]uint32]bool

	surface, xdgSurface, toplevel, rootViewport uint32
	video, subsurface, videoViewport            uint32
	colorSurface                                uint32
	black                                       uint32 // 1x1 black buffer
	blackMem                                    *shmem
	configured                                  chan struct{}
	confOnce                                    sync.Once
	cfgW, cfgH                                  int
	winW, winH                                  int
	attached                                    bool // video surface has a buffer

	feedback  [16]wlFeedback
	cbShownAt int64

	path      wlPath
	l         layout
	recs      []*wlBufRec
	cur       [wlBuffers]*wlBufRec
	last      *wlBufRec
	pool      *wlPool
	gen       uint32
	bufSize   int
	yStride   int
	uvStride  int
	uvOffset  int
	bufW      int
	bufH      int
	staging   []byte
	dmaFailed atomic.Bool
	dmaWait   chan bool
	dead      atomic.Bool

	// Descriptors can only arrive while setup reads the dmabuf format table.
	wantFDs  atomic.Bool
	inFDs    []int
	dmaTable []byte

	// Test hooks, nil in normal use. They run with wmu held.
	hookGlobal func(reg, name uint32, iface string, version uint32)
	hookEvent  func(id uint32, op uint16, a []byte) bool
}

func openWayland(d *Display, o Options) (backend, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		return nil, errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(name) {
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return nil, errors.New("XDG_RUNTIME_DIR is not set")
		}
		name = filepath.Join(dir, name)
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		return nil, err
	}
	w := &wayland{d: d, conn: c, nextID: 2, server: map[uint32]int{}, syncs: map[uint32]chan struct{}{},
		configured: make(chan struct{}), colorOK: map[[2]uint32]bool{}, winW: o.Width, winH: o.Height,
		dmaWait: make(chan bool, wlBuffers), clockID: -1}
	w.objs = make([]wlObj, 64, 256)
	w.objs[1] = wlObj{kind: wkDisplay}
	d.e.selfFree = true
	w.hookGlobal, w.hookEvent = wlTestGlobal(w), wlTestEvent(w)
	w.wantFDs.Store(true)
	go w.reader()
	if err := w.setup(o); err != nil {
		c.Close()
		return nil, err
	}
	return w, nil
}

// Message building. Callers hold wmu.

func (w *wayland) begin(id uint32, op uint16) int {
	s := len(w.out)
	w.out = ne.AppendUint32(w.out, id)
	w.out = ne.AppendUint32(w.out, uint32(op))
	return s
}

func (w *wayland) end(s int) {
	h := ne.Uint32(w.out[s+4:])
	ne.PutUint32(w.out[s+4:], h|uint32(len(w.out)-s)<<16)
}

func (w *wayland) u(v ...uint32) {
	for _, n := range v {
		w.out = ne.AppendUint32(w.out, n)
	}
}

func (w *wayland) str(s string) {
	w.out = ne.AppendUint32(w.out, uint32(len(s)+1))
	w.out = append(w.out, s...)
	w.out = append(w.out, 0)
	for len(w.out)%4 != 0 {
		w.out = append(w.out, 0)
	}
}

// m0 to m4 send requests with only 32-bit arguments, without allocating.
func (w *wayland) m0(id uint32, op uint16) { w.end(w.begin(id, op)) }

func (w *wayland) m1(id uint32, op uint16, a uint32) {
	s := w.begin(id, op)
	w.out = ne.AppendUint32(w.out, a)
	w.end(s)
}

func (w *wayland) m2(id uint32, op uint16, a, b uint32) {
	s := w.begin(id, op)
	w.out = ne.AppendUint32(ne.AppendUint32(w.out, a), b)
	w.end(s)
}

func (w *wayland) m3(id uint32, op uint16, a, b, c uint32) {
	s := w.begin(id, op)
	w.out = ne.AppendUint32(ne.AppendUint32(ne.AppendUint32(w.out, a), b), c)
	w.end(s)
}

func (w *wayland) m4(id uint32, op uint16, a, b, c, d uint32) {
	s := w.begin(id, op)
	w.out = ne.AppendUint32(ne.AppendUint32(ne.AppendUint32(ne.AppendUint32(w.out, a), b), c), d)
	w.end(s)
}

func (w *wayland) flush() error {
	if len(w.out) == 0 {
		return nil
	}
	_, err := w.conn.Write(w.out)
	w.out = w.out[:0]
	return err
}

func (w *wayland) flushFD(fd int) error {
	_, _, err := w.conn.WriteMsgUnix(w.out, syscall.UnixRights(fd), nil)
	w.out = w.out[:0]
	return err
}

func (w *wayland) newID(kind wlKind, aux int) uint32 {
	var id uint32
	if n := len(w.freeIDs); n > 0 {
		id = w.freeIDs[n-1]
		w.freeIDs = w.freeIDs[:n-1]
	} else {
		id = w.nextID
		w.nextID++
	}
	for int(id) >= len(w.objs) {
		w.objs = append(w.objs, wlObj{})
	}
	w.objs[id] = wlObj{kind, aux}
	return id
}

// roundtrip waits until the compositor has handled everything sent so far.
func (w *wayland) roundtrip() error {
	ch := make(chan struct{})
	w.wmu.Lock()
	if w.dead.Load() {
		w.wmu.Unlock()
		return ErrClosed
	}
	id := w.newID(wkCallback, 0)
	w.syncs[id] = ch
	w.m1(1, 0, id)
	err := w.flush()
	w.wmu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		return errors.New("compositor does not answer")
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.err
}

func (w *wayland) bind(reg, name uint32, iface string, version uint32, kind wlKind) uint32 {
	id := w.newID(kind, 0)
	s := w.begin(reg, 0)
	w.u(name)
	w.str(iface)
	w.u(version, id)
	w.end(s)
	return id
}

func (w *wayland) setup(o Options) error {
	w.wmu.Lock()
	reg := w.newID(wkRegistry, 0)
	w.m1(1, 1, reg)
	w.wmu.Unlock()
	if err := w.roundtrip(); err != nil {
		return err
	}
	if err := w.roundtrip(); err != nil { // events of the bound globals
		return err
	}
	w.wantFDs.Store(false)
	w.wmu.Lock()
	missing := w.compositor == 0 || w.shm == 0 || w.xdgBase == 0
	if !missing {
		w.createWindow(o)
	}
	err := w.flush()
	w.wmu.Unlock()
	if missing {
		return errors.New("compositor lacks wl_compositor, wl_shm or xdg_wm_base")
	}
	if err != nil {
		return err
	}
	if w.clockID >= 0 {
		var ts syscall.Timespec
		if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, uintptr(w.clockID), uintptr(unsafe.Pointer(&ts)), 0); e == 0 {
			w.clockOffset = ts.Nano() - now()
		} else {
			w.clockID = -1
		}
	}
	select {
	case <-w.configured:
	case <-time.After(5 * time.Second):
		return errors.New("compositor did not configure the window")
	}
	return w.roundtrip()
}

func (w *wayland) createWindow(o Options) {
	w.surface = w.newID(wkOther, 0)
	w.m1(w.compositor, 0, w.surface)
	w.xdgSurface = w.newID(wkXdgSurface, 0)
	w.m2(w.xdgBase, 2, w.xdgSurface, w.surface)
	w.toplevel = w.newID(wkToplevel, 0)
	w.m1(w.xdgSurface, 1, w.toplevel)
	s := w.begin(w.toplevel, 2)
	w.str(o.Title)
	w.end(s)
	s = w.begin(w.toplevel, 3)
	w.str("gaal")
	w.end(s)
	if o.Fullscreen {
		w.m1(w.toplevel, 11, 0)
	}
	if w.decoManager != 0 {
		deco := w.newID(wkOther, 0)
		w.m2(w.decoManager, 1, deco, w.toplevel)
		w.m1(deco, 1, 2) // server side
	}
	if w.viewporter != 0 && w.subcompositor != 0 {
		w.rootViewport = w.newID(wkOther, 0)
		w.m2(w.viewporter, 1, w.rootViewport, w.surface)
		w.video = w.newID(wkOther, 0)
		w.m1(w.compositor, 0, w.video)
		w.subsurface = w.newID(wkOther, 0)
		w.m3(w.subcompositor, 1, w.subsurface, w.video, w.surface)
		w.m0(w.subsurface, 5) // desync: frames do not wait for the parent
		w.videoViewport = w.newID(wkOther, 0)
		w.m2(w.viewporter, 1, w.videoViewport, w.video)
		// An empty input region lets clicks reach the toplevel.
		region := w.newID(wkOther, 0)
		w.m1(w.compositor, 1, region)
		w.m1(w.video, 5, region)
		w.m0(region, 0)
	} else {
		w.video = w.surface
	}
	w.m0(w.surface, 6)
}

// makeBlack creates the 1x1 buffer that fills the window behind the video.
func (w *wayland) makeBlack() error {
	m, err := newShmem(4, false)
	if err != nil {
		return err
	}
	m.data[0], m.data[1], m.data[2], m.data[3] = 0, 0, 0, 255
	w.blackMem = m
	pool := w.newID(wkOther, 0)
	s := w.begin(w.shm, 0)
	w.u(pool, 4)
	w.end(s)
	if err := w.flushFD(m.fd); err != nil {
		return err
	}
	w.black = w.newID(wkOther, 0)
	s = w.begin(pool, 0)
	w.u(w.black, 0, 1, 1, 4, wlFormatXRGB8888)
	w.end(s)
	w.m0(pool, 1)
	return nil
}

func (w *wayland) reader() {
	buf := make([]byte, 1<<16)
	oob := make([]byte, 128)
	have := 0
	var err error
	for err == nil {
		var n int
		if w.wantFDs.Load() {
			var oobn int
			n, oobn, _, _, err = w.conn.ReadMsgUnix(buf[have:], oob)
			w.takeFDs(oob[:oobn])
		} else {
			n, err = w.conn.Read(buf[have:])
		}
		have += n
		off := 0
		for have-off >= 8 {
			size := int(ne.Uint32(buf[off+4:]) >> 16)
			if size < 8 || have-off < size {
				break
			}
			w.dispatch(ne.Uint32(buf[off:]), uint16(ne.Uint32(buf[off+4:])), buf[off+8:off+size])
			off += size
		}
		copy(buf, buf[off:have])
		have -= off
	}
	w.wmu.Lock()
	w.dead.Store(true)
	for i, ch := range w.syncs {
		close(ch)
		delete(w.syncs, i)
	}
	w.wmu.Unlock()
	if !w.d.e.isClosed() {
		w.d.e.events.push(Event{Kind: EventClose})
	}
}

func (w *wayland) takeFDs(oob []byte) {
	if len(oob) == 0 {
		return
	}
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return
	}
	for i := range msgs {
		if fds, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
			w.wmu.Lock()
			w.inFDs = append(w.inFDs, fds...)
			w.wmu.Unlock()
		}
	}
}

// dmaFeedback handles zwp_linux_dmabuf_feedback_v1 events, looking for linear
// NV12 in the format table.
func (w *wayland) dmaFeedback(op uint16, a []byte) {
	switch op {
	case 1: // format_table
		if len(w.inFDs) == 0 {
			return
		}
		fd := w.inFDs[0]
		w.inFDs = w.inFDs[1:]
		if t, err := syscall.Mmap(fd, 0, int(ne.Uint32(a)), syscall.PROT_READ, syscall.MAP_PRIVATE); err == nil {
			w.dmaTable = t
		}
		syscall.Close(fd)
	case 5: // tranche_formats
		n := int(ne.Uint32(a)) / 2
		for i := 0; i < n && len(a) >= 6+2*i; i++ {
			e := 16 * int(ne.Uint16(a[4+2*i:]))
			if e+16 <= len(w.dmaTable) && ne.Uint32(w.dmaTable[e:]) == drmFormatNV12 &&
				ne.Uint64(w.dmaTable[e+8:]) == drmModLinear {
				w.nv12Linear = true
			}
		}
	case 0: // done
		if w.dmaTable != nil {
			syscall.Munmap(w.dmaTable)
			w.dmaTable = nil
		}
	}
}

func wlString(b []byte) string {
	n := int(ne.Uint32(b))
	if n == 0 || len(b) < 4+n {
		return ""
	}
	return string(b[4 : 4+n-1])
}

func (w *wayland) dispatch(id uint32, op uint16, a []byte) {
	w.wmu.Lock()
	var o wlObj
	if id >= 0xFF000000 {
		if i, ok := w.server[id]; ok {
			o = wlObj{wkBuffer, i}
		}
	} else if int(id) < len(w.objs) {
		o = w.objs[id]
	}
	if w.hookEvent != nil && (o.kind == wkNone || o.kind == wkOther) && w.hookEvent(id, op, a) {
		w.wmu.Unlock()
		return
	}
	switch o.kind {
	case wkDisplay:
		if op == 0 {
			w.err = fmt.Errorf("wayland: protocol error %d on object %d: %s",
				ne.Uint32(a[4:]), ne.Uint32(a), wlString(a[8:]))
			err := w.err
			for i, ch := range w.syncs {
				close(ch)
				delete(w.syncs, i)
			}
			w.wmu.Unlock()
			w.d.e.fail(err)
			w.conn.Close()
			return
		}
		if op == 1 {
			d := ne.Uint32(a)
			w.objs[d] = wlObj{}
			w.freeIDs = append(w.freeIDs, d)
		}
	case wkRegistry:
		if op == 0 {
			w.global(id, ne.Uint32(a), wlString(a[4:]), a)
		}
	case wkCallback:
		if ch := w.syncs[id]; ch != nil {
			delete(w.syncs, id)
			close(ch)
		}
	case wkFrame:
		at := w.cbShownAt
		timed := w.presentation == 0
		w.wmu.Unlock()
		if timed {
			w.d.e.presented(at, now())
		}
		w.d.e.frameDone()
		return
	case wkBuffer:
		if op == 0 {
			w.bufferReleased(o.aux)
			return // unlocks
		}
	case wkShm:
		if ne.Uint32(a) == wlFormatNV12 {
			w.shmNV12 = true
		}
	case wkXdgBase:
		w.m1(id, 3, ne.Uint32(a)) // pong
		w.flush()
	case wkXdgSurface:
		w.configure(ne.Uint32(a))
		return // unlocks
	case wkToplevel:
		switch op {
		case 0:
			w.cfgW, w.cfgH = int(int32(ne.Uint32(a))), int(int32(ne.Uint32(a[4:])))
		case 1:
			w.wmu.Unlock()
			w.d.e.events.push(Event{Kind: EventClose})
			return
		}
	case wkPresentation:
		w.clockID = int32(ne.Uint32(a))
	case wkFeedback:
		w.feedbackEvent(id, o.aux, op, a)
		return // unlocks
	case wkDmabuf:
		if op == 1 && ne.Uint32(a) == drmFormatNV12 && ne.Uint32(a[4:]) == 0 && ne.Uint32(a[8:]) == drmModLinear {
			w.nv12Linear = true
		}
	case wkParams:
		w.paramsEvent(id, o.aux, op, a)
		return // unlocks
	case wkDmaFeedback:
		w.dmaFeedback(op, a)
	case wkColorMgr:
		if op == 1 {
			w.colorOK[[2]uint32{ne.Uint32(a), ne.Uint32(a[4:])}] = true
		}
	}
	w.wmu.Unlock()
}

func (w *wayland) global(reg, name uint32, iface string, a []byte) {
	ver := ne.Uint32(a[len(a)-4:])
	if w.hookGlobal != nil {
		w.hookGlobal(reg, name, iface, ver)
	}
	switch iface {
	case "wl_compositor":
		if ver >= 4 {
			w.compositor = w.bind(reg, name, iface, 4, wkOther)
		}
	case "wl_subcompositor":
		w.subcompositor = w.bind(reg, name, iface, 1, wkOther)
	case "wl_shm":
		w.shm = w.bind(reg, name, iface, 1, wkShm)
	case "xdg_wm_base":
		w.xdgBase = w.bind(reg, name, iface, 1, wkXdgBase)
	case "wp_viewporter":
		if wlDisable&wlNoViewporter == 0 {
			w.viewporter = w.bind(reg, name, iface, 1, wkOther)
		}
	case "wp_presentation":
		w.presentation = w.bind(reg, name, iface, 1, wkPresentation)
	case "zwp_linux_dmabuf_v1":
		if ver >= 3 && wlDisable&wlNoDmabuf == 0 {
			w.dmabuf = w.bind(reg, name, iface, min(ver, 4), wkDmabuf)
			if ver >= 4 {
				w.m1(w.dmabuf, 2, w.newID(wkDmaFeedback, 0))
			}
		}
	case "zxdg_decoration_manager_v1":
		w.decoManager = w.bind(reg, name, iface, 1, wkOther)
	case "wp_color_representation_manager_v1":
		w.colorMgr = w.bind(reg, name, iface, 1, wkColorMgr)
	}
	w.flush()
}

// configure handles xdg_surface.configure. It is called with wmu held and
// releases it.
func (w *wayland) configure(serial uint32) {
	w.m1(w.xdgSurface, 4, serial)
	ww, wh := w.cfgW, w.cfgH
	if ww <= 0 {
		ww = w.winW
	}
	if wh <= 0 {
		wh = w.winH
	}
	changed := ww != w.winW || wh != w.winH
	w.winW, w.winH = ww, wh
	first := w.black == 0 && w.rootViewport != 0
	if first {
		if err := w.makeBlack(); err != nil {
			w.err = err
		}
	}
	w.geometry()
	if w.rootViewport == 0 {
		w.m0(w.surface, 6) // acknowledges the configure
	}
	w.flush()
	scaled := w.path == wlPathShmScaled
	w.wmu.Unlock()
	w.confOnce.Do(func() { close(w.configured) })
	if w.d.e.resized(ww, wh) || changed {
		if scaled {
			w.d.e.invalidate()
		}
	}
}

// geometry applies the window size and frame placement. Callers hold wmu.
func (w *wayland) geometry() {
	if w.rootViewport == 0 {
		return
	}
	w.m3(w.surface, 1, w.black, 0, 0)
	w.m2(w.rootViewport, 2, uint32(w.winW), uint32(w.winH))
	w.m4(w.surface, 9, 0, 0, 1, 1) // damage_buffer
	region := w.newID(wkOther, 0)
	w.m1(w.compositor, 1, region)
	w.m4(region, 1, 0, 0, uint32(w.winW), uint32(w.winH))
	w.m1(w.surface, 4, region)
	w.m0(region, 0)
	if w.path != wlPathNone {
		sr, dr := place(w.d.e.opts.Scale, w.l.width, w.l.height, w.winW, w.winH)
		w.m4(w.videoViewport, 1, uint32(sr.x)<<8, uint32(sr.y)<<8, uint32(sr.w)<<8, uint32(sr.h)<<8)
		w.m2(w.videoViewport, 2, uint32(dr.w), uint32(dr.h))
		w.m2(w.subsurface, 1, uint32(dr.x), uint32(dr.y))
		if w.attached {
			w.m0(w.video, 6)
		}
	}
	w.m0(w.surface, 6)
}

func (w *wayland) bufferReleased(i int) {
	r := w.recs[i]
	r.held = false
	if r.dead {
		w.destroyRec(i)
		w.flush()
		w.wmu.Unlock()
		return
	}
	gen, idx := r.gen, r.idx
	w.wmu.Unlock()
	w.d.e.release(gen, idx)
}

func (w *wayland) destroyRec(i int) {
	r := w.recs[i]
	w.m0(r.id, 0)
	if r.id >= 0xFF000000 {
		delete(w.server, r.id)
	}
	r.dead = true
	r.pool.live--
	if r.pool.live == 0 {
		r.pool.mem.free()
	}
	w.recs[i] = nil
}

func (w *wayland) feedbackEvent(id uint32, slot int, op uint16, a []byte) {
	var at int64
	var refresh uint32
	fb := &w.feedback[slot]
	shownAt := fb.shownAt
	final := op != 0
	if final {
		fb.id = 0
	}
	if op == 1 {
		sec := uint64(ne.Uint32(a))<<32 | uint64(ne.Uint32(a[4:]))
		at = int64(sec)*1e9 + int64(ne.Uint32(a[8:])) - w.clockOffset
		refresh = ne.Uint32(a[12:])
	}
	w.wmu.Unlock()
	switch op {
	case 1:
		w.d.e.presented(shownAt, at)
		if refresh != 0 {
			w.d.e.setRefresh(time.Duration(refresh))
		}
	case 2:
		if shownAt >= 0 {
			w.d.e.discarded()
		}
	}
}

func (w *wayland) paramsEvent(id uint32, aux int, op uint16, a []byte) {
	ok := op == 0
	if ok {
		buf := ne.Uint32(a)
		if r := w.recs[aux]; r != nil && r.id == 0 {
			w.server[buf] = aux
			r.id = buf
		} else {
			w.m0(buf, 0) // it arrived after prepare gave up
		}
	}
	w.m0(id, 0) // destroy the params object
	w.flush()
	w.wmu.Unlock()
	select {
	case w.dmaWait <- ok:
	default:
	}
}

// colorPair returns the color-representation values for l.
func colorPair(l layout) [2]uint32 {
	p := [2]uint32{4, 2} // bt601, limited
	if l.matrix == BT709 {
		p[0] = 2
	}
	if l.full {
		p[1] = 1
	}
	return p
}

// yuvAllowed reports whether the compositor will show l with the right colors.
// Without color representation support, compositors assume limited BT.601.
func (w *wayland) yuvAllowed(l layout) bool {
	if w.colorMgr != 0 && w.colorOK[colorPair(l)] {
		return true
	}
	return l.matrix == BT601 && !l.full
}

// prepare implements backend.
func (w *wayland) prepare(l layout) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.retire()
	w.l = l
	w.gen = w.d.e.generation() + 1
	yuv := w.yuvAllowed(l) && w.rootViewport != 0
	if yuv && w.dmabuf != 0 && w.nv12Linear && !w.dmaFailed.Load() {
		if err := w.prepareDmabuf(l); err == nil {
			return wlBuffers, w.finishPrepare(wlPathDmabuf)
		}
		w.dmaFailed.Store(true)
		w.retire()
	}
	if yuv && w.shmNV12 && wlDisable&wlNoShmYUV == 0 {
		w.layoutNV12(l, 1)
		if err := w.prepareShm(l.width, l.height, w.yStride, wlFormatNV12); err == nil {
			return wlBuffers, w.finishPrepare(wlPathShmNV12)
		}
		w.retire()
	}
	if w.rootViewport == 0 {
		w.bufW, w.bufH = max(w.winW, 1), max(w.winH, 1)
		w.staging = nil
		if w.bufW != l.width || w.bufH != l.height {
			w.staging = make([]byte, 4*l.width*l.height)
		}
		w.yStride = 4 * w.bufW
		w.bufSize = w.yStride * w.bufH
		if err := w.prepareShm(w.bufW, w.bufH, w.yStride, wlFormatXRGB8888); err != nil {
			return 0, err
		}
		return wlBuffers, w.finishPrepare(wlPathShmScaled)
	}
	w.yStride = 4 * l.width
	w.bufSize = w.yStride * l.height
	if err := w.prepareShm(l.width, l.height, w.yStride, wlFormatXRGB8888); err != nil {
		return 0, err
	}
	return wlBuffers, w.finishPrepare(wlPathShmRGB)
}

func (w *wayland) finishPrepare(p wlPath) error {
	w.path = p
	if w.colorMgr != 0 && w.rootViewport != 0 && (p == wlPathDmabuf || p == wlPathShmNV12) {
		if w.colorSurface == 0 {
			w.colorSurface = w.newID(wkOther, 0)
			w.m2(w.colorMgr, 1, w.colorSurface, w.video)
		}
		c := colorPair(w.l)
		w.m2(w.colorSurface, 2, c[0], c[1])
	}
	w.geometry()
	return w.flush()
}

// layoutNV12 computes plane layout with the given stride alignment.
func (w *wayland) layoutNV12(l layout, align int) {
	w.yStride = (l.width + align - 1) &^ (align - 1)
	w.uvStride = w.yStride
	w.uvOffset = w.yStride * ((l.height + 1) &^ 1)
	w.bufSize = w.uvOffset + w.uvStride*(l.height+1)/2
}

func (w *wayland) addRec(p *wlPool, idx int, id uint32) int {
	r := &wlBufRec{id: id, pool: p, idx: idx, gen: w.gen}
	p.live++
	for i, o := range w.recs {
		if o == nil {
			w.recs[i] = r
			w.cur[idx] = r
			return i
		}
	}
	w.recs = append(w.recs, r)
	w.cur[idx] = r
	return len(w.recs) - 1
}

func (w *wayland) prepareShm(bw, bh, stride int, format uint32) error {
	w.bufSize = (w.bufSize + 4095) &^ 4095
	m, err := newShmem(wlBuffers*w.bufSize, false)
	if err != nil {
		return err
	}
	p := &wlPool{mem: m}
	w.pool = p
	pool := w.newID(wkOther, 0)
	s := w.begin(w.shm, 0)
	w.u(pool, uint32(wlBuffers*w.bufSize))
	w.end(s)
	if err := w.flushFD(m.fd); err != nil {
		m.free()
		return err
	}
	for i := 0; i < wlBuffers; i++ {
		rec := w.addRec(p, i, 0)
		id := w.newID(wkBuffer, rec)
		w.cur[i].id = id
		s := w.begin(pool, 0)
		w.u(id, uint32(i*w.bufSize), uint32(bw), uint32(bh), uint32(stride), format)
		w.end(s)
	}
	w.m0(pool, 1)
	return w.flush()
}

func (w *wayland) prepareDmabuf(l layout) error {
	for len(w.dmaWait) > 0 {
		<-w.dmaWait
	}
	w.layoutNV12(l, 256) // what GPUs commonly require for linear sampling
	w.bufSize = (w.bufSize + 4095) &^ 4095
	total := wlBuffers * w.bufSize
	m, err := newShmem(total, true)
	if err != nil {
		return err
	}
	dma, err := udmabuf(m.fd, total)
	if err != nil {
		m.free()
		return err
	}
	defer syscall.Close(dma)
	p := &wlPool{mem: m}
	w.pool = p
	for i := 0; i < wlBuffers; i++ {
		rec := w.addRec(p, i, 0)
		params := w.newID(wkParams, rec)
		w.m1(w.dmabuf, 1, params)
		for plane := 0; plane < 2; plane++ {
			off, stride := i*w.bufSize, w.yStride
			if plane == 1 {
				off += w.uvOffset
				stride = w.uvStride
			}
			if err := w.flush(); err != nil {
				return err
			}
			s := w.begin(params, 1)
			w.u(uint32(plane), uint32(off), uint32(stride), 0, drmModLinear)
			w.end(s)
			if err := w.flushFD(dma); err != nil {
				return err
			}
		}
		w.m4(params, 2, uint32(l.width), uint32(l.height), drmFormatNV12, 0)
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.wmu.Unlock()
	good := true
	timeout := time.After(time.Second)
	for i := 0; i < wlBuffers; i++ {
		select {
		case ok := <-w.dmaWait:
			good = good && ok
		case <-timeout:
			good = false
			i = wlBuffers
		}
	}
	w.wmu.Lock()
	if !good {
		return errors.New("compositor rejected the dmabuf")
	}
	return nil
}

// retire stops using the current buffers. Those the compositor still holds are
// destroyed when it releases them. Callers hold wmu.
func (w *wayland) retire() {
	w.path = wlPathNone
	w.last = nil
	// The old buffer may not fit the new viewport source, so the video surface
	// must not be committed again until a new buffer is attached.
	w.attached = false
	w.staging = nil
	for i, c := range w.cur {
		if c == nil {
			continue
		}
		w.cur[i] = nil
		for j, r := range w.recs {
			if r != c {
				continue
			}
			if r.id == 0 { // never created
				r.pool.live--
				if r.pool.live == 0 {
					r.pool.mem.free()
				}
				w.recs[j] = nil
			} else if r.held {
				r.dead = true
			} else {
				w.destroyRec(j)
			}
		}
	}
}

// fill implements backend.
func (w *wayland) fill(i int, f *gaal.VideoFrame) {
	b := w.cur[i].pool.mem.data[i*w.bufSize : (i+1)*w.bufSize]
	c := w.l.coefs()
	switch w.path {
	case wlPathDmabuf, wlPathShmNV12:
		toNV12(b, w.yStride, b[w.uvOffset:], w.uvStride, f)
	case wlPathShmRGB:
		toBGRX(b, w.yStride, f, c)
	case wlPathShmScaled:
		if w.staging == nil {
			toBGRX(b, w.yStride, f, c)
			return
		}
		toBGRX(w.staging, 4*f.Width, f, c)
		sr, dr := place(w.d.e.opts.Scale, f.Width, f.Height, w.bufW, w.bufH)
		scaleBGRX(b, w.yStride, w.bufW, w.bufH, w.staging, 4*f.Width, sr, dr)
	}
}

// submit implements backend.
func (w *wayland) submit(i int) error {
	shownAt := w.d.e.submittedAt()
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.dead.Load() {
		return ErrClosed
	}
	r := w.cur[i]
	if r == nil {
		return errors.New("wayland: no buffer")
	}
	r.held = true
	w.last = r
	w.attached = true
	w.cbShownAt = shownAt
	if w.presentation != 0 {
		for s := range w.feedback {
			if w.feedback[s].id == 0 {
				id := w.newID(wkFeedback, s)
				w.feedback[s] = wlFeedback{id, shownAt}
				w.m2(w.presentation, 1, w.video, id)
				break
			}
		}
	}
	w.m3(w.video, 1, r.id, 0, 0)
	bw, bh := w.l.width, w.l.height
	if w.path == wlPathShmScaled {
		bw, bh = w.bufW, w.bufH
	}
	w.m4(w.video, 9, 0, 0, uint32(bw), uint32(bh))
	w.m1(w.video, 3, w.newID(wkFrame, 0))
	w.m0(w.video, 6)
	return w.flush()
}

// info implements backend.
func (w *wayland) info() Info {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	in := Info{Backend: "wayland", Path: "none", LatencySource: "frame-callback"}
	if w.presentation != 0 {
		in.LatencySource = "scanout"
	}
	switch w.path {
	case wlPathDmabuf:
		in.Path = "dmabuf-nv12"
	case wlPathShmNV12:
		in.Path = "shm-nv12"
	case wlPathShmRGB:
		in.Path, in.Converted = "shm-xrgb8888", true
	case wlPathShmScaled:
		in.Path, in.Converted = "shm-xrgb8888-scaled", true
	}
	return in
}

// readback returns the buffer last given to the compositor. The compositor
// offers no way to read the window itself without capturing the screen.
func (w *wayland) readback() (*image.RGBA, ReadbackSource, error) {
	defer w.d.e.lockFrames()()
	w.wmu.Lock()
	defer w.wmu.Unlock()
	r := w.last
	if r == nil {
		return nil, "", ErrNoFrame
	}
	b := r.pool.mem.data[r.idx*w.bufSize : (r.idx+1)*w.bufSize]
	l := w.l
	switch w.path {
	case wlPathDmabuf, wlPathShmNV12:
		f := gaal.VideoFrame{Width: l.width, Height: l.height, Format: gaal.NV12, Y: b, U: b[w.uvOffset:],
			YStride: w.yStride, UVStride: w.uvStride}
		out := make([]byte, 4*l.width*l.height)
		toBGRX(out, 4*l.width, &f, l.coefs())
		return bgrxToRGBA(out, 4*l.width, l.width, l.height), ReadbackBuffer, nil
	case wlPathShmScaled:
		return bgrxToRGBA(b, w.yStride, w.bufW, w.bufH), ReadbackBuffer, nil
	}
	return bgrxToRGBA(b, w.yStride, l.width, l.height), ReadbackBuffer, nil
}

// close implements backend.
func (w *wayland) close() error {
	w.wmu.Lock()
	if w.toplevel != 0 {
		w.m0(w.toplevel, 0)
		w.m0(w.xdgSurface, 0)
		w.flush()
	}
	w.wmu.Unlock()
	w.roundtrip()
	return w.conn.Close()
}

const udmabufCreate = 0x40187542

// udmabuf turns a sealed memfd into a dmabuf file descriptor.
func udmabuf(memfd, size int) (int, error) {
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(memfd), fAddSeals, fSealShrink); e != 0 {
		return -1, e
	}
	dev, err := syscall.Open("/dev/udmabuf", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer syscall.Close(dev)
	arg := struct {
		memfd, flags uint32
		offset, size uint64
	}{uint32(memfd), 1, 0, uint64(size)} // UDMABUF_FLAGS_CLOEXEC
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(dev), udmabufCreate, uintptr(unsafe.Pointer(&arg)))
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}
