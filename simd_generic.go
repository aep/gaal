//go:build !(amd64 || arm64)

package gaal

const simdAvailable = false

func interleaveUV16(dst, u, v *byte, n int)   {}
func deinterleaveUV16(src, u, v *byte, n int) {}
