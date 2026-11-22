//go:build amd64 || arm64

package display

const simdPlanes = true

//go:noescape
func interleaveUV16(dst, u, v *byte, n int)

//go:noescape
func deinterleaveUV16(src, u, v *byte, n int)
