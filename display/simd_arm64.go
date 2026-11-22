package display

// useSIMD selects the assembly kernels. Tests clear it to compare results.
var useSIMD = true

// i420Kernel converts n pixels, a multiple of 16, to BGRX.
//
//go:noescape
func i420Kernel(dst, y, u, v *byte, n int, c *coefs)

// nv12Kernel is i420Kernel for interleaved chroma.
//
//go:noescape
func nv12Kernel(dst, y, uv *byte, n int, c *coefs)

const haveCompress = true

// compressKernel maps each byte to (b*m + k) / 255. n is a multiple of 16.
//
//go:noescape
func compressKernel(p *byte, n int, m, k uint16)
