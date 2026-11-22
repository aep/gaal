//go:build windows && !amd64 && !arm64

package gaal

func mfBackends() []backend { return nil }
