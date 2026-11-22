//go:build android

package e2e

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/aep/gaal"
	"github.com/aep/gaal/internal/dl"
)

// Window shows frames on an ANativeWindow supplied by the app.
type Window struct {
	c      dl.Caller
	win    uintptr
	w, h   int
	buffer [64]byte
	yv12   bool
	dst    gaal.VideoFrame
}

type windowAPI struct {
	acquire, release func(uintptr)
	geometry         func(win uintptr, w, h, format int32) int32
	lock, unlock     uintptr
}

var loadWindow = sync.OnceValues(func() (*windowAPI, error) {
	h, err := dl.Open("libandroid.so")
	if err != nil {
		return nil, err
	}
	a := &windowAPI{}
	return a, dl.BindAll(h, dl.Table{"ANativeWindow_acquire": &a.acquire, "ANativeWindow_release": &a.release,
		"ANativeWindow_setBuffersGeometry": &a.geometry, "ANativeWindow_lock": &a.lock,
		"ANativeWindow_unlockAndPost": &a.unlock})
})

// NewWindow wraps an ANativeWindow pointer.
func NewWindow(win uintptr, w, h int) (*Window, error) {
	a, err := loadWindow()
	if err != nil {
		return nil, err
	}
	// YV12 lets the display hardware do the colour conversion.
	const formatRGBX, formatYV12 = 2, 0x32315659
	d := &Window{win: win, w: w, h: h, yv12: true}
	if a.geometry(win, int32(w), int32(h), formatYV12) != 0 {
		d.yv12 = false
		if a.geometry(win, int32(w), int32(h), formatRGBX) != 0 {
			return nil, errors.New("window: cannot set buffer geometry")
		}
	}
	a.acquire(win)
	return d, nil
}

// Show implements Display.
func (d *Window) Show(f *gaal.VideoFrame) error {
	a, _ := loadWindow()
	if int32(d.c.C3(a.lock, d.win, uintptr(unsafe.Pointer(&d.buffer)), 0)) != 0 {
		return errors.New("window: lock failed")
	}
	// ANativeWindow_Buffer: width, height, stride, format, bits.
	pixels := int(*(*int32)(unsafe.Pointer(&d.buffer[8])))
	height := int(*(*int32)(unsafe.Pointer(&d.buffer[4])))
	format := *(*int32)(unsafe.Pointer(&d.buffer[12]))
	bits := *(*unsafe.Pointer)(unsafe.Pointer(&d.buffer[16]))
	if d.yv12 && format == 0x32315659 {
		cs := (pixels/2 + 15) &^ 15
		ch := (height + 1) / 2
		mem := unsafe.Slice((*byte)(bits), pixels*height+2*cs*ch)
		v := mem[pixels*height:]
		o := &d.dst
		o.Width, o.Height, o.Format = f.Width, f.Height, gaal.I420
		o.Y, o.V, o.U = mem, v[:cs*ch], v[cs*ch:]
		o.YStride, o.UVStride = pixels, cs
		err := f.ToI420(o)
		d.c.C1(a.unlock, d.win)
		return err
	}
	stride := pixels * 4
	dst := unsafe.Slice((*byte)(bits), stride*height)
	ToBGRX(f, dst, stride)
	for y := 0; y < f.Height; y++ {
		row := dst[y*stride : y*stride+4*f.Width]
		for x := 0; x < len(row); x += 4 {
			row[x], row[x+2] = row[x+2], row[x]
		}
	}
	d.c.C1(a.unlock, d.win)
	return nil
}

// Verify implements Display. A window cannot be read back from the app.
func (d *Window) Verify() (float64, error) {
	return 0, errors.New("check with adb screencap")
}

// Close implements Display.
func (d *Window) Close() error {
	a, _ := loadWindow()
	a.release(d.win)
	return nil
}
