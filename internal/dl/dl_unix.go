//go:build !windows && (amd64 || arm64)

package dl

import "github.com/ebitengine/purego"

func open(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func sym(h uintptr, name string) (uintptr, error) {
	return purego.Dlsym(h, name)
}

func register(dst any, addr uintptr) { purego.RegisterFunc(dst, addr) }

func syscallN(fn uintptr, args []uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}
