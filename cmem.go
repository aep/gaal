package gaal

import "unsafe"

// Accessors for C structs held in byte slices or C memory. All supported
// targets are little-endian and allow these aligned accesses.

func le32(b []byte, off int, v uint32) { *(*uint32)(unsafe.Pointer(&b[off])) = v }
func le64(b []byte, off int, v uint64) { *(*uint64)(unsafe.Pointer(&b[off])) = v }
func rd32(b []byte, off int) uint32    { return *(*uint32)(unsafe.Pointer(&b[off])) }
func rd64(b []byte, off int) uint64    { return *(*uint64)(unsafe.Pointer(&b[off])) }
func rdPtr(b []byte, off int) uintptr  { return *(*uintptr)(unsafe.Pointer(&b[off])) }

// cMem views n bytes of C memory.
func cMem(addr uintptr, n int) []byte {
	return unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), n)
}
