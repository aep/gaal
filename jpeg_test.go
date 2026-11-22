package gaal

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"
)

// jpegGradient is a 4:2:0 JPEG as Go encodes it, with the standard tables.
func jpegGradient(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x + y) * 255 / (w + h)), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jpegStripTables drops every DHT segment before the scan, as UVC cameras send
// images.
func jpegStripTables(t *testing.T, b []byte) []byte {
	t.Helper()
	out := append([]byte(nil), b[:2]...)
	for i := 2; ; {
		m := b[i+1]
		if m == 0xDA {
			return append(out, b[i:]...)
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if m != 0xC4 {
			out = append(out, b[i:i+2+n]...)
		}
		i += 2 + n
	}
}

func TestCompleteJPEG(t *testing.T) {
	full := jpegGradient(t, 64, 48)
	want, err := jpeg.Decode(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	bare := jpegStripTables(t, full)
	if JPEGNeedsTables(full) || !JPEGNeedsTables(bare) {
		t.Fatal("tables not detected")
	}
	if _, err := jpeg.Decode(bytes.NewReader(bare)); err == nil {
		t.Fatal("Go decoded an image without tables: the test proves nothing")
	}
	prefix := []byte("x")
	fixed := AppendCompleteJPEG(prefix, bare)
	if fixed[0] != 'x' {
		t.Fatal("dst not kept")
	}
	got, err := jpeg.Decode(bytes.NewReader(fixed[1:]))
	if err != nil {
		t.Fatalf("completed image: %v", err)
	}
	// Go's encoder uses the standard tables, so the picture is the same.
	w, g := want.(*image.YCbCr), got.(*image.YCbCr)
	if !bytes.Equal(w.Y, g.Y) || !bytes.Equal(w.Cb, g.Cb) || !bytes.Equal(w.Cr, g.Cr) {
		t.Fatal("completed image decodes to another picture")
	}
	if got := AppendCompleteJPEG(nil, full); !bytes.Equal(got, full) {
		t.Error("an image with tables changed")
	}
	for _, b := range [][]byte{nil, {0xFF}, []byte("not a jpeg"), {0xFF, 0xD8, 0xFF}, {0xFF, 0xD8, 0xFF, 0xE0, 0x00}, {0xFF, 0xD8, 0x12, 0x34}, {0xFF, 0xD8, 0xFF, 0xD9}} {
		if JPEGNeedsTables(b) {
			t.Errorf("%x needs tables", b)
		}
		if got := AppendCompleteJPEG(nil, b); !bytes.Equal(got, b) {
			t.Errorf("%x was changed", b)
		}
	}
	var dst []byte
	if n := testing.AllocsPerRun(50, func() { dst = AppendCompleteJPEG(dst[:0], bare) }); n != 0 {
		t.Errorf("%v allocations per image", n)
	}
}

// jpegPSNR compares one plane of a frame with Go's decode.
func jpegPSNR(f *VideoFrame, plane int, ref *image.YCbCr) float64 {
	w, h := f.Width, f.Height
	if plane > 0 {
		w, h = (w+1)/2, (h+1)/2
	}
	var sum float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var got, want byte
			switch plane {
			case 0:
				got, want = f.Y[y*f.YStride+x], ref.Y[y*ref.YStride+x]
			case 1, 2:
				if f.Format == NV12 {
					got = f.U[y*f.UVStride+2*x+plane-1]
				} else if plane == 1 {
					got = f.U[y*f.UVStride+x]
				} else {
					got = f.V[y*f.UVStride+x]
				}
				want = ref.Cb[y*ref.CStride+x]
				if plane == 2 {
					want = ref.Cr[y*ref.CStride+x]
				}
			}
			d := float64(got) - float64(want)
			sum += d * d
		}
	}
	if sum == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255*float64(w*h)/sum)
}

// Every JPEG decoder takes images without tables, and images of odd size.
func TestJPEGDecoders(t *testing.T) {
	for _, size := range [][2]int{{640, 360}, {641, 359}, {33, 17}} {
		full := jpegGradient(t, size[0], size[1])
		m, err := jpeg.Decode(bytes.NewReader(full))
		if err != nil {
			t.Fatal(err)
		}
		ref := m.(*image.YCbCr)
		bare := jpegStripTables(t, full)
		for _, name := range confNames(JPEG, Decode) {
			t.Run(name, func(t *testing.T) {
				for _, b := range [][]byte{full, bare} {
					f, err := New(Options{Backend: name}).DecodeJPEG(b)
					if err != nil {
						if _, ok := err.(*NoBackendError); ok {
							confUnavailable(t, JPEG, Decode, name, err)
						}
						t.Fatalf("%dx%d, tables %v: %v", size[0], size[1], !JPEGNeedsTables(b), err)
					}
					if f.Width != size[0] || f.Height != size[1] {
						t.Fatalf("decoded %dx%d, want %dx%d", f.Width, f.Height, size[0], size[1])
					}
					for p := 0; p < 3; p++ {
						// Hardware decoders differ from Go's in rounding only. The
						// Windows decoder uses a coarser IDCT: about 51 dB.
						limit := 60.0
						if name == "mediafoundation" {
							limit = 48
						}
						if psnr := jpegPSNR(f, p, ref); psnr < limit {
							t.Errorf("%dx%d plane %d: PSNR %.1f dB against Go", size[0], size[1], p, psnr)
						}
					}
					f.Release()
				}
			})
		}
	}
}
