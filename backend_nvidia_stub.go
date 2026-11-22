//go:build linux && !android && !amd64 && !arm64

package gaal

func nvidiaBackends() []backend { return nil }
