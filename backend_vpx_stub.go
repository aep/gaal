//go:build windows || !(amd64 || arm64)

package gaal

func vpxBackends() []backend { return nil }
