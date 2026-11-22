package display

import (
	"image"
	"math"
	"unsafe"

	"github.com/aep/gaal"
)

// coefs holds fixed-point YCbCr to RGB factors. The assembly reads this layout.
//
// With c8 = (C-128)<<8 and y7 = Y<<7 - yOff, and mul(a,b) = (a*b+0x4000)>>15:
//
//	l = mul(y7, yMul) + round
//	B = (l + mul(u8, bu)) >> 6
//	G = (l - mul(u8, gu) - mul(v8, gv)) >> 6
//	R = (l + mul(v8, rv)) >> 6
//
// Additions saturate to int16 and results clamp to 0-255. The SIMD kernels
// produce exactly the same values as the scalar code.
type coefs struct {
	yOff, yMul, bu, gu, gv, rv, round, alpha int16
}

func makeCoefs(m Matrix, full bool) coefs {
	kr, kb := 0.299, 0.114
	if m == BT709 {
		kr, kb = 0.2126, 0.0722
	}
	kg := 1 - kr - kb
	ys, cs, off := 255.0/219, 255.0/224, 16
	if full {
		ys, cs, off = 1, 1, 0
	}
	fix := func(v, unit float64) int16 { return int16(math.Round(v * unit)) }
	return coefs{
		yOff:  int16(off << 7),
		yMul:  fix(ys, 1<<14),
		bu:    fix(2*(1-kb)*cs, 1<<13),
		gu:    fix(2*(1-kb)*kb/kg*cs, 1<<13),
		gv:    fix(2*(1-kr)*kr/kg*cs, 1<<13),
		rv:    fix(2*(1-kr)*cs, 1<<13),
		round: 32,
		alpha: 255,
	}
}

var coefTable = [4]coefs{
	makeCoefs(BT601, false), makeCoefs(BT601, true),
	makeCoefs(BT709, false), makeCoefs(BT709, true),
}

func coefsFor(m Matrix, full bool) *coefs {
	i := 0
	if m == BT709 {
		i = 2
	}
	if full {
		i++
	}
	return &coefTable[i]
}

func mulhrs(a, b int32) int32 { return (a*b + 0x4000) >> 15 }

func sat16(v int32) int32 { return min(max(v, -32768), 32767) }

func clamp8(v int32) byte { return byte(min(max(v, 0), 255)) }

// pixel converts one sample with the same arithmetic as the SIMD kernels.
func (c *coefs) pixel(y, u, v byte) (b, g, r byte) {
	l := mulhrs(int32(y)<<7-int32(c.yOff), int32(c.yMul)) + int32(c.round)
	u8 := (int32(u) - 128) << 8
	v8 := (int32(v) - 128) << 8
	bv := sat16(l + mulhrs(u8, int32(c.bu)))
	gv := sat16(sat16(l-mulhrs(u8, int32(c.gu))) - mulhrs(v8, int32(c.gv)))
	rv := sat16(l + mulhrs(v8, int32(c.rv)))
	return clamp8(bv >> 6), clamp8(gv >> 6), clamp8(rv >> 6)
}

// i420RowGo writes n BGRX pixels. The chroma index is x>>1 from the start of
// the row, and start is the first pixel to convert.
func i420RowGo(dst, y, u, v []byte, start, n int, c *coefs) {
	for x := start; x < n; x++ {
		b, g, r := c.pixel(y[x], u[x>>1], v[x>>1])
		p := dst[x*4 : x*4+4 : x*4+4]
		p[0], p[1], p[2], p[3] = b, g, r, 255
	}
}

func nv12RowGo(dst, y, uv []byte, start, n int, c *coefs) {
	for x := start; x < n; x++ {
		b, g, r := c.pixel(y[x], uv[x&^1], uv[x|1])
		p := dst[x*4 : x*4+4 : x*4+4]
		p[0], p[1], p[2], p[3] = b, g, r, 255
	}
}

// toBGRX converts f to 32-bit pixels with bytes B, G, R, 255.
func toBGRX(dst []byte, stride int, f *gaal.VideoFrame, c *coefs) {
	w, h := f.Width, f.Height
	cw := (w + 1) / 2
	whole := w &^ 15
	for r := 0; r < h; r++ {
		out := dst[r*stride : r*stride+4*w]
		y := f.Y[r*f.YStride : r*f.YStride+w]
		co := (r / 2) * f.UVStride
		if f.Format == gaal.NV12 {
			uv := f.U[co : co+2*cw]
			if whole > 0 && useSIMD {
				nv12Kernel(&out[0], &y[0], &uv[0], whole, c)
				nv12RowGo(out, y, uv, whole, w, c)
			} else {
				nv12RowGo(out, y, uv, 0, w, c)
			}
			continue
		}
		u, v := f.U[co:co+cw], f.V[co:co+cw]
		if whole > 0 && useSIMD {
			i420Kernel(&out[0], &y[0], &u[0], &v[0], whole, c)
			i420RowGo(out, y, u, v, whole, w, c)
		} else {
			i420RowGo(out, y, u, v, 0, w, c)
		}
	}
}

// bgrxToRGBA converts rows of BGRX pixels to an opaque image.
func bgrxToRGBA(src []byte, stride, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		s := src[y*stride : y*stride+4*w]
		d := img.Pix[y*img.Stride : y*img.Stride+4*w]
		for x := 0; x < 4*w; x += 4 {
			d[x], d[x+1], d[x+2], d[x+3] = s[x+2], s[x+1], s[x], 255
		}
	}
	return img
}

func copyPlane(dst []byte, dstStride int, src []byte, srcStride, width, height int) {
	if dstStride == width && srcStride == width {
		copy(dst[:width*height], src[:width*height])
		return
	}
	for y := 0; y < height; y++ {
		copy(dst[y*dstStride:y*dstStride+width], src[y*srcStride:y*srcStride+width])
	}
}

func interleaveUV(dst, u, v []byte) {
	n := len(u)
	done := 0
	if whole := n &^ 15; whole > 0 && simdPlanes {
		interleaveUV16(&dst[0], &u[0], &v[0], whole)
		done = whole
	}
	for i := done; i < n; i++ {
		dst[2*i], dst[2*i+1] = u[i], v[i]
	}
}

func deinterleaveUV(src, u, v []byte) {
	n := len(u)
	done := 0
	if whole := n &^ 15; whole > 0 && simdPlanes {
		deinterleaveUV16(&src[0], &u[0], &v[0], whole)
		done = whole
	}
	for i := done; i < n; i++ {
		u[i], v[i] = src[2*i], src[2*i+1]
	}
}

// Factors of compressPlane that turn full-range samples into limited range.
const (
	lumaM, lumaK     = 219, 16*255 + 127
	chromaM, chromaK = 224, 128*255 - 128*224 + 127
)

// compressPlane replaces each byte b with (b*m + k) / 255.
func compressPlane(p []byte, m, k uint16) {
	done := 0
	if whole := len(p) &^ 15; whole > 0 && haveCompress {
		compressKernel(&p[0], whole, m, k)
		done = whole
	}
	for i := done; i < len(p); i++ {
		v := uint32(p[i])*uint32(m) + uint32(k)
		p[i] = byte((v + v>>8 + 1) >> 8)
	}
}

// toNV12 writes f into NV12 planes.
func toNV12(y []byte, yStride int, uv []byte, uvStride int, f *gaal.VideoFrame) {
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	copyPlane(y, yStride, f.Y, f.YStride, f.Width, f.Height)
	if f.Format == gaal.NV12 {
		copyPlane(uv, uvStride, f.U, f.UVStride, 2*cw, ch)
		return
	}
	for r := 0; r < ch; r++ {
		co := r * f.UVStride
		interleaveUV(uv[r*uvStride:r*uvStride+2*cw], f.U[co:co+cw], f.V[co:co+cw])
	}
}

// toPlanar writes f into separate Y, U and V planes. Pass V before U for YV12.
func toPlanar(y []byte, yStride int, u, v []byte, uvStride int, f *gaal.VideoFrame) {
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	copyPlane(y, yStride, f.Y, f.YStride, f.Width, f.Height)
	if f.Format == gaal.I420 {
		copyPlane(u, uvStride, f.U, f.UVStride, cw, ch)
		copyPlane(v, uvStride, f.V, f.UVStride, cw, ch)
		return
	}
	for r := 0; r < ch; r++ {
		co := r * f.UVStride
		o := r * uvStride
		deinterleaveUV(f.U[co:co+2*cw], u[o:o+cw], v[o:o+cw])
	}
}

// scaleBGRX draws the src rectangle of a 32-bit image into the dst rectangle of
// another with nearest-neighbour sampling, and paints the rest of the target
// black. It is the last resort when nothing else can scale.
func scaleBGRX(dst []byte, dstStride, dstW, dstH int, src []byte, srcStride int, sr, dr rect) {
	dp := unsafe.Slice((*uint32)(unsafe.Pointer(&dst[0])), len(dst)/4)
	sp := unsafe.Slice((*uint32)(unsafe.Pointer(&src[0])), len(src)/4)
	stepX := (sr.w << 16) / max(dr.w, 1)
	stepY := (sr.h << 16) / max(dr.h, 1)
	for y := 0; y < dstH; y++ {
		row := dp[y*dstStride/4 : y*dstStride/4+dstW]
		if y < dr.y || y >= dr.y+dr.h {
			clear(row)
			continue
		}
		sy := sr.y + ((y-dr.y)*stepY+stepY/2)>>16
		in := sp[sy*srcStride/4+sr.x:]
		clear(row[:dr.x])
		sx := stepX / 2
		out := row[dr.x : dr.x+dr.w]
		for i := range out {
			out[i] = in[sx>>16]
			sx += stepX
		}
		clear(row[dr.x+dr.w:])
	}
}
