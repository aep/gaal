//go:build !(amd64 || arm64)

package gaal

func openH264Backends() []backend { return nil }
