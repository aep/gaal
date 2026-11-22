package display

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/aep/gaal"
)

func platformBackends() []opener { return []opener{{"windows", openWindows}} }

// winDisableD3D makes the backend use GDI, for tests.
var winDisableD3D bool

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	d3d11dll = syscall.NewLazyDLL("d3d11.dll")

	procRegisterClassEx   = user32.NewProc("RegisterClassExW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procShowWindow        = user32.NewProc("ShowWindow")
	procPeekMessage       = user32.NewProc("PeekMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessage   = user32.NewProc("DispatchMessageW")
	procMsgWait           = user32.NewProc("MsgWaitForMultipleObjectsEx")
	procGetClientRect     = user32.NewProc("GetClientRect")
	procGetDC             = user32.NewProc("GetDC")
	procReleaseDC         = user32.NewProc("ReleaseDC")
	procBeginPaint        = user32.NewProc("BeginPaint")
	procEndPaint          = user32.NewProc("EndPaint")
	procAdjustWindowRect  = user32.NewProc("AdjustWindowRectEx")
	procLoadCursor        = user32.NewProc("LoadCursorW")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procStretchDIBits     = gdi32.NewProc("StretchDIBits")
	procSetStretchBltMode = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx     = gdi32.NewProc("SetBrushOrgEx")
	procPatBlt            = gdi32.NewProc("PatBlt")
	procBitBlt            = gdi32.NewProc("BitBlt")
	procCreateCompatDC    = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatBmp   = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject      = gdi32.NewProc("SelectObject")
	procDeleteObject      = gdi32.NewProc("DeleteObject")
	procDeleteDC          = gdi32.NewProc("DeleteDC")
	procGetDIBits         = gdi32.NewProc("GetDIBits")
	procCreateEvent       = kernel32.NewProc("CreateEventW")
	procSetEvent          = kernel32.NewProc("SetEvent")
	procCloseHandle       = kernel32.NewProc("CloseHandle")
	procWaitSingle        = kernel32.NewProc("WaitForSingleObject")
	procGetModuleHandle   = kernel32.NewProc("GetModuleHandleW")
	procD3D11CreateDevice = d3d11dll.NewProc("D3D11CreateDevice")
)

// winCall makes system and COM calls without allocating. It is not safe for
// concurrent use.
type winCall struct{ a [16]uintptr }

func (c *winCall) call(fn uintptr, n int) uintptr {
	r, _, _ := syscall.SyscallN(fn, c.a[:n]...)
	return r
}

func (c *winCall) p(proc *syscall.LazyProc, args ...uintptr) uintptr {
	n := copy(c.a[:], args)
	return c.call(proc.Addr(), n)
}

func winPtr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

func comMethod(obj uintptr, slot int) uintptr {
	vt := *(*uintptr)(winPtr(obj))
	return *(*uintptr)(winPtr(vt + uintptr(slot)*unsafe.Sizeof(vt)))
}

// m calls a COM method. The object is passed as the first argument.
func (c *winCall) m(obj uintptr, slot int, args ...uintptr) uint32 {
	c.a[0] = obj
	n := copy(c.a[1:], args)
	return uint32(c.call(comMethod(obj, slot), n+1))
}

func comRelease(c *winCall, obj *uintptr) {
	if *obj != 0 {
		c.m(*obj, 2)
		*obj = 0
	}
}

type guid struct {
	a    uint32
	b, c uint16
	d    [8]byte
}

var (
	iidVideoDevice  = guid{0x10EC4D5B, 0x975A, 0x4689, [8]byte{0xB9, 0xE4, 0xD0, 0xAA, 0xC3, 0x0F, 0xE3, 0x33}}
	iidVideoContext = guid{0x61F21C45, 0x3C0E, 0x4A74, [8]byte{0x9C, 0xEA, 0x67, 0x10, 0x0D, 0x9A, 0xD5, 0xE4}}
	iidDXGIDevice   = guid{0x54EC77FA, 0x1377, 0x44E6, [8]byte{0x8C, 0x32, 0x88, 0xFD, 0x5F, 0x44, 0xC8, 0x4C}}
	iidDXGIFactory2 = guid{0x50C83A1C, 0xE072, 0x4C48, [8]byte{0x87, 0xB0, 0x36, 0x30, 0xFA, 0x36, 0xA6, 0xD0}}
	iidSwapChain2   = guid{0xA8BE2AC4, 0x199F, 0x4946, [8]byte{0xB3, 0x31, 0x79, 0x59, 0x9F, 0xB9, 0x8D, 0xE7}}
	iidTexture2D    = guid{0x6F15AAF2, 0xD208, 0x4E89, [8]byte{0x9A, 0xB4, 0x48, 0x95, 0x35, 0xD3, 0x4F, 0x9C}}
)

// Vtable slots.
const (
	slotQI = 0

	devCreateTexture2D      = 5
	devGetImmediateContext  = 40
	ctxMap                  = 14
	ctxUnmap                = 15
	ctxCopyResource         = 47
	ctxFlush                = 111
	vdCreateProcessor       = 4
	vdCreateInputView       = 8
	vdCreateOutputView      = 9
	vdCreateEnumerator      = 10
	vcSetOutputTargetRect   = 13
	vcSetOutputBackground   = 14
	vcSetOutputColorSpace   = 15
	vcSetStreamFrameFormat  = 27
	vcSetStreamColorSpace   = 28
	vcSetStreamSourceRect   = 30
	vcSetStreamDestRect     = 31
	vcBlt                   = 53
	dxgiGetAdapter          = 7
	dxgiGetParent           = 6
	factoryCreateSwapHwnd   = 15
	swapPresent             = 8
	swapGetBuffer           = 9
	swapResizeBuffers       = 13
	swap2SetMaxLatency      = 31
	swap2GetLatencyWaitable = 33

	dxgiFormatNV12  = 103
	dxgiFormatBGRA8 = 87
	swapFlags       = 0x40 // DXGI_SWAP_CHAIN_FLAG_FRAME_LATENCY_WAITABLE_OBJECT
)

type texDesc struct {
	w, h, mips, array, format, samples, quality, usage, bind, cpu, misc uint32
}

type winRect struct{ l, t, r, b int32 }

type vpStream struct {
	enable, outputIndex, inputFrame, past, future       uint32
	_                                                   uint32
	ppPast, input, ppFuture, ppPastR, inputR, ppFutureR uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wparam  uintptr
	lparam  uintptr
	time    uint32
	x, y    int32
	_       uint32
}

type wndClass struct {
	size, style                uint32
	proc                       uintptr
	clsExtra, wndExtra         int32
	instance, icon, cursor, bg uintptr
	menu, name, iconSm         uintptr
}

type bitmapInfo struct {
	size                   uint32
	width, height          int32
	planes, bitCount       uint16
	compression, sizeImage uint32
	xppm, yppm             int32
	clrUsed, clrImportant  uint32
	colors                 [3]uint32
}

type paintStruct struct {
	hdc     uintptr
	erase   int32
	rc      winRect
	restore int32
	incUp   int32
	rgb     [32]byte
}

// The window procedure finds its window through this map.
var (
	winMu    sync.Mutex
	winByHwn = map[uintptr]*winDisplay{}
	winProc  uintptr
	winClass = syscall.StringToUTF16Ptr("GaalDisplay")
)

type winDisplay struct {
	d    *Display
	opts Options
	c    winCall // used only by the window thread
	hwnd uintptr
	wake uintptr // event

	want    atomic.Int32 // buffer to present, or -1
	closing atomic.Bool
	opened  chan error
	done    chan struct{}
	winW    atomic.Int32
	winH    atomic.Int32
	shownAt atomic.Int64
	work    chan func() // run on the window thread
	resized atomic.Bool

	mu   sync.Mutex // protects l and the path fields read by info and readback
	l    layout
	gdi  bool
	path string

	// GDI path.
	pixels  [3][]byte
	bmi     bitmapInfo
	cleared bool

	// Direct3D path. Owned by the window thread, except that fill writes
	// through the mapped pointers of staging textures it was given.
	dev, ctx, vdev, vctx       uintptr
	swap, swap2, latency       uintptr
	backView                   uintptr
	enum, vp, input, inputView uintptr
	staging                    [3]uintptr
	mapped                     [3]struct {
		data  uintptr
		pitch uint32
		depth uint32
	}
	even             gaal.VideoFrame
	msg              winMsg
	handles          [2]uintptr
	srcRect, dstRect winRect
	fullRect         winRect
	stream           vpStream
	presenting       bool
	presentAt        int64
	lastCopied       int
}

func openWindows(d *Display, o Options) (backend, error) {
	w := &winDisplay{d: d, opts: o, opened: make(chan error, 1), done: make(chan struct{}),
		work: make(chan func(), 4), lastCopied: -1}
	w.want.Store(-1)
	w.winW.Store(int32(o.Width))
	w.winH.Store(int32(o.Height))
	go w.thread()
	if err := <-w.opened; err != nil {
		return nil, err
	}
	return w, nil
}

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	winMu.Lock()
	w := winByHwn[hwnd]
	winMu.Unlock()
	if w != nil {
		switch msg {
		case 0x10: // WM_CLOSE
			w.d.e.events.push(Event{Kind: EventClose})
			return 0
		case 0x05: // WM_SIZE
			cw, ch := int(lparam&0xffff), int(lparam>>16&0xffff)
			if cw > 0 && ch > 0 {
				w.winW.Store(int32(cw))
				w.winH.Store(int32(ch))
				w.resized.Store(true)
				w.d.e.resized(cw, ch)
			}
			return 0
		case 0x14: // WM_ERASEBKGND
			return 1
		case 0x0F: // WM_PAINT
			var ps paintStruct
			var c winCall
			c.p(procBeginPaint, hwnd, esc(&ps))
			c.p(procEndPaint, hwnd, esc(&ps))
			w.resized.Store(true)
			w.d.e.redraw()
			return 0
		}
	}
	var c winCall
	return c.p(procDefWindowProc, hwnd, msg, wparam, lparam)
}

func (w *winDisplay) thread() {
	runtime.LockOSThread()
	defer close(w.done)
	if err := w.createWindow(); err != nil {
		w.opened <- err
		return
	}
	if !winDisableD3D {
		if err := w.initD3D(); err != nil {
			w.releaseD3D()
		}
	}
	w.mu.Lock()
	w.gdi = w.swap == 0
	w.path = "d3d11-videoprocessor-nv12"
	if w.gdi {
		w.path = "gdi-stretchdibits"
	}
	w.mu.Unlock()
	w.opened <- nil
	w.loop()
	w.releaseD3D()
	c := &w.c
	winMu.Lock()
	delete(winByHwn, w.hwnd)
	winMu.Unlock()
	c.p(procDestroyWindow, w.hwnd)
	c.p(procCloseHandle, w.wake)
}

func (w *winDisplay) createWindow() error {
	c := &w.c
	inst := c.p(procGetModuleHandle, 0)
	winMu.Lock()
	if winProc == 0 {
		winProc = syscall.NewCallback(wndProc)
		wc := wndClass{proc: winProc, instance: inst, name: esc(winClass), style: 3}
		wc.size = uint32(unsafe.Sizeof(wc))
		wc.cursor = c.p(procLoadCursor, 0, 32512)
		c.p(procRegisterClassEx, esc(&wc))
	}
	winMu.Unlock()
	const overlapped = 0x00CF0000
	const popup = 0x80000000
	style, x, y := uintptr(overlapped), uintptr(0x80000000), uintptr(0x80000000) // CW_USEDEFAULT
	r := winRect{0, 0, int32(w.opts.Width), int32(w.opts.Height)}
	if w.opts.Fullscreen {
		style, x, y = popup, 0, 0
		r.r, r.b = int32(c.p(procGetSystemMetrics, 0)), int32(c.p(procGetSystemMetrics, 1))
		w.winW.Store(r.r)
		w.winH.Store(r.b)
	} else {
		c.p(procAdjustWindowRect, esc(&r), style, 0, 0)
	}
	title, err := syscall.UTF16PtrFromString(w.opts.Title)
	if err != nil {
		return err
	}
	w.hwnd = c.p(procCreateWindowEx, 0, esc(winClass), esc(title), style,
		x, y, uintptr(r.r-r.l), uintptr(r.b-r.t), 0, 0, inst, 0)
	if w.hwnd == 0 {
		return errors.New("CreateWindowEx failed")
	}
	winMu.Lock()
	winByHwn[w.hwnd] = w
	winMu.Unlock()
	w.wake = c.p(procCreateEvent, 0, 0, 0, 0)
	c.p(procShowWindow, w.hwnd, 5)
	var rc winRect
	c.p(procGetClientRect, w.hwnd, esc(&rc))
	if rc.r > 0 && rc.b > 0 {
		w.winW.Store(rc.r)
		w.winH.Store(rc.b)
		w.d.e.resized(int(rc.r), int(rc.b))
	}
	return nil
}

func hr(name string, v uint32) error {
	if int32(v) < 0 {
		return fmt.Errorf("%s: 0x%08x", name, v)
	}
	return nil
}

func (w *winDisplay) initD3D() error {
	c := &w.c
	if err := procD3D11CreateDevice.Find(); err != nil {
		return err
	}
	const hardware, bgra, video = 1, 0x20, 0x800
	r := uint32(c.p(procD3D11CreateDevice, 0, hardware, 0, bgra|video, 0, 0, 7,
		uintptr(unsafe.Pointer(&w.dev)), 0, uintptr(unsafe.Pointer(&w.ctx))))
	if err := hr("D3D11CreateDevice", r); err != nil {
		return err
	}
	if err := hr("ID3D11VideoDevice", c.m(w.dev, slotQI, esc(&iidVideoDevice), uintptr(unsafe.Pointer(&w.vdev)))); err != nil {
		return err
	}
	if err := hr("ID3D11VideoContext", c.m(w.ctx, slotQI, esc(&iidVideoContext), uintptr(unsafe.Pointer(&w.vctx)))); err != nil {
		return err
	}
	var dxgi, adapter, factory uintptr
	defer comRelease(c, &dxgi)
	defer comRelease(c, &adapter)
	defer comRelease(c, &factory)
	if err := hr("IDXGIDevice", c.m(w.dev, slotQI, esc(&iidDXGIDevice), esc(&dxgi))); err != nil {
		return err
	}
	if err := hr("GetAdapter", c.m(dxgi, dxgiGetAdapter, esc(&adapter))); err != nil {
		return err
	}
	if err := hr("GetParent", c.m(adapter, dxgiGetParent, esc(&iidDXGIFactory2), esc(&factory))); err != nil {
		return err
	}
	desc := [12]uint32{uint32(w.winW.Load()), uint32(w.winH.Load()), dxgiFormatBGRA8, 0, 1, 0,
		0x20, 2, 0 /* stretch */, 4 /* flip discard */, 0, swapFlags}
	if err := hr("CreateSwapChainForHwnd", c.m(factory, factoryCreateSwapHwnd, w.dev, w.hwnd,
		esc(&desc), 0, 0, uintptr(unsafe.Pointer(&w.swap)))); err != nil {
		return err
	}
	if err := hr("IDXGISwapChain2", c.m(w.swap, slotQI, esc(&iidSwapChain2), uintptr(unsafe.Pointer(&w.swap2)))); err != nil {
		return err
	}
	c.m(w.swap2, swap2SetMaxLatency, 1)
	c.a[0] = w.swap2
	w.latency = c.call(comMethod(w.swap2, swap2GetLatencyWaitable), 1)
	if w.latency == 0 {
		return errors.New("no frame latency object")
	}
	c.p(procWaitSingle, w.latency, 1000)
	return nil
}

func (w *winDisplay) releaseVP() {
	c := &w.c
	for i := range w.staging {
		if w.staging[i] != 0 && w.mapped[i].data != 0 {
			c.m(w.ctx, ctxUnmap, w.staging[i], 0)
			w.mapped[i].data = 0
		}
		comRelease(c, &w.staging[i])
	}
	comRelease(c, &w.backView)
	comRelease(c, &w.inputView)
	comRelease(c, &w.input)
	comRelease(c, &w.vp)
	comRelease(c, &w.enum)
	w.lastCopied = -1
}

func (w *winDisplay) releaseD3D() {
	c := &w.c
	if w.ctx != 0 {
		w.releaseVP()
	}
	if w.latency != 0 {
		c.p(procCloseHandle, w.latency)
		w.latency = 0
	}
	comRelease(c, &w.swap2)
	comRelease(c, &w.swap)
	comRelease(c, &w.vctx)
	comRelease(c, &w.vdev)
	comRelease(c, &w.ctx)
	comRelease(c, &w.dev)
}

// outputView makes the view of the current back buffer.
func (w *winDisplay) outputView() error {
	c := &w.c
	comRelease(c, &w.backView)
	var tex uintptr
	if err := hr("GetBuffer", c.m(w.swap, swapGetBuffer, 0, esc(&iidTexture2D), esc(&tex))); err != nil {
		return err
	}
	defer comRelease(c, &tex)
	desc := [3]uint32{1, 0, 0} // D3D11_VPOV_DIMENSION_TEXTURE2D
	return hr("CreateVideoProcessorOutputView", c.m(w.vdev, vdCreateOutputView, tex, w.enum,
		esc(&desc), uintptr(unsafe.Pointer(&w.backView))))
}

// prepareD3D runs on the window thread.
func (w *winDisplay) prepareD3D(l layout) error {
	c := &w.c
	w.releaseVP()
	ww, wh := uint32(w.winW.Load()), uint32(w.winH.Load())
	content := [10]uint32{0, 60, 1, uint32(l.width), uint32(l.height), 60, 1, ww, wh, 0}
	if err := hr("CreateVideoProcessorEnumerator", c.m(w.vdev, vdCreateEnumerator,
		esc(&content), uintptr(unsafe.Pointer(&w.enum)))); err != nil {
		return err
	}
	if err := hr("CreateVideoProcessor", c.m(w.vdev, vdCreateProcessor, w.enum, 0, uintptr(unsafe.Pointer(&w.vp)))); err != nil {
		return err
	}
	tex := texDesc{w: uint32(l.width &^ 1), h: uint32(l.height &^ 1), mips: 1, array: 1, format: dxgiFormatNV12, samples: 1,
		bind: 0x20} // render target, which video processor input accepts
	if err := hr("CreateTexture2D", c.m(w.dev, devCreateTexture2D, esc(&tex), 0, uintptr(unsafe.Pointer(&w.input)))); err != nil {
		return err
	}
	inDesc := [4]uint32{0, 1, 0, 0} // FourCC, D3D11_VPIV_DIMENSION_TEXTURE2D, mip, slice
	if err := hr("CreateVideoProcessorInputView", c.m(w.vdev, vdCreateInputView, w.input, w.enum,
		esc(&inDesc), uintptr(unsafe.Pointer(&w.inputView)))); err != nil {
		return err
	}
	tex.usage, tex.bind, tex.cpu = 3, 0, 0x10000 // staging, CPU write
	for i := range w.staging {
		if err := hr("CreateTexture2D", c.m(w.dev, devCreateTexture2D, esc(&tex), 0, uintptr(unsafe.Pointer(&w.staging[i])))); err != nil {
			return err
		}
		if err := w.mapStaging(i); err != nil {
			return err
		}
	}
	if err := w.outputView(); err != nil {
		return err
	}
	// Bit fields of D3D11_VIDEO_PROCESSOR_COLOR_SPACE.
	cs := uint32(1 << 4) // nominal range 16-235
	if l.full {
		cs = 2 << 4
	}
	if l.matrix == BT709 {
		cs |= 1 << 2
	}
	c.m(w.vctx, vcSetStreamColorSpace, w.vp, 0, esc(&cs))
	output := uint32(0)
	c.m(w.vctx, vcSetOutputColorSpace, w.vp, esc(&output))
	c.m(w.vctx, vcSetStreamFrameFormat, w.vp, 0, 0)
	black := [4]float32{0, 0, 0, 1}
	c.m(w.vctx, vcSetOutputBackground, w.vp, 0, esc(&black))
	return nil
}

func (w *winDisplay) mapStaging(i int) error {
	return hr("Map", w.c.m(w.ctx, ctxMap, w.staging[i], 0, 2 /* write */, 0, uintptr(unsafe.Pointer(&w.mapped[i]))))
}

// onThread runs f on the window thread and waits for it.
func (w *winDisplay) onThread(f func() error) error {
	ch := make(chan error, 1)
	w.work <- func() { ch <- f() }
	var c winCall
	c.p(procSetEvent, w.wake)
	return <-ch
}

// prepare implements backend.
func (w *winDisplay) prepare(l layout) (int, error) {
	w.mu.Lock()
	w.l = l
	gdi := w.gdi
	w.mu.Unlock()
	if !gdi {
		err := w.onThread(func() error { w.abandonWanted(); return w.prepareD3D(l) })
		if err == nil {
			return len(w.staging), nil
		}
		w.onThread(func() error { w.releaseD3D(); return nil })
		w.mu.Lock()
		w.gdi, w.path = true, "gdi-stretchdibits"
		w.mu.Unlock()
	}
	w.onThread(func() error {
		w.abandonWanted()
		for i := range w.pixels {
			w.pixels[i] = make([]byte, 4*l.width*l.height)
		}
		w.bmi = bitmapInfo{width: int32(l.width), height: -int32(l.height), planes: 1, bitCount: 32}
		w.bmi.size = 40
		w.cleared = false
		return nil
	})
	return len(w.pixels), nil
}

// abandonWanted drops a frame that was submitted for the buffers about to be
// replaced, and tells the engine the display is free again.
func (w *winDisplay) abandonWanted() {
	if w.want.Swap(-1) >= 0 {
		w.d.e.frameDone()
	}
}

// fill implements backend.
func (w *winDisplay) fill(i int, f *gaal.VideoFrame) {
	if w.gdi {
		toBGRX(w.pixels[i], 4*f.Width, f, w.l.coefs())
		return
	}
	m := &w.mapped[i]
	pitch := int(m.pitch)
	h := f.Height &^ 1
	mem := unsafe.Slice((*byte)(winPtr(m.data)), pitch*h+pitch*h/2)
	e := &w.even
	e.Width, e.Height, e.Format = f.Width&^1, h, f.Format
	e.Y, e.U, e.V, e.YStride, e.UVStride = f.Y, f.U, f.V, f.YStride, f.UVStride
	toNV12(mem, pitch, mem[pitch*h:], pitch, e)
}

// submit implements backend.
func (w *winDisplay) submit(i int) error {
	w.shownAt.Store(w.d.e.submittedAt())
	w.want.Store(int32(i))
	var c winCall
	c.p(procSetEvent, w.wake)
	return nil
}

func (w *winDisplay) loop() {
	c := &w.c
	w.handles = [2]uintptr{w.wake, w.latency}
	count := uintptr(1)
	if w.latency != 0 {
		count = 2
	}
	const allInput = 0x04FF
	const inputAvailable = 4
	for !w.closing.Load() {
		r := uint32(c.p(procMsgWait, count, uintptr(unsafe.Pointer(&w.handles[0])), 0xFFFFFFFF, allInput, inputAvailable))
		if count == 2 && r == 1 && w.presenting { // the display can take a new frame
			w.presenting = false
			w.d.e.presented(w.presentAt, now())
			w.d.e.frameDone()
		}
		for len(w.work) > 0 {
			(<-w.work)()
		}
		for c.p(procPeekMessage, uintptr(unsafe.Pointer(&w.msg)), 0, 0, 0, 1) != 0 {
			c.p(procTranslateMessage, uintptr(unsafe.Pointer(&w.msg)))
			c.p(procDispatchMessage, uintptr(unsafe.Pointer(&w.msg)))
		}
		if w.swap != 0 && w.resized.Swap(false) {
			w.resizeSwap()
		}
		if i := w.want.Swap(-1); i >= 0 {
			w.present(int(i))
		}
	}
}

func (w *winDisplay) resizeSwap() {
	c := &w.c
	if w.vp == 0 {
		return
	}
	comRelease(c, &w.backView)
	c.m(w.ctx, ctxFlush)
	if hr("ResizeBuffers", c.m(w.swap, swapResizeBuffers, 0, uintptr(w.winW.Load()), uintptr(w.winH.Load()), 0, swapFlags)) == nil {
		if w.outputView() == nil {
			w.d.e.redraw()
		}
	}
}

func (w *winDisplay) present(i int) {
	at := w.shownAt.Load()
	w.mu.Lock()
	gdi, l := w.gdi, w.l
	w.mu.Unlock()
	if gdi {
		w.presentGDI(i, l)
		w.d.e.presented(at, now())
		w.d.e.frameDone()
		return
	}
	c := &w.c
	if w.vp == 0 || w.backView == 0 {
		w.d.e.frameDone()
		return
	}
	if at >= 0 && w.lastCopied != i || w.lastCopied < 0 {
		c.m(w.ctx, ctxUnmap, w.staging[i], 0)
		w.mapped[i].data = 0
		c.m(w.ctx, ctxCopyResource, w.input, w.staging[i])
		if err := w.mapStaging(i); err != nil {
			w.d.e.fail(err)
		}
		w.lastCopied = i
	}
	ww, wh := int(w.winW.Load()), int(w.winH.Load())
	sr, dr := place(w.opts.Scale, l.width&^1, l.height&^1, ww, wh)
	w.srcRect = winRect{int32(sr.x), int32(sr.y), int32(sr.x + sr.w), int32(sr.y + sr.h)}
	w.dstRect = winRect{int32(dr.x), int32(dr.y), int32(dr.x + dr.w), int32(dr.y + dr.h)}
	w.fullRect = winRect{0, 0, int32(ww), int32(wh)}
	c.m(w.vctx, vcSetStreamSourceRect, w.vp, 0, 1, uintptr(unsafe.Pointer(&w.srcRect)))
	c.m(w.vctx, vcSetStreamDestRect, w.vp, 0, 1, uintptr(unsafe.Pointer(&w.dstRect)))
	c.m(w.vctx, vcSetOutputTargetRect, w.vp, 1, uintptr(unsafe.Pointer(&w.fullRect)))
	w.stream = vpStream{enable: 1, input: w.inputView}
	r := c.m(w.vctx, vcBlt, w.vp, w.backView, 0, 1, uintptr(unsafe.Pointer(&w.stream)))
	if err := hr("VideoProcessorBlt", r); err != nil {
		w.d.e.fail(err)
		w.d.e.frameDone()
		return
	}
	if err := hr("Present", c.m(w.swap, swapPresent, 1, 0)); err != nil {
		w.d.e.fail(err)
		w.d.e.frameDone()
		return
	}
	// With flip-model swap chains, Direct3D 11 keeps buffer 0 pointing at the
	// current back buffer, so the output view stays valid.
	w.presenting, w.presentAt = true, at
}

func (w *winDisplay) presentGDI(i int, l layout) {
	c := &w.c
	hdc := c.p(procGetDC, w.hwnd)
	if hdc == 0 {
		return
	}
	ww, wh := int(w.winW.Load()), int(w.winH.Load())
	sr, dr := place(w.opts.Scale, l.width, l.height, ww, wh)
	if !w.cleared || w.resized.Swap(false) {
		c.p(procPatBlt, hdc, 0, 0, uintptr(ww), uintptr(wh), 0x00000042) // BLACKNESS
		w.cleared = true
	}
	const halftone, dibRGB, srccopy = 4, 0, 0x00CC0020
	c.p(procSetStretchBltMode, hdc, halftone)
	c.p(procSetBrushOrgEx, hdc, 0, 0, 0)
	c.p(procStretchDIBits, hdc, uintptr(dr.x), uintptr(dr.y), uintptr(dr.w), uintptr(dr.h),
		uintptr(sr.x), uintptr(l.height-sr.y-sr.h), uintptr(sr.w), uintptr(sr.h),
		uintptr(unsafe.Pointer(&w.pixels[i][0])), uintptr(unsafe.Pointer(&w.bmi)), dibRGB, srccopy)
	c.p(procReleaseDC, w.hwnd, hdc)
}

// info implements backend.
func (w *winDisplay) info() Info {
	w.mu.Lock()
	defer w.mu.Unlock()
	src := "submit"
	if !w.gdi {
		src = "swapchain"
	}
	return Info{Backend: "windows", Path: w.path, Converted: w.gdi, LatencySource: src}
}

// readback reads the window through GDI. A flip-model swap chain is not
// visible to GDI, so this only works on the GDI path.
func (w *winDisplay) readback() (*image.RGBA, ReadbackSource, error) {
	w.mu.Lock()
	gdi := w.gdi
	w.mu.Unlock()
	if !gdi {
		return nil, "", ErrUnsupported
	}
	var img *image.RGBA
	err := w.onThread(func() error {
		var c winCall
		ww, wh := int(w.winW.Load()), int(w.winH.Load())
		hdc := c.p(procGetDC, w.hwnd)
		mem := c.p(procCreateCompatDC, hdc)
		bmp := c.p(procCreateCompatBmp, hdc, uintptr(ww), uintptr(wh))
		old := c.p(procSelectObject, mem, bmp)
		c.p(procBitBlt, mem, 0, 0, uintptr(ww), uintptr(wh), hdc, 0, 0, 0x00CC0020)
		c.p(procSelectObject, mem, old)
		bi := bitmapInfo{width: int32(ww), height: -int32(wh), planes: 1, bitCount: 32}
		bi.size = 40
		buf := make([]byte, 4*ww*wh)
		n := c.p(procGetDIBits, mem, bmp, 0, uintptr(wh), uintptr(unsafe.Pointer(&buf[0])), esc(&bi), 0)
		c.p(procDeleteObject, bmp)
		c.p(procDeleteDC, mem)
		c.p(procReleaseDC, w.hwnd, hdc)
		if n == 0 {
			return errors.New("GetDIBits failed")
		}
		img = bgrxToRGBA(buf, 4*ww, ww, wh)
		return nil
	})
	return img, ReadbackWindow, err
}

// close implements backend.
func (w *winDisplay) close() error {
	w.closing.Store(true)
	var c winCall
	c.p(procSetEvent, w.wake)
	<-w.done
	return nil
}
