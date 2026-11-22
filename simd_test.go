package gaal

import (
	"math/rand"
	"testing"
)

func TestInterleaveRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 1; n < 200; n++ {
		// Offsets exercise unaligned loads and stores.
		off := n % 7
		u, v := make([]byte, n+off), make([]byte, n+off)
		rng.Read(u)
		rng.Read(v)
		u, v = u[off:], v[off:]
		dst := make([]byte, 2*n+off+1)
		dst[2*n+off] = 0xA5
		pairs := dst[off : 2*n+off]
		interleaveUV(pairs, u, v)
		if dst[2*n+off] != 0xA5 {
			t.Fatalf("n=%d wrote past the end", n)
		}
		for i := range u {
			if pairs[2*i] != u[i] || pairs[2*i+1] != v[i] {
				t.Fatalf("n=%d interleave differs at %d", n, i)
			}
		}
		gu, gv := make([]byte, n), make([]byte, n)
		deinterleaveUV(pairs, gu, gv)
		for i := range u {
			if gu[i] != u[i] || gv[i] != v[i] {
				t.Fatalf("n=%d deinterleave differs at %d", n, i)
			}
		}
	}
}
