//go:build !amd64 && !arm64

package display

const simdPlanes = false

var useSIMD = false

func interleaveUV16(dst, u, v *byte, n int)   { panic("unreachable") }
func deinterleaveUV16(src, u, v *byte, n int) { panic("unreachable") }

func i420Kernel(dst, y, u, v *byte, n int, c *coefs) { panic("unreachable") }
func nv12Kernel(dst, y, uv *byte, n int, c *coefs)   { panic("unreachable") }

const haveCompress = false

func compressKernel(p *byte, n int, m, k uint16) { panic("unreachable") }
