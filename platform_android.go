//go:build android

package gaal

import "sync"

// allBackends lists every backend, best first within each codec.
var allBackends = sync.OnceValue(func() []backend {
	out := mediaCodecBackends()
	out = append(out, opusBackends()...)
	out = append(out, vpxBackends()...)
	out = append(out, audioBackends()...)
	out = append(out, turboJPEGBackends()...)
	out = append(out, openH264Backends()...)
	return append(out, goJPEGBackends()...)
})

func libNames(name, _ string) []string { return []string{"lib" + name + ".so"} }

func downloadableH264() bool { return false }
