package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// libjpeg-turbo through its TurboJPEG API. This is a SIMD software codec.
// Windows is excluded because C unsigned long is 32 bits there.

type tjAPI struct {
	initDec, initEnc func() uintptr
	destroy          func(uintptr) int32
	errStr           func(uintptr) string
	planeW, planeH   func(comp, size, sub int32) int32
	bufSize          func(w, h, sub int32) uintptr
	header           uintptr
	toPlanes         uintptr
	compress         uintptr
}

var loadTJ = sync.OnceValues(func() (*tjAPI, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("turbojpeg: not supported on Windows")
	}
	h, err := dl.Open(libNames("turbojpeg", "0")...)
	if err != nil {
		return nil, err
	}
	a := &tjAPI{}
	err = dl.BindAll(h, dl.Table{
		"tjInitDecompress": &a.initDec, "tjInitCompress": &a.initEnc, "tjDestroy": &a.destroy,
		"tjDecompressHeader3": &a.header, "tjDecompressToYUVPlanes": &a.toPlanes,
		"tjCompressFromYUVPlanes": &a.compress, "tjGetErrorStr2": &a.errStr,
		"tjPlaneWidth": &a.planeW, "tjPlaneHeight": &a.planeH, "tjBufSize": &a.bufSize,
	})
	return a, err
})

const (
	tjSamp420       = 2
	tjSampGray      = 3
	tjFlagNoRealloc = 1024
)

// tjState holds call arguments on the heap so calls do not allocate.
type tjState struct {
	c              dl.Caller
	api            *tjAPI
	h              uintptr
	w, ht, sub, cs int32
	planes         [3]unsafe.Pointer
	strides        [3]int32
	out            unsafe.Pointer
	size           uintptr
	y, u, v        []byte // native planes, when they differ from the frame's
}

func turboJPEGBackends() []backend {
	avail := func(*Library) error { _, err := loadTJ(); return err }
	var out []backend
	for _, c := range []Codec{JPEG} {
		dec := BackendInfo{"turbojpeg", c, Decode, false}
		enc := BackendInfo{"turbojpeg", c, Encode, false}
		out = append(out,
			backend{BackendInfo: dec, avail: avail, vdec: func(*Library, VideoConfig) (VideoDecoder, error) {
				a, err := loadTJ()
				if err != nil {
					return nil, err
				}
				t := &tjState{api: a, h: a.initDec()}
				if t.h == 0 {
					return nil, errors.New("turbojpeg: init failed")
				}
				s := &imageDecoder{info: dec, release: func() { a.destroy(t.h) }}
				s.decode = func(b []byte) (*VideoFrame, error) { return t.decode(&s.frames, b) }
				return s, nil
			}},
			backend{BackendInfo: enc, avail: avail, venc: func(_ *Library, cfg VideoConfig) (VideoEncoder, error) {
				a, err := loadTJ()
				if err != nil {
					return nil, err
				}
				t := &tjState{api: a, h: a.initEnc()}
				if t.h == 0 {
					return nil, errors.New("turbojpeg: init failed")
				}
				limit := int(a.bufSize(int32(cfg.Width), int32(cfg.Height), tjSamp420))
				s := &imageEncoder{info: enc, cfg: cfg, release: func() { a.destroy(t.h) }}
				s.encode = func(f *VideoFrame) (*EncodedPacket, error) {
					src, err := s.i420(f)
					if err != nil {
						return nil, err
					}
					p := s.packets.get(limit)
					if err := t.encode(src, p, cfg.Quality); err != nil {
						p.Release()
						return nil, err
					}
					return p, nil
				}
				return s, nil
			}})
	}
	return out
}

func (t *tjState) corrupt() error {
	return fmt.Errorf("%w: turbojpeg: %s", ErrCorrupt, t.api.errStr(t.h))
}

func (t *tjState) fail() error { return fmt.Errorf("turbojpeg: %s", t.api.errStr(t.h)) }

func (t *tjState) setPlanes(y, u, v []byte, ys, us int) {
	t.planes = [3]unsafe.Pointer{unsafe.Pointer(&y[0]), unsafe.Pointer(&u[0]), unsafe.Pointer(&v[0])}
	t.strides = [3]int32{int32(ys), int32(us), int32(us)}
}

func (t *tjState) decode(pool *FramePool, data []byte) (*VideoFrame, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty JPEG", ErrInvalid)
	}
	defer runtime.KeepAlive(data)
	a := t.api
	r := t.c.C7(a.header, t.h, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&t.w)), uintptr(unsafe.Pointer(&t.ht)), uintptr(unsafe.Pointer(&t.sub)), uintptr(unsafe.Pointer(&t.cs)))
	if int32(r) != 0 {
		return nil, t.corrupt()
	}
	f := pool.Get(int(t.w), int(t.ht), I420)
	// The luma plane is padded to whole chroma samples, at most to a multiple
	// of 4: other sizes need a native plane. planeW allocates, so it is only
	// asked then.
	yw, yh := f.Width, f.Height
	if t.w%4 != 0 || t.ht%4 != 0 {
		yw, yh = int(a.planeW(0, t.w, t.sub)), int(a.planeH(0, t.ht, t.sub))
	}
	padded := yw != f.Width || yh != f.Height
	y, ys := f.Y, f.YStride
	if padded {
		if cap(t.y) < yw*yh {
			t.y = make([]byte, yw*yh)
		}
		y, ys = t.y[:yw*yh], yw
	}
	var cw, ch int
	if t.sub == tjSamp420 {
		t.setPlanes(y, f.U, f.V, ys, f.UVStride)
	} else {
		// Other subsamplings decode into native planes and are resampled.
		if t.sub != tjSampGray {
			cw, ch = int(a.planeW(1, t.w, t.sub)), int(a.planeH(1, t.ht, t.sub))
		}
		if n := cw*ch + 1; cap(t.u) < n {
			t.u, t.v = make([]byte, n), make([]byte, n)
		}
		t.setPlanes(y, t.u[:1], t.v[:1], ys, cw)
	}
	r = t.c.C8(a.toPlanes, t.h, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&t.planes[0])), uintptr(t.w), uintptr(unsafe.Pointer(&t.strides[0])), uintptr(t.ht), 0)
	if int32(r) != 0 {
		f.Release()
		return nil, t.corrupt()
	}
	if padded {
		copyPlane(f.Y, f.YStride, y, ys, f.Width, f.Height)
	}
	if t.sub != tjSamp420 {
		t.resample(f, cw, ch)
	}
	return f, nil
}

func (t *tjState) resample(f *VideoFrame, cw, ch int) {
	dw, dh := (f.Width+1)/2, (f.Height+1)/2
	u, v := t.u[:cap(t.u)], t.v[:cap(t.v)]
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			i := y*f.UVStride + x
			if t.sub == tjSampGray {
				f.U[i], f.V[i] = 128, 128
				continue
			}
			j := (y*ch/dh)*cw + x*cw/dw
			f.U[i], f.V[i] = u[j], v[j]
		}
	}
}

// encode compresses directly into the packet's buffer.
func (t *tjState) encode(f *VideoFrame, p *EncodedPacket, quality int) error {
	t.setPlanes(f.Y, f.U, f.V, f.YStride, f.UVStride)
	t.out, t.size = unsafe.Pointer(&p.Data[0]), uintptr(len(p.Data))
	r := t.c.C10(t.api.compress, t.h, uintptr(unsafe.Pointer(&t.planes[0])), uintptr(f.Width),
		uintptr(unsafe.Pointer(&t.strides[0])), uintptr(f.Height), tjSamp420,
		uintptr(unsafe.Pointer(&t.out)), uintptr(unsafe.Pointer(&t.size)), uintptr(quality), tjFlagNoRealloc)
	runtime.KeepAlive(f)
	t.out = nil
	if int32(r) != 0 {
		return t.fail()
	}
	p.Data = p.Data[:t.size]
	return nil
}
