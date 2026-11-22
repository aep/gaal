package display

import (
	"sync/atomic"
	"unsafe"
)

var escapeSink atomic.Pointer[byte]

// esc returns p as an address for a native call. Storing p forces the variable
// onto the heap, where the address stays valid even if the goroutine's stack
// moves before the call happens. It is for setup code: frame paths keep their
// structures in long-lived objects instead.
func esc[T any](p *T) uintptr {
	escapeSink.Store((*byte)(unsafe.Pointer(p)))
	return uintptr(unsafe.Pointer(p))
}
