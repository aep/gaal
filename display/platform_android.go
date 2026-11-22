package display

import (
	"errors"
	"image"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/aep/gaal"
	"github.com/aep/gaal/internal/dl"
)

func platformBackends() []opener { return []opener{{"android", openAndroid}} }

const (
	androidYV12 = 0x32315659
	androidRGBX = 2

	// ADataSpace values.
	dsBT601Limited = 0x10C20000
	dsBT709Limited = 0x10C10000
)

type androidAPI struct {
	acquire, release, geometry, lock, unlock, getWidth, getHeight, dataSpace uintptr
}

var loadAndroid = sync.OnceValues(func() (*androidAPI, error) {
	h, err := dl.Open("libandroid.so")
	if err != nil {
		return nil, err
	}
	a := &androidAPI{}
	err = dl.BindAll(h, dl.Table{"ANativeWindow_acquire": &a.acquire, "ANativeWindow_release": &a.release,
		"ANativeWindow_setBuffersGeometry": &a.geometry, "ANativeWindow_lock": &a.lock,
		"ANativeWindow_unlockAndPost": &a.unlock, "ANativeWindow_getWidth": &a.getWidth,
		"ANativeWindow_getHeight": &a.getHeight})
	if err != nil {
		return nil, err
	}
	a.dataSpace, _ = dl.Sym(h, "ANativeWindow_setBuffersDataSpace") // API 28
	return a, nil
})

// androidJob is one frame for the presenter. It carries its own layout, because
// prepare can replace the staging frames while a frame is under way.
type androidJob struct {
	frame   *gaal.VideoFrame
	l       layout
	shownAt int64
}

// android shows frames on an ANativeWindow. Show copies into a staging frame and
// a presenter goroutine, which may block in ANativeWindow_lock until the display
// frees a buffer, copies the newest one into the window.
//
// The window scales buffers to its own size, so aspect ratio is kept by making
// the buffer the shape of the window, with black bars inside it.
type android struct {
	d     *Display
	a     *androidAPI
	win   uintptr
	c     dl.Caller // used only by the presenter, after open
	work  chan androidJob
	stop  chan struct{}
	done  chan struct{}
	dirty atomic.Bool // geometry must be set again

	stage [3]*gaal.VideoFrame
	l     layout

	// Owned by the presenter.
	yv12     bool
	geoSet   bool
	geoFor   layout
	bw, bh   int
	src, dst rect
	buffer   [64]byte
	rgb      []byte
	closing  atomic.Bool
}

func openAndroid(d *Display, o Options) (backend, error) {
	if o.NativeWindow == 0 {
		return nil, errors.New("Options.NativeWindow is required on Android")
	}
	a, err := loadAndroid()
	if err != nil {
		return nil, err
	}
	b := &android{d: d, a: a, win: o.NativeWindow, work: make(chan androidJob, 1),
		stop: make(chan struct{}), done: make(chan struct{})}
	b.c.C1(a.acquire, b.win)
	b.dirty.Store(true)
	go b.run()
	return b, nil
}

// windowChanged implements windowChanger.
func (b *android) windowChanged() { b.dirty.Store(true) }

func (b *android) prepare(l layout) (int, error) {
	b.l = l
	for i := range b.stage {
		b.stage[i] = gaal.NewVideoFrame(l.width, l.height, gaal.I420)
	}
	return len(b.stage), nil
}

func (b *android) fill(i int, f *gaal.VideoFrame) {
	s := b.stage[i]
	toPlanar(s.Y, s.YStride, s.U, s.V, s.UVStride, f)
	if b.l.full {
		// Composers can ignore the range in the dataspace of YV12 buffers: on the
		// device this was tested on, full range came out wrong by about 20
		// levels while limited range was exact. So the samples are converted.
		compressPlane(s.Y, lumaM, lumaK)
		compressPlane(s.U, chromaM, chromaK)
		compressPlane(s.V, chromaM, chromaK)
	}
}

func (b *android) submit(i int) error {
	b.work <- androidJob{b.stage[i], b.l, b.d.e.submittedAt()}
	return nil
}

func (b *android) run() {
	defer close(b.done)
	for {
		select {
		case <-b.stop:
			return
		case job := <-b.work:
			if err := b.present(job.frame, job.l); err != nil {
				if !b.closing.Load() {
					b.d.e.fail(err)
				}
				b.d.e.frameDone()
				continue
			}
			b.d.e.presented(job.shownAt, now())
			b.d.e.frameDone()
		}
	}
}

func even(n int) int { return (n + 1) &^ 1 }

// bufferShape returns the buffer size and the rectangles to use for a frame in
// a window of the given size.
func bufferShape(mode ScaleMode, fw, fh, ww, wh int) (bw, bh int, src, dst rect) {
	src = rect{0, 0, fw, fh}
	bw, bh = fw, fh
	if ww > 0 && wh > 0 && mode != ScaleStretch {
		wider := ww*fh > wh*fw
		switch {
		case mode == ScaleFit && wider:
			bw = even(fh * ww / wh)
		case mode == ScaleFit:
			bh = even(fw * wh / ww)
		case wider:
			bh = even(fw * wh / ww)
			src.h, src.y = bh, (fh-bh)/2&^1
		default:
			bw = even(fh * ww / wh)
			src.w, src.x = bw, (fw-bw)/2&^1
		}
	}
	if mode == ScaleFill {
		return bw, bh, src, rect{0, 0, bw, bh}
	}
	dst = rect{(bw - fw) / 2 &^ 1, (bh - fh) / 2 &^ 1, fw, fh}
	return bw, bh, src, dst
}

func (b *android) configure(l layout) error {
	a := b.a
	geo := func(w, h int, f uintptr) bool {
		return int32(b.c.C5(a.geometry, b.win, uintptr(w), uintptr(h), f, 0)) == 0
	}
	if !geo(0, 0, androidYV12) {
		geo(0, 0, androidRGBX)
	}
	ww, wh := int(int32(b.c.C1(a.getWidth, b.win))), int(int32(b.c.C1(a.getHeight, b.win)))
	if ww > 0 && wh > 0 {
		b.d.e.resized(ww, wh)
	}
	b.bw, b.bh, b.src, b.dst = bufferShape(b.d.e.opts.Scale, l.width, l.height, ww, wh)
	b.yv12 = geo(b.bw, b.bh, androidYV12)
	if !b.yv12 {
		if !geo(b.bw, b.bh, androidRGBX) {
			return errors.New("android: cannot set buffer geometry")
		}
		b.rgb = make([]byte, 4*l.width*l.height)
	}
	if a.dataSpace != 0 && b.yv12 {
		ds := uintptr(dsBT601Limited)
		if l.matrix == BT709 {
			ds = dsBT709Limited
		}
		b.c.C2(a.dataSpace, b.win, ds)
	}
	b.geoFor, b.geoSet = l, true
	return nil
}

// copyWindowed writes one plane of the source rectangle into dst at the given
// position and fills everything around it with v.
func copyWindowed(dst []byte, dstStride, bw, bh int, src []byte, srcStride int, sr, dr rect, v byte) {
	for y := 0; y < bh; y++ {
		row := dst[y*dstStride : y*dstStride+bw]
		if y < dr.y || y >= dr.y+dr.h {
			for i := range row {
				row[i] = v
			}
			continue
		}
		for i := 0; i < dr.x; i++ {
			row[i] = v
		}
		s := (y-dr.y+sr.y)*srcStride + sr.x
		copy(row[dr.x:dr.x+dr.w], src[s:s+dr.w])
		for i := dr.x + dr.w; i < bw; i++ {
			row[i] = v
		}
	}
}

func half(r rect) rect { return rect{r.x / 2, r.y / 2, (r.w + 1) / 2, (r.h + 1) / 2} }

func (b *android) present(s *gaal.VideoFrame, l layout) error {
	a := b.a
	if b.dirty.Swap(false) || !b.geoSet || b.geoFor != l {
		if err := b.configure(l); err != nil {
			return err
		}
	}
	if int32(b.c.C3(a.lock, b.win, uintptr(unsafe.Pointer(&b.buffer)), 0)) != 0 {
		return errors.New("android: cannot lock the window")
	}
	// ANativeWindow_Buffer: width, height, stride, format, bits.
	bufW := int(*(*int32)(unsafe.Pointer(&b.buffer[0])))
	bufH := int(*(*int32)(unsafe.Pointer(&b.buffer[4])))
	stride := int(*(*int32)(unsafe.Pointer(&b.buffer[8])))
	format := *(*int32)(unsafe.Pointer(&b.buffer[12]))
	bits := *(*unsafe.Pointer)(unsafe.Pointer(&b.buffer[16]))
	bw, bh := min(b.bw, bufW), min(b.bh, bufH)
	sr, dr := b.src, b.dst
	dr.w, dr.h = min(dr.w, bw-dr.x), min(dr.h, bh-dr.y)
	sr.w, sr.h = min(sr.w, dr.w), min(sr.h, dr.h)
	if format == androidYV12 {
		cs := (stride/2 + 15) &^ 15
		ch := (bufH + 1) / 2
		mem := unsafe.Slice((*byte)(bits), stride*bufH+2*cs*ch)
		v := mem[stride*bufH:]
		u := v[cs*ch:]
		copyWindowed(mem, stride, bw, bh, s.Y, s.YStride, sr, dr, 16)
		cw, chh := (bw+1)/2, (bh+1)/2
		copyWindowed(u, cs, cw, chh, s.U, s.UVStride, half(sr), half(dr), 128)
		copyWindowed(v, cs, cw, chh, s.V, s.UVStride, half(sr), half(dr), 128)
	} else {
		p := 4 * stride
		mem := unsafe.Slice((*byte)(bits), p*bufH)
		// The stage holds limited range samples, as fill stored them.
		toBGRX(b.rgb, 4*s.Width, s, coefsFor(l.matrix, false))
		scaleBGRX(mem, p, bw, bh, b.rgb, 4*s.Width, sr, dr)
		for y := 0; y < bh; y++ {
			row := mem[y*p : y*p+4*bw]
			for x := 0; x < len(row); x += 4 {
				row[x], row[x+2] = row[x+2], row[x]
			}
		}
	}
	b.c.C1(a.unlock, b.win)
	return nil
}

func (b *android) info() Info {
	path := "anativewindow-yv12"
	conv := false
	if b.geoSet && !b.yv12 {
		path, conv = "anativewindow-rgbx", true
	}
	return Info{Backend: "android", Path: path, Converted: conv, LatencySource: "submit"}
}

func (b *android) readback() (*image.RGBA, ReadbackSource, error) {
	return nil, "", ErrUnsupported // use "adb exec-out screencap" instead
}

func (b *android) close() error {
	b.closing.Store(true)
	close(b.stop)
	<-b.done
	b.c.C1(b.a.release, b.win)
	return nil
}
