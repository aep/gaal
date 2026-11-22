//go:build android || windows || !(amd64 || arm64)

package gaal

func libAudioBackends() []backend { return nil }
