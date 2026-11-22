package display

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aep/gaal"
)

// TestMain gives the tests the main thread that AppKit needs.
func TestMain(m *testing.M) {
	code := 0
	Main(func() { code = m.Run() })
	os.Exit(code)
}

// TestMacPixelBuffers needs no window server: it exercises the IOSurface
// buffers that frames are written to.
func TestMacPixelBuffers(t *testing.T) {
	a, err := loadMac()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{1280, 720}, {1920, 1080}} {
		for _, format := range []gaal.PixelFormat{gaal.NV12, gaal.I420} {
			for _, r := range []gaal.ColorRange{gaal.RangeLimited, gaal.RangeFull} {
				name := fmt.Sprintf("%dx%d/%d/%v", size[0], size[1], format, r)
				f := quadFrame(size[0], size[1], format, r)
				l := layout{size[0], size[1], format, BT709, r == gaal.RangeFull}
				b, err := newMacBuffers(a, l)
				if err != nil {
					t.Fatal(name, err)
				}
				start := time.Now()
				const n = 50
				for i := 0; i < n; i++ {
					b.fill(i%len(b.pbs), f)
				}
				cost := time.Since(start) / n
				allocs := testing.AllocsPerRun(20, func() { b.fill(0, f) })
				img := b.image(0)
				if err := checkQuads(img, rect{0, 0, size[0], size[1]}, l.coefs(), 3); err != nil {
					t.Errorf("%s: %v", name, err)
				}
				t.Logf("%s: fill %v, %v allocs", name, cost, allocs)
				if allocs != 0 {
					t.Errorf("%s: %v allocations", name, allocs)
				}
				b.free()
			}
		}
	}
}

func TestLiveMac(t *testing.T) {
	wantLive(t)
	if !hasWindowServer() {
		t.Skip("this process has no window server session")
	}
	exercise(t, Options{Backend: "macos"}, "calayer-iosurface-nv12")
}
