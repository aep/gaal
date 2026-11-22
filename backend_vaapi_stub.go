//go:build linux && !android && !amd64 && !arm64

package gaal

func vaapiBackends() []backend { return nil }
