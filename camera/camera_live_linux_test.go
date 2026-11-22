//go:build linux && !android && (amd64 || arm64)

package camera

import (
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/aep/gaal"
)

// liveDevice returns the first capture device, skipping without one.
func liveDevice(t *testing.T) Device {
	t.Helper()
	ds, err := Devices()
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) == 0 {
		t.Skip("no V4L2 capture device")
	}
	for _, d := range ds {
		t.Logf("device %s: %q driver %s bus %s", d.Path, d.Name, d.Driver, d.Bus)
	}
	return ds[0]
}

func TestLiveCapture(t *testing.T) {
	d := liveDevice(t)
	for _, keep := range []bool{false, true} {
		cam, err := Open(Config{Device: d.Path, Width: 640, Height: 480, FPS: 30, KeepJPEG: keep})
		if err != nil {
			t.Fatal(err)
		}
		c := cam.(*v4l2Camera)
		t.Logf("KeepJPEG=%v: %s", keep, cam.Name())
		var last time.Time
		for i := range 10 {
			cp, err := c.Capture(3 * time.Second)
			if err != nil {
				cam.Close()
				t.Fatal(err)
			}
			if age := time.Since(cp.Captured); age < 0 || age > time.Second {
				t.Fatalf("frame %d captured %v ago", i, age)
			}
			if !cp.Captured.After(last) {
				t.Fatalf("frame %d captured %v, not after %v", i, cp.Captured, last)
			}
			last = cp.Captured
			jpegDevice := isJPEG(c.format)
			switch {
			case keep && jpegDevice:
				if cp.JPEG == nil || cp.Frame != nil || !jpegWhole(cp.JPEG) || gaal.JPEGNeedsTables(cp.JPEG) {
					t.Fatalf("frame %d: want a complete JPEG", i)
				}
			default:
				if cp.Frame == nil || cp.JPEG != nil {
					t.Fatalf("frame %d: want a frame", i)
				}
				if cp.Frame.Width != cp.Width || cp.Frame.Height != cp.Height {
					t.Fatalf("frame %dx%d, capture says %dx%d", cp.Frame.Width, cp.Frame.Height, cp.Width, cp.Height)
				}
				cp.Frame.Release()
			}
		}
		f, err := cam.Next(3 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if f.PTS <= 0 {
			t.Fatalf("PTS %v", f.PTS)
		}
		f.Release()
		if keep && isJPEG(c.format) {
			n := testing.AllocsPerRun(20, func() {
				if _, err := c.Capture(3 * time.Second); err != nil {
					t.Fatal(err)
				}
			})
			t.Logf("KeepJPEG allocations per capture: %v", n)
			if n > 0 {
				t.Errorf("%v allocations per JPEG capture", n)
			}
		}
		cam.Close()
	}
}

func TestLiveCloseWhileBlocked(t *testing.T) {
	d := liveDevice(t)
	cam, err := Open(Config{Device: d.Path, Width: 640, Height: 480})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			f, err := cam.Next(time.Minute)
			if err != nil {
				done <- err
				return
			}
			f.Release()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	cam.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked call returned %v, want ErrClosed", err)
		}
		t.Logf("unblocked in %v", time.Since(start))
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock Next")
	}
	if _, err := cam.Next(time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after Close: %v", err)
	}
}

func TestLiveBadDevice(t *testing.T) {
	if _, err := Open(Config{Device: "/dev/null"}); err == nil {
		t.Fatal("/dev/null opened as a camera")
	}
}

// Every raw format a device offers is captured, with zero allocations per
// frame once the pool is warm.
func TestLiveRawFormats(t *testing.T) {
	liveDevice(t)
	ds, _ := Devices()
	defer func() { formatFilter = nil }()
	tested := 0
	for _, d := range ds {
		fd, err := openNode(d.Path)
		if err != nil {
			continue
		}
		_, caps, _ := queryCap(fd)
		bt, _ := captureBufType(caps)
		formats := supportedFormats(fd, bt)
		syscall.Close(fd)
		for _, format := range formats {
			if isJPEG(format) {
				continue
			}
			formatFilter = func(f uint32) bool { return f == format }
			cam, err := Open(Config{Device: d.Path, Width: 640, Height: 480})
			if err != nil {
				t.Errorf("%s %s: %v", d.Path, fourccString(format), err)
				continue
			}
			c := cam.(*v4l2Camera)
			for range 3 {
				f, err := cam.Next(3 * time.Second)
				if err != nil {
					t.Fatalf("%s: %v", cam.Name(), err)
				}
				if f.Width != c.width || f.Height != c.height {
					t.Fatalf("%s: frame %dx%d", cam.Name(), f.Width, f.Height)
				}
				f.Release()
			}
			n := testing.AllocsPerRun(10, func() {
				f, err := cam.Next(3 * time.Second)
				if err != nil {
					t.Fatal(err)
				}
				f.Release()
			})
			t.Logf("%s: %v allocations per frame", cam.Name(), n)
			if n > 0 {
				t.Errorf("%s: %v allocations per frame", cam.Name(), n)
			}
			cam.Close()
			tested++
		}
	}
	if tested == 0 {
		t.Skip("no device offers a raw format")
	}
}

// A UVC camera that sends payload header times has its pictures timed by
// them: a little before the driver stamped them (the camera's own latency),
// and steadily so.
func TestLiveSensorTimestamps(t *testing.T) {
	d := liveDevice(t)
	cam, err := Open(Config{Device: d.Path, Width: 1280, Height: 720, FPS: 60, KeepJPEG: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	c := cam.(*v4l2Camera)
	if c.meta == nil {
		t.Skipf("%s: no UVC metadata node to time pictures by", cam.Name())
	}
	t.Logf("%s, metadata %s", cam.Name(), c.meta.path)
	var leads []time.Duration
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		cp, err := c.Capture(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) < 1500*time.Millisecond {
			continue // the clock's rate is being measured
		}
		stamp := c.wallBase.Add(c.heldTS - c.monoBase)
		leads = append(leads, stamp.Sub(cp.Captured))
	}
	if len(leads) < 30 {
		t.Fatalf("%d pictures", len(leads))
	}
	slices.Sort(leads)
	lo, med, hi := leads[len(leads)/20], leads[len(leads)/2], leads[len(leads)*19/20]
	t.Logf("capture started before the driver's stamp: median %v, 5%% %v, 95%% %v (rate %.4f MHz)", med, lo, hi, c.meta.clock.rate*1000)
	if lo <= 0 || hi > maxCaptureLead || hi-lo > 5*time.Millisecond {
		t.Errorf("implausible or unsteady: 5%% %v, 95%% %v", lo, hi)
	}
}
