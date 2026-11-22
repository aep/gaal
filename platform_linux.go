//go:build linux && !android

package gaal

import "sync"

// allBackends lists every backend, best first within each codec.
var allBackends = sync.OnceValue(func() []backend {
	// libjpeg-turbo before the GPUs: a JPEG image decodes faster on the CPU
	// than it travels to a GPU and back
	out := turboJPEGBackends()
	out = append(out, nvidiaBackends()...)
	out = append(out, vaapiBackends()...)
	out = append(out, opusBackends()...)
	out = append(out, vpxBackends()...)
	out = append(out, audioBackends()...)
	out = append(out, openH264Backends()...)
	return append(out, goJPEGBackends()...)
})

func libNames(name, soversion string) []string {
	return []string{"lib" + name + ".so." + soversion, "lib" + name + ".so"}
}

// Cisco's licence: the binary must come directly from Cisco and users must be able to disable it.
func downloadableH264() bool { return ohDownloadable() }
