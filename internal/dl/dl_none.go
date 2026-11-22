//go:build !windows && !(amd64 || arm64)

package dl

import (
	"errors"
	"runtime"
)

// gaal's library layouts are for 64-bit targets only, so 32-bit builds load
// nothing. Without purego they also stay statically linked.

var errNoLoader = errors.New("dl: libraries are not loaded on " + runtime.GOARCH)

func open(string) (uintptr, error) { return 0, errNoLoader }

func sym(uintptr, string) (uintptr, error) { return 0, errNoLoader }

func register(any, uintptr) { panic(errNoLoader) }

func syscallN(uintptr, []uintptr) uintptr { panic(errNoLoader) }
