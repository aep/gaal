package display

import "testing"

func TestLiveWindows(t *testing.T) {
	wantLive(t)
	t.Run("best", func(t *testing.T) { exercise(t, Options{Backend: "windows"}, "") })
	t.Run("gdi", func(t *testing.T) {
		winDisableD3D = true
		defer func() { winDisableD3D = false }()
		exercise(t, Options{Backend: "windows"}, "gdi-stretchdibits")
	})
}
