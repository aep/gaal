package dl

import (
	"syscall"

	"github.com/ebitengine/purego"
)

func open(name string) (uintptr, error) {
	h, err := syscall.LoadLibrary(name)
	return uintptr(h), err
}

func sym(h uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(h), name)
}

func register(dst any, addr uintptr) { purego.RegisterFunc(dst, addr) }

func syscallN(fn uintptr, args []uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}
