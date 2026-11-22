//go:build !darwin

package display

// Main runs fn. On macOS it also runs the AppKit event loop on the calling
// thread, so portable programs call it from their main goroutine.
func Main(fn func()) { fn() }
