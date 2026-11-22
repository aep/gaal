//go:build linux && !android

package display

func platformBackends() []opener {
	return []opener{{"wayland", openWayland}, {"x11", openX11}}
}
