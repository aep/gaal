package camera

import (
	"fmt"

	"github.com/aep/gaal"
)

// fourcc builds a V4L2 pixel format code.
func fourcc(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

var (
	pixMJPEG  = fourcc('M', 'J', 'P', 'G')
	pixJPEG   = fourcc('J', 'P', 'E', 'G')
	pixYUYV   = fourcc('Y', 'U', 'Y', 'V')
	pixUYVY   = fourcc('U', 'Y', 'V', 'Y')
	pixYVYU   = fourcc('Y', 'V', 'Y', 'U')
	pixNV12   = fourcc('N', 'V', '1', '2')
	pixNV21   = fourcc('N', 'V', '2', '1')
	pixNV12M  = fourcc('N', 'M', '1', '2')
	pixNV21M  = fourcc('N', 'M', '2', '1')
	pixYUV420 = fourcc('Y', 'U', '1', '2')
	pixYVU420 = fourcc('Y', 'V', '1', '2')
	pixYUV42M = fourcc('Y', 'M', '1', '2')
	pixGREY   = fourcc('G', 'R', 'E', 'Y')
)

// preferredFormats are the pixel formats read, best first. MJPEG keeps USB
// bandwidth low at high resolutions; the raw formats cover SoC pipelines.
var preferredFormats = []uint32{
	pixMJPEG, pixJPEG,
	pixYUYV, pixUYVY, pixYVYU,
	pixNV12, pixNV21, pixNV12M, pixNV21M,
	pixYUV420, pixYVU420, pixYUV42M,
	pixGREY,
}

func isJPEG(f uint32) bool { return f == pixMJPEG || f == pixJPEG }

func fourccString(f uint32) string {
	b := []byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)}
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// rawFrame is one captured raw picture: a byte slice per memory plane and the
// driver's bytes per line of each (0: tightly packed).
type rawFrame struct {
	format        uint32
	width, height int
	planes        [][]byte
	strides       []int
}

func (f *rawFrame) stride(i, minimum int) int {
	if i < len(f.strides) && f.strides[i] >= minimum {
		return f.strides[i]
	}
	return minimum
}

func (f *rawFrame) short(plane int) error {
	return fmt.Errorf("camera: short %s frame (plane %d: %d bytes for %dx%d)",
		fourccString(f.format), plane, len(f.planes[plane]), f.width, f.height)
}

// convert copies a raw picture into a pooled frame without colour
// conversion: NV12 and NV21 become NV12, the others I420.
func convert(f *rawFrame, pool *gaal.FramePool) (*gaal.VideoFrame, error) {
	if len(f.planes) == 0 {
		return nil, fmt.Errorf("camera: frame has no planes")
	}
	w, h := f.width, f.height
	ch := (h + 1) / 2
	switch f.format {
	case pixYUYV:
		return packed422(f, pool, 0, 1, 2, 3)
	case pixUYVY:
		return packed422(f, pool, 1, 0, 3, 2)
	case pixYVYU:
		return packed422(f, pool, 0, 3, 2, 1)

	case pixNV12, pixNV21:
		ys := f.stride(0, w)
		y := f.planes[0]
		if len(y) < ys*h {
			return nil, f.short(0)
		}
		return semiPlanar(f, pool, y[:ys*h], ys, y[ys*h:], ys, f.format == pixNV21)
	case pixNV12M, pixNV21M:
		if len(f.planes) < 2 {
			return nil, fmt.Errorf("camera: %s needs 2 planes, got %d", fourccString(f.format), len(f.planes))
		}
		ys := f.stride(0, w)
		return semiPlanar(f, pool, f.planes[0], ys, f.planes[1], f.stride(1, ys), f.format == pixNV21M)

	case pixYUV420, pixYVU420:
		ys := f.stride(0, w)
		cs := (ys + 1) / 2
		p := f.planes[0]
		ySize, cSize := ys*h, cs*ch
		if len(p) < ySize+2*cSize {
			return nil, f.short(0)
		}
		u, v := p[ySize:ySize+cSize], p[ySize+cSize:ySize+2*cSize]
		if f.format == pixYVU420 {
			u, v = v, u
		}
		return planar(f, pool, p[:ySize], ys, u, v, cs)
	case pixYUV42M:
		if len(f.planes) < 3 {
			return nil, fmt.Errorf("camera: YM12 needs 3 planes, got %d", len(f.planes))
		}
		return planar(f, pool, f.planes[0], f.stride(0, w), f.planes[1], f.planes[2], f.stride(1, (w+1)/2))

	case pixGREY:
		s := f.stride(0, w)
		if len(f.planes[0]) < s*(h-1)+w {
			return nil, f.short(0)
		}
		out := pool.Get(w, h, gaal.I420)
		for row := range h {
			copy(out.Y[row*out.YStride:row*out.YStride+w], f.planes[0][row*s:])
		}
		fill(out.U, 128)
		fill(out.V, 128)
		return out, nil
	}
	return nil, fmt.Errorf("camera: unsupported pixel format %s", fourccString(f.format))
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

// packed422 converts packed 4:2:2 given the byte offsets of Y0, U, Y1 and V in
// each 4-byte macropixel. Chroma of each pair of rows is averaged.
func packed422(f *rawFrame, pool *gaal.FramePool, y0, u, y1, v int) (*gaal.VideoFrame, error) {
	w, h := f.width, f.height
	cw := (w + 1) / 2
	data := f.planes[0]
	stride := f.stride(0, cw*4)
	if len(data) < stride*(h-1)+cw*4 {
		return nil, f.short(0)
	}
	out := pool.Get(w, h, gaal.I420)
	for row := range h {
		src := data[row*stride : row*stride+cw*4]
		yd := out.Y[row*out.YStride : row*out.YStride+w]
		for x := 0; x+1 < w; x += 2 {
			m := src[x*2 : x*2+4]
			yd[x], yd[x+1] = m[y0], m[y1]
		}
		if w%2 == 1 {
			yd[w-1] = src[(w-1)*2+y0]
		}
	}
	for cy := range (h + 1) / 2 {
		a := data[2*cy*stride : 2*cy*stride+cw*4]
		b := a
		if r := 2*cy + 1; r < h {
			b = data[r*stride : r*stride+cw*4]
		}
		ud := out.U[cy*out.UVStride : cy*out.UVStride+cw]
		vd := out.V[cy*out.UVStride : cy*out.UVStride+cw]
		for x := range cw {
			ud[x] = byte((uint(a[4*x+u]) + uint(b[4*x+u]) + 1) >> 1)
			vd[x] = byte((uint(a[4*x+v]) + uint(b[4*x+v]) + 1) >> 1)
		}
	}
	return out, nil
}

// semiPlanar copies NV12, or NV21 with its chroma swapped, into an NV12 frame.
func semiPlanar(f *rawFrame, pool *gaal.FramePool, y []byte, yStride int, uv []byte, uvStride int, swap bool) (*gaal.VideoFrame, error) {
	w, h := f.width, f.height
	cw, ch := (w+1)/2, (h+1)/2
	if len(y) < yStride*(h-1)+w || len(uv) < uvStride*(ch-1)+cw*2 {
		return nil, fmt.Errorf("camera: short 4:2:0 semi-planar frame (y=%d uv=%d bytes for %dx%d)", len(y), len(uv), w, h)
	}
	out := pool.Get(w, h, gaal.NV12)
	for row := range h {
		copy(out.Y[row*out.YStride:row*out.YStride+w], y[row*yStride:])
	}
	for row := range ch {
		src := uv[row*uvStride : row*uvStride+cw*2]
		dst := out.U[row*out.UVStride : row*out.UVStride+cw*2]
		if !swap {
			copy(dst, src)
			continue
		}
		for x := 0; x < len(dst); x += 2 {
			dst[x], dst[x+1] = src[x+1], src[x]
		}
	}
	return out, nil
}

// planar copies I420.
func planar(f *rawFrame, pool *gaal.FramePool, y []byte, yStride int, u, v []byte, cStride int) (*gaal.VideoFrame, error) {
	w, h := f.width, f.height
	cw, ch := (w+1)/2, (h+1)/2
	need := cStride*(ch-1) + cw
	if len(y) < yStride*(h-1)+w || len(u) < need || len(v) < need {
		return nil, fmt.Errorf("camera: short 4:2:0 planar frame (%dx%d)", w, h)
	}
	out := pool.Get(w, h, gaal.I420)
	for row := range h {
		copy(out.Y[row*out.YStride:row*out.YStride+w], y[row*yStride:])
	}
	for row := range ch {
		copy(out.U[row*out.UVStride:row*out.UVStride+cw], u[row*cStride:])
		copy(out.V[row*out.UVStride:row*out.UVStride+cw], v[row*cStride:])
	}
	return out, nil
}
