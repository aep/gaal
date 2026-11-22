package display

// useSIMD selects the assembly kernels. Tests clear it to compare results.
var useSIMD = detectAVX2()

func cpuid(leaf, sub uint32) (eax, ebx, ecx, edx uint32)
func xgetbv() (eax, edx uint32)

func detectAVX2() bool {
	maxLeaf, _, _, _ := cpuid(0, 0)
	if maxLeaf < 7 {
		return false
	}
	_, _, c, _ := cpuid(1, 0)
	const osxsave, avx = 1 << 27, 1 << 28
	if c&osxsave == 0 || c&avx == 0 {
		return false
	}
	if lo, _ := xgetbv(); lo&6 != 6 {
		return false
	}
	_, b, _, _ := cpuid(7, 0)
	return b&(1<<5) != 0
}

// i420Kernel converts n pixels, a multiple of 16, to BGRX. It needs AVX2.
//
//go:noescape
func i420Kernel(dst, y, u, v *byte, n int, c *coefs)

// nv12Kernel is i420Kernel for interleaved chroma.
//
//go:noescape
func nv12Kernel(dst, y, uv *byte, n int, c *coefs)

const haveCompress = false

func compressKernel(p *byte, n int, m, k uint16) { panic("unreachable") }
