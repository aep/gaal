package gaal

import "sync"

// allBackends lists every backend, best first within each codec.
var allBackends = sync.OnceValue(func() []backend {
	out := opusBackends() // libopus conceals loss, so it goes first if installed
	out = append(out, appleBackends()...)
	out = append(out, vpxBackends()...)
	out = append(out, audioBackends()...)
	out = append(out, turboJPEGBackends()...)
	out = append(out, openH264Backends()...)
	return append(out, goJPEGBackends()...)
})

func libNames(name, soversion string) []string {
	base := "lib" + name + ".dylib"
	versioned := "lib" + name + "." + soversion + ".dylib"
	return []string{base, versioned, "/opt/homebrew/lib/" + base, "/opt/homebrew/lib/" + versioned, "/usr/local/lib/" + base}
}

// Cisco's licence: the binary must come directly from Cisco and users must be able to disable it.
func downloadableH264() bool { return ohDownloadable() }
