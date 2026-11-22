//go:build !linux && !android && !darwin && !windows

package gaal

import "sync"

// allBackends lists every backend, best first within each codec. Other systems
// only have system libraries and pure Go.
var allBackends = sync.OnceValue(func() []backend {
	out := opusBackends()
	out = append(out, vpxBackends()...)
	out = append(out, audioBackends()...)
	out = append(out, turboJPEGBackends()...)
	return append(out, goJPEGBackends()...)
})

func libNames(name, soversion string) []string {
	return []string{"lib" + name + ".so." + soversion, "lib" + name + ".so"}
}

func downloadableH264() bool { return false }
