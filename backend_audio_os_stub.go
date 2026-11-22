//go:build !(darwin && arm64) && !(android && (amd64 || arm64)) && !(windows && (amd64 || arm64))

package gaal

func osAudioBackends() []backend { return nil }
