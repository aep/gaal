//go:build linux && !android

package display

// Set by the tests to observe Wayland globals and objects of their own.
var (
	wlTestGlobal = func(*wayland) func(reg, name uint32, iface string, version uint32) { return nil }
	wlTestEvent  = func(*wayland) func(id uint32, op uint16, a []byte) bool { return nil }
)
