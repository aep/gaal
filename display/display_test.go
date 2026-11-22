package display

import (
	"errors"
	"flag"
	"image/color"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/aep/gaal"
)

func testFrame(w, h int, format gaal.PixelFormat, seed int64) *gaal.VideoFrame {
	f := gaal.NewVideoFrame(w, h, format)
	r := rand.New(rand.NewSource(seed))
	r.Read(f.Y)
	r.Read(f.U)
	if format == gaal.I420 {
		r.Read(f.V)
	}
	return f
}

// solidFrame has one color everywhere.
func solidFrame(w, h int, format gaal.PixelFormat, y, u, v byte) *gaal.VideoFrame {
	f := gaal.NewVideoFrame(w, h, format)
	for i := range f.Y {
		f.Y[i] = y
	}
	if format == gaal.NV12 {
		for i := 0; i+1 < len(f.U); i += 2 {
			f.U[i], f.U[i+1] = u, v
		}
		return f
	}
	for i := range f.U {
		f.U[i], f.V[i] = u, v
	}
	return f
}

func withSIMD(on bool, fn func()) {
	old := useSIMD
	useSIMD = on && old
	defer func() { useSIMD = old }()
	fn()
}

func TestSIMDMatchesScalar(t *testing.T) {
	if !useSIMD {
		t.Skip("no SIMD on this machine")
	}
	for _, format := range []gaal.PixelFormat{gaal.I420, gaal.NV12} {
		for _, w := range []int{1, 2, 15, 16, 17, 31, 32, 33, 64, 321} {
			for _, h := range []int{1, 2, 3, 16} {
				f := testFrame(w, h, format, int64(w*h))
				for _, m := range []Matrix{BT601, BT709} {
					for _, full := range []bool{false, true} {
						c := coefsFor(m, full)
						a, b := make([]byte, 4*w*h), make([]byte, 4*w*h)
						toBGRX(a, 4*w, f, c)
						withSIMD(false, func() { toBGRX(b, 4*w, f, c) })
						if string(a) != string(b) {
							t.Fatalf("format %d %dx%d matrix %v full %v: SIMD differs from scalar", format, w, h, m, full)
						}
					}
				}
			}
		}
	}
}

// TestSIMDExtremes covers saturation with every corner of the sample range.
func TestSIMDExtremes(t *testing.T) {
	if !useSIMD {
		t.Skip("no SIMD on this machine")
	}
	vals := []byte{0, 1, 15, 16, 17, 127, 128, 129, 234, 235, 236, 239, 240, 241, 254, 255}
	const w = 32
	f := gaal.NewVideoFrame(w, 2, gaal.I420)
	for _, y := range vals {
		for _, u := range vals {
			for _, v := range vals {
				for i := range f.Y {
					f.Y[i] = y
				}
				for i := range f.U {
					f.U[i], f.V[i] = u, v
				}
				for ci := range coefTable {
					c := &coefTable[ci]
					a, b := make([]byte, 4*w*2), make([]byte, 4*w*2)
					toBGRX(a, 4*w, f, c)
					withSIMD(false, func() { toBGRX(b, 4*w, f, c) })
					if string(a) != string(b) {
						t.Fatalf("y=%d u=%d v=%d table %d: %v != %v", y, u, v, ci, a[:4], b[:4])
					}
				}
			}
		}
	}
}

// TestColorAccuracy compares the fixed-point math with the exact formula.
func TestColorAccuracy(t *testing.T) {
	for _, m := range []Matrix{BT601, BT709} {
		for _, full := range []bool{false, true} {
			kr, kb := 0.299, 0.114
			if m == BT709 {
				kr, kb = 0.2126, 0.0722
			}
			kg := 1 - kr - kb
			c := coefsFor(m, full)
			for y := 0; y < 256; y += 3 {
				for u := 0; u < 256; u += 5 {
					for v := 0; v < 256; v += 5 {
						ly, cu, cv := float64(y), float64(u)-128, float64(v)-128
						if !full {
							ly = (ly - 16) * 255 / 219
							cu *= 255.0 / 224
							cv *= 255.0 / 224
						}
						r := ly + 2*(1-kr)*cv
						b := ly + 2*(1-kb)*cu
						g := ly - 2*(1-kb)*kb/kg*cu - 2*(1-kr)*kr/kg*cv
						gb, gg, gr := c.pixel(byte(y), byte(u), byte(v))
						for _, p := range [][2]float64{{float64(gb), b}, {float64(gg), g}, {float64(gr), r}} {
							want := math.Max(0, math.Min(255, p[1]))
							if math.Abs(p[0]-want) > 1.5 {
								t.Fatalf("%v full=%v yuv %d,%d,%d: got %v want %.2f", m, full, y, u, v, p[0], want)
							}
						}
					}
				}
			}
		}
	}
}

func TestPlaneHelpers(t *testing.T) {
	for _, n := range []int{1, 15, 16, 17, 40} {
		u, v := make([]byte, n), make([]byte, n)
		rand.Read(u)
		rand.Read(v)
		uv := make([]byte, 2*n)
		interleaveUV(uv, u, v)
		for i := 0; i < n; i++ {
			if uv[2*i] != u[i] || uv[2*i+1] != v[i] {
				t.Fatalf("interleave n=%d i=%d", n, i)
			}
		}
		a, b := make([]byte, n), make([]byte, n)
		deinterleaveUV(uv, a, b)
		if string(a) != string(u) || string(b) != string(v) {
			t.Fatalf("deinterleave n=%d", n)
		}
	}
}

func TestCompressPlane(t *testing.T) {
	check := func(m, k uint16, in, want byte) {
		t.Helper()
		p := []byte{in}
		compressPlane(p, m, k)
		if p[0] != want {
			t.Errorf("%d maps to %d, want %d", in, p[0], want)
		}
	}
	check(lumaM, lumaK, 0, 16)
	check(lumaM, lumaK, 255, 235)
	check(chromaM, chromaK, 128, 128)
	check(chromaM, chromaK, 0, 16)
	check(chromaM, chromaK, 255, 240)
	for _, f := range [][2]uint16{{lumaM, lumaK}, {chromaM, chromaK}} {
		for _, n := range []int{15, 16, 17, 256, 1000} {
			p := make([]byte, n)
			rand.Read(p)
			want := make([]byte, n)
			for i, v := range p {
				x := uint32(v)*uint32(f[0]) + uint32(f[1])
				want[i] = byte((x + x>>8 + 1) >> 8)
			}
			compressPlane(p, f[0], f[1])
			if string(p) != string(want) {
				t.Fatalf("n=%d differs from scalar", n)
			}
		}
	}
}

func TestPlace(t *testing.T) {
	cases := []struct {
		mode     ScaleMode
		sw, sh   int
		ww, wh   int
		src, dst rect
	}{
		{ScaleFit, 1920, 1080, 1920, 1080, rect{0, 0, 1920, 1080}, rect{0, 0, 1920, 1080}},
		{ScaleFit, 1920, 1080, 1000, 1000, rect{0, 0, 1920, 1080}, rect{0, 218, 1000, 563}},
		{ScaleFit, 1080, 1920, 1000, 1000, rect{0, 0, 1080, 1920}, rect{218, 0, 563, 1000}},
		{ScaleFill, 1920, 1080, 1000, 1000, rect{420, 0, 1080, 1080}, rect{0, 0, 1000, 1000}},
		{ScaleFill, 1080, 1920, 1000, 1000, rect{0, 420, 1080, 1080}, rect{0, 0, 1000, 1000}},
		{ScaleStretch, 1920, 1080, 1000, 1000, rect{0, 0, 1920, 1080}, rect{0, 0, 1000, 1000}},
	}
	for _, c := range cases {
		s, d := place(c.mode, c.sw, c.sh, c.ww, c.wh)
		if s != c.src || d != c.dst {
			t.Errorf("place(%d,%d,%d,%d,%d) = %v %v, want %v %v", c.mode, c.sw, c.sh, c.ww, c.wh, s, d, c.src, c.dst)
		}
	}
}

func TestScaleBGRX(t *testing.T) {
	src := []byte{1, 0, 0, 255, 2, 0, 0, 255, 3, 0, 0, 255, 4, 0, 0, 255} // 2x2
	dst := make([]byte, 4*4*4)
	sr, dr := place(ScaleFit, 2, 2, 4, 4)
	scaleBGRX(dst, 16, 4, 4, src, 8, sr, dr)
	want := []byte{1, 1, 2, 2, 1, 1, 2, 2, 3, 3, 4, 4, 3, 3, 4, 4}
	for i, v := range want {
		if dst[4*i] != v {
			t.Fatalf("pixel %d = %d, want %d", i, dst[4*i], v)
		}
	}
}

func openManual(t *testing.T) (*Display, *headless) {
	t.Helper()
	d, err := Open(Options{Backend: "headless"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	h := d.b.(*headless)
	h.manual = true
	return d, h
}

func TestLatestWins(t *testing.T) {
	d, h := openManual(t)
	mark := func(y byte) *gaal.VideoFrame { return solidFrame(16, 16, gaal.I420, y, 128, 128) }
	if err := d.Show(mark(50)); err != nil { // goes straight to the display
		t.Fatal(err)
	}
	for y := byte(60); y <= 100; y += 10 { // pile up while the display is busy
		if err := d.Show(mark(y)); err != nil {
			t.Fatal(err)
		}
	}
	h.vblank() // 50 completes, the newest pending frame is submitted
	h.vblank() // that one completes
	img, src, err := d.Readback()
	if err != nil {
		t.Fatal(err)
	}
	if src != ReadbackBuffer {
		t.Errorf("source %q", src)
	}
	want := c8(100)
	if got := img.RGBAAt(3, 3); got != want {
		t.Errorf("displayed %v, want the newest frame %v", got, want)
	}
	s := d.Stats()
	if s.Shown != 6 || s.Presented != 2 || s.Dropped != 4 {
		t.Errorf("stats %+v", s)
	}
}

func c8(y byte) color.RGBA {
	b, g, r := coefsFor(BT601, false).pixel(y, 128, 128)
	return color.RGBA{r, g, b, 255}
}

func TestLayoutChange(t *testing.T) {
	d, h := openManual(t)
	for _, size := range [][2]int{{32, 18}, {64, 36}, {64, 36}, {16, 16}} {
		for _, fmtv := range []gaal.PixelFormat{gaal.I420, gaal.NV12} {
			if err := d.Show(testFrame(size[0], size[1], fmtv, 1)); err != nil {
				t.Fatal(err)
			}
			h.vblank()
		}
	}
	h.vblank()
	img, _, err := d.Readback()
	if err != nil {
		t.Fatal(err)
	}
	if img.Rect.Dx() != 16 || img.Rect.Dy() != 16 {
		t.Errorf("readback is %v", img.Rect)
	}
}

func TestRefreshPacing(t *testing.T) {
	d, err := Open(Options{Backend: "headless", Refresh: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f := testFrame(64, 36, gaal.NV12, 2)
	for i := 0; i < 200; i++ {
		if err := d.Show(f); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	s := d.Stats()
	if s.Shown != 200 || s.Presented == 0 || s.Presented+s.Dropped < 199 {
		t.Errorf("stats %+v", s)
	}
}

func TestClosedAndInvalid(t *testing.T) {
	d, err := Open(Options{Backend: "headless"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Show(&gaal.VideoFrame{Width: 4, Height: 4}); !errors.Is(err, gaal.ErrInvalid) {
		t.Errorf("bad frame: %v", err)
	}
	if err := d.Show(nil); !errors.Is(err, gaal.ErrInvalid) {
		t.Errorf("nil frame: %v", err)
	}
	if _, _, err := d.Readback(); !errors.Is(err, ErrNoFrame) {
		t.Errorf("empty readback: %v", err)
	}
	d.Close()
	if err := d.Close(); err != nil {
		t.Error(err)
	}
	if err := d.Show(testFrame(4, 4, gaal.I420, 1)); !errors.Is(err, ErrClosed) {
		t.Errorf("after close: %v", err)
	}
}

func TestEventQueue(t *testing.T) {
	q := eventQueue{ready: make(chan struct{}, 1)}
	q.push(Event{Kind: EventResize, Width: 1, Height: 1})
	q.push(Event{Kind: EventResize, Width: 2, Height: 2})
	q.push(Event{Kind: EventClose})
	if e, _ := q.pop(); e.Width != 2 {
		t.Errorf("resize did not coalesce: %+v", e)
	}
	if e, _ := q.pop(); e.Kind != EventClose {
		t.Errorf("got %+v", e)
	}
	if _, ok := q.pop(); ok {
		t.Error("queue should be empty")
	}
	if !q.closeRequested() {
		t.Error("close not remembered")
	}
}

func TestHeadlessNoAllocs(t *testing.T) {
	d, err := Open(Options{Backend: "headless", Refresh: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f := testFrame(640, 360, gaal.NV12, 3)
	d.Show(f)
	if n := testing.AllocsPerRun(200, func() { d.Show(f) }); n != 0 {
		t.Errorf("%v allocations per Show", n)
	}
}

func TestConvertNoAllocs(t *testing.T) {
	f := testFrame(640, 360, gaal.I420, 3)
	dst := make([]byte, 4*640*360)
	if n := testing.AllocsPerRun(50, func() { toBGRX(dst, 4*640, f, coefsFor(BT709, false)) }); n != 0 {
		t.Errorf("%v allocations", n)
	}
}

func TestUnknownBackend(t *testing.T) {
	if _, err := Open(Options{Backend: "nope"}); !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v", err)
	}
}

func benchConvert(b *testing.B, w, h int, format gaal.PixelFormat) {
	f := testFrame(w, h, format, 9)
	dst := make([]byte, 4*w*h)
	c := coefsFor(BT709, false)
	b.SetBytes(int64(4 * w * h))
	b.ReportAllocs()
	for b.Loop() {
		toBGRX(dst, 4*w, f, c)
	}
}

func BenchmarkBGRX720pNV12(b *testing.B)  { benchConvert(b, 1280, 720, gaal.NV12) }
func BenchmarkBGRX1080pNV12(b *testing.B) { benchConvert(b, 1920, 1080, gaal.NV12) }
func BenchmarkBGRX1080pI420(b *testing.B) { benchConvert(b, 1920, 1080, gaal.I420) }
func BenchmarkBGRX1080pScalar(b *testing.B) {
	withSIMD(false, func() { benchConvert(b, 1920, 1080, gaal.NV12) })
}

var liveFlag = flag.Bool("display.live", false, "open real windows")

func wantLive(t *testing.T) {
	t.Helper()
	if os.Getenv("GAAL_DISPLAY_LIVE") == "" && !*liveFlag {
		t.Skip("set GAAL_DISPLAY_LIVE=1 or -display.live to open real windows")
	}
}
