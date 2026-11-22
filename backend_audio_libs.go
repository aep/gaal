//go:build !android && !windows && (amd64 || arm64)

package gaal

// libAudioBackends lists the codecs reached through installed libraries. All of
// them are software codecs and are absent when the library is not installed.
func libAudioBackends() []backend {
	var out []backend
	out = append(out, fdkBackends()...)
	out = append(out, lameBackends()...)
	out = append(out, mpg123Backends()...)
	out = append(out, flacBackends()...)
	out = append(out, vorbisBackends()...)
	return append(out, a52Backends()...)
}
