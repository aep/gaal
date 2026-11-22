//go:build windows && (amd64 || arm64)

package gaal

import "syscall"

func ohOpen(name string) (uintptr, error) {
	h, err := syscall.LoadLibrary(name)
	return uintptr(h), err
}

func ohClose(h uintptr) { syscall.FreeLibrary(syscall.Handle(h)) }
