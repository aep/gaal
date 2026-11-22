//go:build linux && !android

package display

import (
	"os"
	"testing"
	"time"

	"github.com/aep/gaal"
)

func TestLiveWayland(t *testing.T) {
	wantLive(t)
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("no Wayland")
	}
	cases := []struct {
		name    string
		disable int
		path    string
	}{{"best", 0, ""}, {"shm", wlNoDmabuf | wlNoShmYUV, "shm-xrgb8888"}, {"noviewporter", wlNoViewporter | wlNoDmabuf, "shm-xrgb8888-scaled"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wlDisable = c.disable
			defer func() { wlDisable = 0 }()
			exercise(t, Options{Backend: "wayland"}, c.path)
		})
	}
	// BT.601 limited needs no color signalling, so it can use NV12 directly.
	t.Run("bt601", func(t *testing.T) { exercise(t, Options{Backend: "wayland", Matrix: BT601}, "") })
}

func TestLiveX11(t *testing.T) {
	wantLive(t)
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no X11")
	}
	cases := []struct {
		name    string
		disable int
		path    string
	}{{"best", 0, ""}, {"render", x11NoXV, "shm-render"}, {"shmput", x11NoXV | x11NoRender, "shm-put"}, {"putimage", x11NoShm, "putimage"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x11Disable = c.disable
			defer func() { x11Disable = 0 }()
			exercise(t, Options{Backend: "x11"}, c.path)
		})
	}
}

func waitClose(t *testing.T, d *Display) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !d.CloseRequested() {
		select {
		case <-d.EventReady():
		case <-deadline:
			t.Fatal("no close event")
		}
	}
}

// TestLiveCloseWayland asks the compositor to close this test's own window.
func TestLiveCloseWayland(t *testing.T) {
	wantLive(t)
	d, c := openCaptured(t, Options{Width: 320, Height: 180})
	defer d.Close()
	f := quadFrame(320, 180, gaal.NV12, gaal.RangeLimited)
	for i := 0; i < 10; i++ {
		d.Show(f)
		time.Sleep(16 * time.Millisecond)
	}
	if !c.requestClose() {
		t.Skip("compositor has no foreign toplevel management")
	}
	waitClose(t, d)
}

// TestLiveCloseX11 sends the window the message a window manager sends.
func TestLiveCloseX11(t *testing.T) {
	wantLive(t)
	d, err := Open(Options{Backend: "x11", Title: "gaal close test", Width: 320, Height: 180})
	if err != nil {
		t.Skip(err)
	}
	defer d.Close()
	d.Show(quadFrame(320, 180, gaal.NV12, gaal.RangeLimited))
	x := d.b.(*x11)
	ev := make([]byte, 32)
	ev[0], ev[1] = 33, 32 // ClientMessage, format 32
	put32(ev[4:], x.win, x.wmProtocols, x.wmDelete)
	if err := x.check(newReq(25, 0).u32(x.win, 0).u8(ev...)); err != nil { // SendEvent
		t.Fatal(err)
	}
	waitClose(t, d)
}
