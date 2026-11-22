// Package dl loads native shared libraries and binds their symbols without
// any C code in this module.
//
// Bind uses reflection and allocates per call, so it is for setup and error
// paths. Hot paths use Sym with purego.SyscallN.
package dl

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"
)

// Open loads the first library in names that can be loaded.
func Open(names ...string) (uintptr, error) {
	var errs []string
	for _, name := range names {
		h, err := open(name)
		if err == nil {
			return h, nil
		}
		errs = append(errs, err.Error())
	}
	return 0, errors.New("dl: cannot load library: " + strings.Join(errs, "; "))
}

// Sym returns the address of a symbol.
func Sym(h uintptr, name string) (uintptr, error) {
	addr, err := sym(h, name)
	if err != nil {
		return 0, fmt.Errorf("dl: symbol %s: %w", name, err)
	}
	return addr, nil
}

// Table maps symbol names to destinations. A *uintptr receives the address, and
// a pointer to a function variable receives a Go binding.
type Table map[string]any

// BindAll resolves each entry of t and returns the first failure.
func BindAll(h uintptr, t Table) error {
	for name, dst := range t {
		addr, err := Sym(h, name)
		if err != nil {
			return err
		}
		if p, ok := dst.(*uintptr); ok {
			*p = addr
		} else {
			register(dst, addr)
		}
	}
	return nil
}

// GoString copies a NUL-terminated C string.
func GoString(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

// Caller makes C calls without allocating, by reusing one argument buffer. It
// is not safe for concurrent use. Pointer arguments must stay reachable for the
// duration of the call.
type Caller struct{ a [10]uintptr }

func (c *Caller) call(fn uintptr, n int) uintptr {
	return syscallN(fn, c.a[:n])
}

// C1 calls fn with one argument. The other methods differ only in arity.
func (c *Caller) C1(fn, a0 uintptr) uintptr {
	c.a[0] = a0
	return c.call(fn, 1)
}

func (c *Caller) C2(fn, a0, a1 uintptr) uintptr {
	c.a[0], c.a[1] = a0, a1
	return c.call(fn, 2)
}

func (c *Caller) C5(fn, a0, a1, a2, a3, a4 uintptr) uintptr {
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = a0, a1, a2, a3, a4
	return c.call(fn, 5)
}

func (c *Caller) C6(fn, a0, a1, a2, a3, a4, a5 uintptr) uintptr {
	c.a[5] = a5
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = a0, a1, a2, a3, a4
	return c.call(fn, 6)
}

func (c *Caller) C7(fn, a0, a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	c.a[5], c.a[6] = a5, a6
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = a0, a1, a2, a3, a4
	return c.call(fn, 7)
}

func (c *Caller) C8(fn, a0, a1, a2, a3, a4, a5, a6, a7 uintptr) uintptr {
	c.a[5], c.a[6], c.a[7] = a5, a6, a7
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = a0, a1, a2, a3, a4
	return c.call(fn, 8)
}

func (c *Caller) C10(fn, a0, a1, a2, a3, a4, a5, a6, a7, a8, a9 uintptr) uintptr {
	c.a[5], c.a[6], c.a[7], c.a[8], c.a[9] = a5, a6, a7, a8, a9
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = a0, a1, a2, a3, a4
	return c.call(fn, 10)
}

func (c *Caller) C3(fn, a0, a1, a2 uintptr) uintptr {
	c.a[0], c.a[1], c.a[2] = a0, a1, a2
	return c.call(fn, 3)
}

// Variadic calls fn(a0, a1, v) where v is the first variadic argument. Apple
// arm64 passes variadic arguments on the stack, past the eight register slots.
func (c *Caller) Variadic(fn, a0, a1, v uintptr) uintptr {
	if runtime.GOARCH == "arm64" && (runtime.GOOS == "darwin" || runtime.GOOS == "ios") {
		return c.C10(fn, a0, a1, 0, 0, 0, 0, 0, 0, v, 0)
	}
	return c.C3(fn, a0, a1, v)
}
