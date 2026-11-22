package gaal

import "sync"

// allBackends lists every backend, best first within each codec.
var allBackends = sync.OnceValue(func() []backend {
	out := mfBackends()
	out = append(out, opusBackends()...)
	out = append(out, vpxBackends()...)
	out = append(out, audioBackends()...)
	out = append(out, turboJPEGBackends()...)
	out = append(out, openH264Backends()...)
	return append(out, goJPEGBackends()...)
})

func libNames(name, soversion string) []string {
	return []string{name + ".dll", "lib" + name + ".dll", "lib" + name + "-" + soversion + ".dll"}
}

// Cisco's licence: the binary must come directly from Cisco and users must be able to disable it.
func downloadableH264() bool { return ohDownloadable() }
