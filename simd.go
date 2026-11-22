package gaal

// interleaveUV writes u and v as pairs into dst. len(dst) must be at least
// 2*len(u), and len(v) at least len(u).
func interleaveUV(dst, u, v []byte) {
	n := len(u)
	_, _ = dst[2*n-1], v[n-1]
	block := n &^ 15
	if simdAvailable && block > 0 {
		interleaveUV16(&dst[0], &u[0], &v[0], block)
	} else {
		block = 0
	}
	for x := block; x < n; x++ {
		dst[2*x], dst[2*x+1] = u[x], v[x]
	}
}

// deinterleaveUV splits pairs from src into u and v.
func deinterleaveUV(src, u, v []byte) {
	n := len(u)
	_, _ = src[2*n-1], v[n-1]
	block := n &^ 15
	if simdAvailable && block > 0 {
		deinterleaveUV16(&src[0], &u[0], &v[0], block)
	} else {
		block = 0
	}
	for x := block; x < n; x++ {
		u[x], v[x] = src[2*x], src[2*x+1]
	}
}
