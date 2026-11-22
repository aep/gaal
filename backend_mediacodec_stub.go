//go:build android && !(amd64 || arm64)

package gaal

func mediaCodecBackends() []backend { return nil }
