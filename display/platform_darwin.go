package display

import (
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/aep/gaal"
	"github.com/aep/gaal/internal/dl"
	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

func platformBackends() []opener { return []opener{{"macos", openMac}} }

// AppKit requires the process's first thread, which runs init functions.
func init() { runtime.LockOSThread() }

type nsPoint struct{ X, Y float64 }
type nsSize struct{ W, H float64 }
type nsRect struct {
	Origin nsPoint
	Size   nsSize
}

func sel(name string) objc.SEL { return objc.RegisterName(name) }

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func nsString(s string) objc.ID {
	b := append([]byte(s), 0)
	id := class("NSString").Send(sel("stringWithUTF8String:"), unsafe.Pointer(&b[0]))
	runtime.KeepAlive(b)
	return id
}

const (
	cv420Video = 0x34323076 // '420v'
	cv420Full  = 0x34323066 // '420f'
)

// macAPI holds the framework entry points. Everything on the frame path goes
// through dl.Caller, which does not allocate.
type macAPI struct {
	msgSend, poolPush, poolPop                      uintptr
	cvCreate, cvRelease, cvLock, cvUnlock           uintptr
	cvPlaneAddr, cvPlaneStride, cvSurface, cvAttach uintptr

	keyIOSurface, keyMatrix, keyPrimaries, keyTransfer uintptr
	matrix601, matrix709, prim709, prim601, tf709      uintptr

	selSetContents, selBegin, selCommit, selDisable, selFlush uintptr
	caTransaction                                             uintptr
	attrs                                                     objc.ID
}

func cfConst(h uintptr, name string, dst *uintptr) error {
	p, err := dl.Sym(h, name)
	if err != nil {
		return err
	}
	*dst = **(**uintptr)(unsafe.Pointer(&p))
	return nil
}

var loadMac = sync.OnceValues(func() (*macAPI, error) {
	a := &macAPI{}
	objcLib, err := dl.Open("/usr/lib/libobjc.A.dylib")
	if err != nil {
		return nil, err
	}
	cv, err := dl.Open("/System/Library/Frameworks/CoreVideo.framework/CoreVideo")
	if err != nil {
		return nil, err
	}
	if _, err := dl.Open("/System/Library/Frameworks/QuartzCore.framework/QuartzCore"); err != nil {
		return nil, err
	}
	if _, err := dl.Open("/System/Library/Frameworks/Foundation.framework/Foundation"); err != nil {
		return nil, err
	}
	err = dl.BindAll(objcLib, dl.Table{"objc_msgSend": &a.msgSend,
		"objc_autoreleasePoolPush": &a.poolPush, "objc_autoreleasePoolPop": &a.poolPop})
	if err != nil {
		return nil, err
	}
	err = dl.BindAll(cv, dl.Table{"CVPixelBufferCreate": &a.cvCreate, "CVPixelBufferRelease": &a.cvRelease,
		"CVPixelBufferLockBaseAddress": &a.cvLock, "CVPixelBufferUnlockBaseAddress": &a.cvUnlock,
		"CVPixelBufferGetBaseAddressOfPlane": &a.cvPlaneAddr, "CVPixelBufferGetBytesPerRowOfPlane": &a.cvPlaneStride,
		"CVPixelBufferGetIOSurface": &a.cvSurface, "CVBufferSetAttachment": &a.cvAttach})
	if err != nil {
		return nil, err
	}
	for name, dst := range map[string]*uintptr{
		"kCVPixelBufferIOSurfacePropertiesKey":       &a.keyIOSurface,
		"kCVImageBufferYCbCrMatrixKey":               &a.keyMatrix,
		"kCVImageBufferColorPrimariesKey":            &a.keyPrimaries,
		"kCVImageBufferTransferFunctionKey":          &a.keyTransfer,
		"kCVImageBufferYCbCrMatrix_ITU_R_601_4":      &a.matrix601,
		"kCVImageBufferYCbCrMatrix_ITU_R_709_2":      &a.matrix709,
		"kCVImageBufferColorPrimaries_ITU_R_709_2":   &a.prim709,
		"kCVImageBufferColorPrimaries_SMPTE_C":       &a.prim601,
		"kCVImageBufferTransferFunction_ITU_R_709_2": &a.tf709,
	} {
		if err := cfConst(cv, name, dst); err != nil {
			return nil, err
		}
	}
	a.selSetContents = uintptr(sel("setContents:"))
	a.selBegin = uintptr(sel("begin"))
	a.selCommit = uintptr(sel("commit"))
	a.selDisable = uintptr(sel("setDisableActions:"))
	a.selFlush = uintptr(sel("flush"))
	a.caTransaction = uintptr(class("CATransaction"))
	if a.caTransaction == 0 {
		return nil, errors.New("QuartzCore is not available")
	}
	empty := class("NSDictionary").Send(sel("dictionary"))
	a.attrs = class("NSDictionary").Send(sel("dictionaryWithObject:forKey:"), empty, a.keyIOSurface)
	a.attrs.Send(sel("retain"))
	return a, nil
})

// macBuffers is a set of IOSurface-backed NV12 pixel buffers.
type macBuffers struct {
	a    *macAPI
	c    dl.Caller
	l    layout
	pbs  [4]uintptr
	last atomic.Int32
}

func newMacBuffers(a *macAPI, l layout) (*macBuffers, error) {
	b := &macBuffers{a: a, l: l}
	b.last.Store(-1)
	format := uintptr(cv420Video)
	if l.full {
		format = cv420Full
	}
	matrix, prim := a.matrix601, a.prim601
	if l.matrix == BT709 {
		matrix, prim = a.matrix709, a.prim709
	}
	for i := range b.pbs {
		var pb uintptr
		r := int32(b.c.C6(a.cvCreate, 0, uintptr(l.width), uintptr(l.height), format, uintptr(a.attrs), esc(&pb)))
		if r != 0 {
			b.free()
			return nil, fmt.Errorf("CVPixelBufferCreate: %d", r)
		}
		b.pbs[i] = pb
		const propagate = 1
		b.c.C5(a.cvAttach, pb, a.keyMatrix, matrix, propagate, 0)
		b.c.C5(a.cvAttach, pb, a.keyPrimaries, prim, propagate, 0)
		b.c.C5(a.cvAttach, pb, a.keyTransfer, a.tf709, propagate, 0)
	}
	return b, nil
}

func (b *macBuffers) free() {
	for i, pb := range b.pbs {
		if pb != 0 {
			b.c.C1(b.a.cvRelease, pb)
			b.pbs[i] = 0
		}
	}
}

func (b *macBuffers) plane(pb uintptr, i, rows int) ([]byte, int) {
	addr := b.c.C2(b.a.cvPlaneAddr, pb, uintptr(i))
	stride := int(b.c.C2(b.a.cvPlaneStride, pb, uintptr(i)))
	return unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), stride*rows), stride
}

func (b *macBuffers) fill(i int, f *gaal.VideoFrame) {
	a, pb := b.a, b.pbs[i]
	b.c.C2(a.cvLock, pb, 0)
	y, ys := b.plane(pb, 0, f.Height)
	uv, us := b.plane(pb, 1, (f.Height+1)/2)
	toNV12(y, ys, uv, us, f)
	b.c.C2(a.cvUnlock, pb, 0)
}

// image converts buffer i to RGBA.
func (b *macBuffers) image(i int) *image.RGBA {
	const readOnly = 1
	pb, l := b.pbs[i], b.l
	var c dl.Caller
	c.C2(b.a.cvLock, pb, readOnly)
	defer c.C2(b.a.cvUnlock, pb, readOnly)
	y, ys := b.plane(pb, 0, l.height)
	uv, us := b.plane(pb, 1, (l.height+1)/2)
	f := gaal.VideoFrame{Width: l.width, Height: l.height, Format: gaal.NV12, Y: y, U: uv, YStride: ys, UVStride: us}
	out := make([]byte, 4*l.width*l.height)
	toBGRX(out, 4*l.width, &f, l.coefs())
	return bgrxToRGBA(out, 4*l.width, l.width, l.height)
}

// The main thread loop.
var (
	mainActive atomic.Bool
	mainQueue  = make(chan func(), 16)
	mainQuit   atomic.Bool
	appReady   atomic.Bool
	wakeMain   func()
)

// hasWindowServer reports whether this process belongs to a GUI session.
func hasWindowServer() bool {
	cg, err := dl.Open("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
	if err != nil {
		return false
	}
	fn, err := dl.Sym(cg, "CGSessionCopyCurrentDictionary")
	if err != nil {
		return false
	}
	r, _, _ := purego.SyscallN(fn)
	return r != 0
}

// Main runs fn on a new goroutine and the AppKit event loop on the calling
// thread, which must be the main thread: call it from the main goroutine, before
// anything else that could move it. Main returns when fn returns.
func Main(fn func()) {
	if !mainActive.CompareAndSwap(false, true) {
		fn()
		return
	}
	done := make(chan struct{})
	gui := hasWindowServer()
	var app objc.ID
	var nextEvent, sendEvent, mask, future, mode uintptr
	var a *macAPI
	if gui {
		var err error
		if a, err = loadMac(); err != nil {
			gui = false
		}
	}
	if gui {
		if _, err := dl.Open("/System/Library/Frameworks/AppKit.framework/AppKit"); err != nil {
			gui = false
		}
	}
	if gui {
		app = class("NSApplication").Send(sel("sharedApplication"))
		app.Send(sel("setActivationPolicy:"), 0)
		app.Send(sel("finishLaunching"))
		nextEvent = uintptr(sel("nextEventMatchingMask:untilDate:inMode:dequeue:"))
		sendEvent = uintptr(sel("sendEvent:"))
		mask = ^uintptr(0)
		future = uintptr(class("NSDate").Send(sel("distantFuture")))
		mode = uintptr(nsString("kCFRunLoopDefaultMode"))
		wakeMain = func() {
			ev := class("NSEvent").Send(sel("otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:"),
				15, nsPoint{}, 0, float64(0), 0, 0, int16(0), 0, 0)
			app.Send(sel("postEvent:atStart:"), ev, true)
		}
		appReady.Store(true)
	}
	go func() {
		defer close(done)
		defer func() {
			mainQuit.Store(true)
			if wakeMain != nil {
				wakeMain()
			}
		}()
		fn()
	}()
	var c dl.Caller
	for {
		for drained := false; !drained; {
			select {
			case f := <-mainQueue:
				f()
			default:
				drained = true
			}
		}
		if mainQuit.Load() {
			break
		}
		if !gui {
			select {
			case f := <-mainQueue:
				f()
			case <-done:
			}
			continue
		}
		pool := c.C1(a.poolPush, 0)
		ev := c.C6(a.msgSend, uintptr(app), nextEvent, mask, future, mode, 1)
		if ev != 0 {
			c.C3(a.msgSend, uintptr(app), sendEvent, ev)
		}
		c.C1(a.poolPop, pool)
	}
	<-done
}

// onMain runs f on the main thread and waits for it.
func onMain(f func()) {
	done := make(chan struct{})
	mainQueue <- func() { f(); close(done) }
	if wakeMain != nil {
		wakeMain()
	}
	<-done
}

// macWindows maps window delegates to their displays.
var (
	macMu       sync.Mutex
	macWindows  = map[objc.ID]*macWindow{}
	macDelegate objc.Class
)

func delegateClass() (objc.Class, error) {
	if macDelegate != 0 {
		return macDelegate, nil
	}
	cls, err := objc.RegisterClass("GaalWindowDelegate", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
		{Cmd: sel("windowShouldClose:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) bool {
			if w := lookupWindow(self); w != nil {
				w.d.e.events.push(Event{Kind: EventClose})
			}
			return false // the application decides by calling Close
		}},
		{Cmd: sel("windowDidResize:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
			if w := lookupWindow(self); w != nil {
				w.updateSize()
			}
		}},
	})
	macDelegate = cls
	return cls, err
}

func lookupWindow(id objc.ID) *macWindow {
	macMu.Lock()
	defer macMu.Unlock()
	return macWindows[id]
}

// macJob is one frame for the presenter goroutine.
type macJob struct {
	buf     *macBuffers
	index   int
	shownAt int64
}

type macWindow struct {
	d        *Display
	a        *macAPI
	window   objc.ID
	view     objc.ID
	layer    objc.ID
	delegate objc.ID
	buf      *macBuffers
	old      []*macBuffers
	c        dl.Caller
	work     chan macJob
	stop     chan struct{}
	done     chan struct{}
	refresh  time.Duration
	mu       sync.Mutex
	lastShow int
	retired  [4]atomic.Int64 // when each buffer left the screen
}

func openMac(d *Display, o Options) (backend, error) {
	if !mainActive.Load() {
		return nil, errors.New("the program must run inside display.Main")
	}
	if !appReady.Load() {
		return nil, errors.New("no window server session")
	}
	a, err := loadMac()
	if err != nil {
		return nil, err
	}
	w := &macWindow{d: d, a: a, work: make(chan macJob, 1), stop: make(chan struct{}),
		done: make(chan struct{}), refresh: time.Second / 60, lastShow: -1}
	onMain(func() { err = w.create(o) })
	if err != nil {
		return nil, err
	}
	go w.run()
	return w, nil
}

// create builds the window. It runs on the main thread.
func (w *macWindow) create(o Options) error {
	cls, err := delegateClass()
	if err != nil {
		return err
	}
	rect := nsRect{Size: nsSize{float64(o.Width), float64(o.Height)}}
	const titled, closable, miniaturizable, resizable = 1, 2, 4, 8
	const backingBuffered = 2
	win := class("NSWindow").Send(sel("alloc")).Send(sel("initWithContentRect:styleMask:backing:defer:"),
		rect, uintptr(titled|closable|miniaturizable|resizable), uintptr(backingBuffered), false)
	if win == 0 {
		return errors.New("cannot create the window")
	}
	w.window = win
	win.Send(sel("setReleasedWhenClosed:"), false)
	win.Send(sel("setTitle:"), nsString(o.Title))
	win.Send(sel("center"))
	w.delegate = objc.ID(cls).Send(sel("alloc")).Send(sel("init"))
	macMu.Lock()
	macWindows[w.delegate] = w
	macMu.Unlock()
	win.Send(sel("setDelegate:"), w.delegate)

	layer := class("CALayer").Send(sel("layer"))
	layer.Send(sel("retain"))
	w.layer = layer
	gravity := "resizeAspect"
	switch o.Scale {
	case ScaleFill:
		gravity = "resizeAspectFill"
	case ScaleStretch:
		gravity = "resize"
	}
	layer.Send(sel("setContentsGravity:"), nsString(gravity))
	layer.Send(sel("setMasksToBounds:"), true)
	black := class("NSColor").Send(sel("blackColor"))
	layer.Send(sel("setBackgroundColor:"), black.Send(sel("CGColor")))
	w.view = win.Send(sel("contentView"))
	w.view.Send(sel("setLayer:"), layer)
	w.view.Send(sel("setWantsLayer:"), true)

	if s := win.Send(sel("screen")); s != 0 {
		if fps := objc.Send[int](s, sel("maximumFramesPerSecond")); fps > 0 {
			w.refresh = time.Second / time.Duration(fps)
			w.d.e.setRefresh(w.refresh)
		}
	}
	win.Send(sel("makeKeyAndOrderFront:"), 0)
	class("NSApplication").Send(sel("sharedApplication")).Send(sel("activateIgnoringOtherApps:"), true)
	if o.Fullscreen {
		const fullScreenPrimary = 1 << 7
		win.Send(sel("setCollectionBehavior:"), uintptr(fullScreenPrimary))
		win.Send(sel("toggleFullScreen:"), 0)
	}
	w.updateSize()
	return nil
}

func (w *macWindow) updateSize() {
	r := objc.Send[nsRect](w.view, sel("bounds"))
	w.d.e.resized(int(r.Size.W), int(r.Size.H))
}

func (w *macWindow) prepare(l layout) (int, error) {
	b, err := newMacBuffers(w.a, l)
	if err != nil {
		return 0, err
	}
	w.mu.Lock()
	if w.buf != nil {
		w.old = append(w.old, w.buf)
	}
	w.buf = b
	w.lastShow = -1
	w.mu.Unlock()
	for i := range w.retired {
		w.retired[i].Store(0)
	}
	return len(b.pbs), nil
}

// fill writes buffer i. The window server may keep reading a buffer for about a
// frame after it is replaced, so a buffer that just left the screen rests first.
// Buffers are used in turn, so this rarely waits.
func (w *macWindow) fill(i int, f *gaal.VideoFrame) {
	if d := time.Duration(w.retired[i].Load() + int64(2*w.refresh) - now()); d > 0 {
		time.Sleep(min(d, 2*w.refresh))
	}
	w.buf.fill(i, f)
}

func (w *macWindow) submit(i int) error {
	w.work <- macJob{w.buf, i, w.d.e.submittedAt()}
	return nil
}

// run commits frames to the layer. Core Animation has no completion signal that
// is usable here, so frames are paced by the display's refresh interval.
func (w *macWindow) run() {
	defer close(w.done)
	runtime.LockOSThread() // CATransaction state is per thread
	a := w.a
	c := &w.c
	timer := time.NewTimer(time.Hour)
	front := -1
	for {
		select {
		case <-w.stop:
			return
		case job := <-w.work:
			b, i, at := job.buf, job.index, job.shownAt
			w.mu.Lock()
			if b == w.buf {
				w.lastShow = i
			}
			w.mu.Unlock()
			pool := c.C1(a.poolPush, 0)
			surface := c.C1(a.cvSurface, b.pbs[i])
			c.C2(a.msgSend, a.caTransaction, a.selBegin)
			c.C3(a.msgSend, a.caTransaction, a.selDisable, 1)
			c.C3(a.msgSend, uintptr(w.layer), a.selSetContents, surface)
			c.C2(a.msgSend, a.caTransaction, a.selCommit)
			c.C2(a.msgSend, a.caTransaction, a.selFlush)
			c.C1(a.poolPop, pool)
			if front >= 0 && front != i {
				w.retired[front].Store(now())
			}
			front = i
			w.d.e.presented(at, now())
			timer.Reset(w.refresh)
			select {
			case <-timer.C:
			case <-w.stop:
				return
			}
			w.d.e.frameDone()
		}
	}
}

func (w *macWindow) info() Info {
	return Info{Backend: "macos", Path: "calayer-iosurface-nv12", LatencySource: "submit"}
}

// windowImage captures this window, which macOS allows without any permission
// because it belongs to the calling process. It returns nil if that fails.
func (w *macWindow) windowImage() *image.RGBA {
	cg, err := dl.Open("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
	if err != nil {
		return nil
	}
	cf, err := dl.Open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
	if err != nil {
		return nil
	}
	var (
		create                func(r nsRect, list, window, option uint32) uintptr
		width, height, stride func(uintptr) uintptr
		provider, copyData    func(uintptr) uintptr
		bytePtr, length       func(uintptr) uintptr
		release               func(uintptr)
	)
	addr, err := dl.Sym(cg, "CGWindowListCreateImage")
	if err != nil {
		return nil
	}
	purego.RegisterFunc(&create, addr)
	if dl.BindAll(cg, dl.Table{"CGImageGetWidth": &width, "CGImageGetHeight": &height, "CGImageGetBytesPerRow": &stride,
		"CGImageGetDataProvider": &provider, "CGDataProviderCopyData": &copyData}) != nil {
		return nil
	}
	if dl.BindAll(cf, dl.Table{"CFDataGetBytePtr": &bytePtr, "CFDataGetLength": &length, "CFRelease": &release}) != nil {
		return nil
	}
	const includingWindow, ignoreFraming = 8, 1
	inf := math.Inf(1)
	var number uint32
	var frame, content nsRect
	onMain(func() {
		number = uint32(objc.Send[int](w.window, sel("windowNumber")))
		frame = objc.Send[nsRect](w.window, sel("frame"))
		content = objc.Send[nsRect](w.view, sel("bounds"))
	})
	cgImage := create(nsRect{Origin: nsPoint{inf, inf}}, includingWindow, number, ignoreFraming)
	if cgImage == 0 {
		return nil
	}
	defer release(cgImage)
	data := copyData(provider(cgImage))
	if data == 0 {
		return nil
	}
	defer release(data)
	iw, ih, rows := int(width(cgImage)), int(height(cgImage)), int(stride(cgImage))
	p := bytePtr(data)
	raw := unsafe.Slice(*(**byte)(unsafe.Pointer(&p)), int(length(data)))
	if iw == 0 || ih == 0 || len(raw) < rows*ih {
		return nil
	}
	full := bgrxToRGBA(raw, rows, iw, ih) // 32-bit little-endian BGRA
	// Leave out the title bar, which is at the top of the window.
	k := float64(iw) / frame.Size.W
	top := int((frame.Size.H - content.Size.H) * k)
	r := image.Rect(0, top, iw, min(ih, top+int(content.Size.H*k)))
	return full.SubImage(r).(*image.RGBA)
}

func (w *macWindow) readback() (*image.RGBA, ReadbackSource, error) {
	if img := w.windowImage(); img != nil {
		return img, ReadbackWindow, nil
	}
	defer w.d.e.lockFrames()()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf == nil || w.lastShow < 0 {
		return nil, "", ErrNoFrame
	}
	return w.buf.image(w.lastShow), ReadbackBuffer, nil
}

func (w *macWindow) close() error {
	close(w.stop)
	<-w.done
	onMain(func() {
		w.window.Send(sel("setDelegate:"), 0)
		w.window.Send(sel("close"))
		macMu.Lock()
		delete(macWindows, w.delegate)
		macMu.Unlock()
		w.layer.Send(sel("release"))
		w.window.Send(sel("release"))
		w.delegate.Send(sel("release"))
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf != nil {
		w.buf.free()
	}
	for _, b := range w.old {
		b.free()
	}
	w.old = nil
	return nil
}
