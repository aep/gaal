//go:build linux && !android

package display

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/aep/gaal"
)

// wlCapture reads one toplevel through ext-image-copy-capture. It only ever
// captures the toplevel whose title matches, which is this test's own window,
// so nothing else on the desktop is read.
type wlCapture struct {
	w                       *wayland
	title                   string
	listGlobal, srcMgr, cap [2]uint32 // name, version
	list, handle            uint32
	titles                  map[uint32]string
	session, frame          uint32
	bufW, bufH              int
	shmXRGB                 bool
	sessionDone             chan struct{}
	frameDone               chan error

	// wlr-foreign-toplevel, used to ask this test's own window to close.
	toplevelMgr    uint32
	toplevelTitles map[uint32]string
	ownToplevel    uint32
}

// requestClose asks the compositor to close this test's window, as a user would.
func (c *wlCapture) requestClose() bool {
	c.w.wmu.Lock()
	defer c.w.wmu.Unlock()
	if c.ownToplevel == 0 {
		return false
	}
	c.w.m0(c.ownToplevel, 5)
	c.w.flush()
	return true
}

func (c *wlCapture) global(reg, name uint32, iface string, version uint32) {
	switch iface {
	case "ext_foreign_toplevel_list_v1":
		c.listGlobal = [2]uint32{name, version}
		c.list = c.w.bind(reg, name, iface, 1, wkNone)
		c.w.flush()
	case "ext_foreign_toplevel_image_capture_source_manager_v1":
		c.srcMgr = [2]uint32{c.w.bind(reg, name, iface, 1, wkNone), 1}
		c.w.flush()
	case "zwlr_foreign_toplevel_manager_v1":
		c.toplevelMgr = c.w.bind(reg, name, iface, min(version, 3), wkNone)
		c.w.flush()
	case "ext_image_copy_capture_manager_v1":
		c.cap = [2]uint32{c.w.bind(reg, name, iface, 1, wkNone), 1}
		c.w.flush()
	}
}

func (c *wlCapture) event(id uint32, op uint16, a []byte) bool {
	switch {
	case id == c.toplevelMgr && id != 0:
		if op == 0 {
			c.toplevelTitles[ne.Uint32(a)] = ""
		}
		return true
	case id == c.list && c.list != 0:
		if op == 0 { // toplevel
			c.titles[ne.Uint32(a)] = ""
		}
		return true
	case id >= 0xFF000000:
		if _, ok := c.toplevelTitles[id]; ok {
			if op == 0 && wlString(a) == c.title { // title
				c.ownToplevel = id
			}
			return true
		}
		if _, ok := c.titles[id]; ok {
			if op == 2 { // title
				c.titles[id] = wlString(a)
				if c.titles[id] == c.title {
					c.handle = id
				}
			}
			return true
		}
	case id == c.session && id != 0:
		switch op {
		case 0:
			c.bufW, c.bufH = int(ne.Uint32(a)), int(ne.Uint32(a[4:]))
		case 1:
			c.shmXRGB = c.shmXRGB || ne.Uint32(a) == wlFormatXRGB8888
		case 4:
			select {
			case c.sessionDone <- struct{}{}:
			default:
			}
		case 5:
			select {
			case c.frameDone <- errors.New("capture session stopped"):
			default:
			}
		}
		return true
	case id == c.frame && id != 0:
		switch op {
		case 3:
			c.frameDone <- nil
		case 4:
			c.frameDone <- fmt.Errorf("capture failed, reason %d", ne.Uint32(a))
		}
		return true
	}
	return false
}

// grab returns the current window content.
func (c *wlCapture) grab() (*image.RGBA, error) {
	w := c.w
	w.wmu.Lock()
	if c.handle == 0 || c.srcMgr[0] == 0 || c.cap[0] == 0 {
		w.wmu.Unlock()
		return nil, errors.New("window capture is not available")
	}
	src := w.newID(wkOther, 0)
	w.m2(c.srcMgr[0], 0, src, c.handle)
	c.session = w.newID(wkNone, 0)
	w.m3(c.cap[0], 0, c.session, src, 0)
	w.flush()
	w.wmu.Unlock()
	select {
	case <-c.sessionDone:
	case err := <-c.frameDone:
		return nil, err
	case <-time.After(2 * time.Second):
		return nil, errors.New("no capture session")
	}
	w.wmu.Lock()
	bw, bh := c.bufW, c.bufH
	ok := c.shmXRGB
	w.wmu.Unlock()
	if !ok || bw <= 0 || bh <= 0 {
		return nil, errors.New("capture offers no XRGB8888 shm buffer")
	}
	m, err := newShmem(4*bw*bh, false)
	if err != nil {
		return nil, err
	}
	defer m.free()
	w.wmu.Lock()
	pool := w.newID(wkOther, 0)
	s := w.begin(w.shm, 0)
	w.u(pool, uint32(4*bw*bh))
	w.end(s)
	w.flushFD(m.fd)
	buf := w.newID(wkOther, 0)
	s = w.begin(pool, 0)
	w.u(buf, 0, uint32(bw), uint32(bh), uint32(4*bw), wlFormatXRGB8888)
	w.end(s)
	c.frame = w.newID(wkNone, 0)
	w.m1(c.session, 0, c.frame)
	w.m1(c.frame, 1, buf)
	w.m4(c.frame, 2, 0, 0, uint32(bw), uint32(bh))
	w.m0(c.frame, 3)
	w.flush()
	w.wmu.Unlock()
	select {
	case err := <-c.frameDone:
		if err != nil {
			return nil, err
		}
	case <-time.After(2 * time.Second):
		return nil, errors.New("capture timed out")
	}
	return bgrxToRGBA(m.data, 4*bw, bw, bh), nil
}

// openCaptured opens a Wayland window that can be captured.
func openCaptured(t *testing.T, o Options) (*Display, *wlCapture) {
	t.Helper()
	o.Backend = "wayland"
	o.Title = fmt.Sprintf("gaal-display-test-%d", time.Now().UnixNano())
	c := &wlCapture{title: o.Title, titles: map[uint32]string{}, toplevelTitles: map[uint32]string{},
		sessionDone: make(chan struct{}, 1), frameDone: make(chan error, 2)}
	wlTestGlobal = func(w *wayland) func(uint32, uint32, string, uint32) { c.w = w; return c.global }
	wlTestEvent = func(*wayland) func(uint32, uint16, []byte) bool { return c.event }
	defer func() {
		wlTestGlobal = func(*wayland) func(uint32, uint32, string, uint32) { return nil }
		wlTestEvent = func(*wayland) func(uint32, uint16, []byte) bool { return nil }
	}()
	d, err := Open(o)
	if err != nil {
		t.Skipf("cannot open: %v", err)
	}
	return d, c
}

// TestLiveWaylandPixels checks what the compositor actually renders for each
// path, by capturing this test's own window.
func TestLiveWaylandPixels(t *testing.T) {
	wantLive(t)
	for _, tc := range []struct {
		name    string
		matrix  Matrix
		rng     gaal.ColorRange
		disable int
		path    string
	}{
		{"nv12-601-limited", BT601, gaal.RangeLimited, 0, "dmabuf-nv12"},
		{"rgb-601-limited", BT601, gaal.RangeLimited, wlNoDmabuf, "shm-xrgb8888"},
		{"rgb-709-limited", BT709, gaal.RangeLimited, 0, "shm-xrgb8888"},
		{"rgb-709-full", BT709, gaal.RangeFull, 0, "shm-xrgb8888"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wlDisable = tc.disable
			defer func() { wlDisable = 0 }()
			d, c := openCaptured(t, Options{Width: 640, Height: 360, Matrix: tc.matrix})
			defer d.Close()
			f := quadFrame(1280, 720, gaal.NV12, tc.rng)
			for i := 0; i < 20; i++ {
				if err := d.Show(f); err != nil {
					t.Fatal(err)
				}
				time.Sleep(16 * time.Millisecond)
			}
			if p := d.Info().Path; p != tc.path && !strings.HasPrefix(p, tc.path) {
				t.Logf("path is %s, wanted %s", p, tc.path)
			}
			s := d.Stats()
			t.Logf("path=%s show avg=%v latency avg=%v presented=%d dropped=%d", d.Info().Path, s.ShowAvg, s.LatencyAvg, s.Presented, s.Dropped)
			img, err := c.grab()
			if err != nil {
				t.Skipf("capture: %v", err)
			}
			ww, wh := d.Size()
			scale := float64(img.Rect.Dx()) / float64(ww)
			_, dr := place(ScaleFit, 1280, 720, ww, wh)
			dr = rect{int(float64(dr.x) * scale), int(float64(dr.y) * scale), int(float64(dr.w) * scale), int(float64(dr.h) * scale)}
			l := d.e.layoutOf(f)
			if err := checkQuads(img, dr, l.coefs(), 8); err != nil {
				t.Errorf("compositor output: %v", err)
			} else {
				t.Logf("compositor output matches, %dx%d", img.Rect.Dx(), img.Rect.Dy())
			}
		})
	}
}
