package display

import (
	"fmt"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/aep/gaal"
)

// quadFrame has a different solid color in each quadrant, on chroma-block
// boundaries so that subsampling cannot blur them.
var quadColors = [4][3]byte{{81, 90, 240}, {145, 54, 34}, {41, 240, 110}, {210, 16, 146}}

func quadFrame(w, h int, format gaal.PixelFormat, r gaal.ColorRange) *gaal.VideoFrame {
	f := gaal.NewVideoFrame(w, h, format)
	f.Range = r
	cw, ch := (w+1)/2, (h+1)/2
	q := func(x, y, mx, my int) *[3]byte {
		i := 0
		if x >= mx/2 {
			i = 1
		}
		if y >= my/2 {
			i += 2
		}
		return &quadColors[i]
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f.Y[y*f.YStride+x] = q(x/2, y/2, cw, ch)[0]
		}
	}
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			c := q(x, y, cw, ch)
			if format == gaal.NV12 {
				f.U[y*f.UVStride+2*x], f.U[y*f.UVStride+2*x+1] = c[1], c[2]
			} else {
				f.U[y*f.UVStride+x], f.V[y*f.UVStride+x] = c[1], c[2]
			}
		}
	}
	return f
}

func near(a, b byte, tol int) bool {
	d := int(a) - int(b)
	return d >= -tol && d <= tol
}

// checkQuads compares the quadrant centers of img with the expected colors.
func checkQuads(img *image.RGBA, dst rect, c *coefs, tol int) error {
	bx, by := img.Rect.Min.X, img.Rect.Min.Y
	for i, q := range quadColors {
		x := dst.x + dst.w/4 + (i&1)*dst.w/2
		y := dst.y + dst.h/4 + (i>>1)*dst.h/2
		got := img.RGBAAt(bx+x, by+y)
		b, g, r := c.pixel(q[0], q[1], q[2])
		want := color.RGBA{r, g, b, 255}
		if !near(got.R, want.R, tol) || !near(got.G, want.G, tol) || !near(got.B, want.B, tol) {
			return fmt.Errorf("quadrant %d at %d,%d: got %v, want %v", i, x, y, got, want)
		}
	}
	return nil
}

// exercise opens a window, shows frames at 60 fps and checks the result.
func exercise(t *testing.T, o Options, wantPath string) {
	t.Helper()
	o.Title = "gaal display test"
	o.Width, o.Height = 640, 360
	d, err := Open(o)
	if err != nil {
		t.Skipf("cannot open: %v", err)
	}
	defer d.Close()
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}} {
		for _, format := range []gaal.PixelFormat{gaal.NV12, gaal.I420} {
			for _, r := range []gaal.ColorRange{gaal.RangeLimited, gaal.RangeFull} {
				f := quadFrame(size[0], size[1], format, r)
				if err := d.Show(f); err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond)
				d.ResetStats()
				const n = 30
				for i := 0; i < n; i++ {
					if err := d.Show(f); err != nil {
						t.Fatal(err)
					}
					time.Sleep(16 * time.Millisecond)
				}
				time.Sleep(60 * time.Millisecond)
				s := d.Stats()
				in := d.Info()
				allocs := testing.AllocsPerRun(50, func() { d.Show(f) })
				// A burst gives the cost of Show even on systems with a coarse clock.
				const burst = 200
				start := time.Now()
				for i := 0; i < burst; i++ {
					d.Show(f)
				}
				cost := time.Since(start) / burst
				t.Logf("%dx%d fmt=%d range=%v path=%s show avg=%v max=%v burst=%v latency(%s) avg=%v max=%v presented=%d dropped=%d refresh=%v allocs=%v",
					size[0], size[1], format, r, in.Path, s.ShowAvg, s.ShowMax, cost, in.LatencySource,
					s.LatencyAvg, s.LatencyMax, s.Presented, s.Dropped, s.Refresh, allocs)
				if allocs != 0 {
					t.Errorf("%v allocations per Show", allocs)
				}
				if wantPath != "" && r == gaal.RangeLimited && in.Path != wantPath {
					t.Errorf("path %q, want %q", in.Path, wantPath)
				}
				time.Sleep(50 * time.Millisecond)
				img, src, err := d.Readback()
				if err != nil {
					t.Logf("readback: %v", err)
					continue
				}
				ww, wh := d.Size()
				dst := rect{0, 0, img.Rect.Dx(), img.Rect.Dy()}
				if src == ReadbackWindow {
					_, dst = place(o.Scale, size[0], size[1], ww, wh)
					// The image can have more pixels than the window has units.
					k := float64(img.Rect.Dx()) / float64(ww)
					dst = rect{int(float64(dst.x) * k), int(float64(dst.y) * k), int(float64(dst.w) * k), int(float64(dst.h) * k)}
				}
				l := d.e.layoutOf(f)
				tol := 6
				if in.Backend == "macos" && src == ReadbackWindow {
					// Core Animation color-manages the window for the display.
					tol = 16
				}
				if err := checkQuads(img, dst, l.coefs(), tol); err != nil {
					t.Errorf("%s readback (%s): %v", src, in.Path, err)
				} else {
					t.Logf("readback ok from %s, window %dx%d", src, ww, wh)
				}
			}
		}
	}
	for {
		ev, ok := d.PollEvent()
		if !ok {
			break
		}
		t.Logf("event %+v", ev)
	}
}
