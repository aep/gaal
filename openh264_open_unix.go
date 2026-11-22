//go:build (amd64 || arm64) && !windows

package gaal

import "github.com/ebitengine/purego"

// OpenH264 exports all of its internal symbols, so it is loaded privately. With
// RTLD_GLOBAL a second copy in the process would bind to the first one's code.
func ohOpen(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func ohClose(h uintptr) { purego.Dlclose(h) }
