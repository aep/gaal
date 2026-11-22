//go:build linux && !android

package dl

import "testing"

// BenchmarkCall measures the cost of crossing into C for a trivial function.
func BenchmarkCall(b *testing.B) {
	h, err := Open("libc.so.6")
	if err != nil {
		b.Skip(err)
	}
	abs, err := Sym(h, "abs")
	if err != nil {
		b.Fatal(err)
	}
	var c Caller
	b.ReportAllocs()
	for b.Loop() {
		c.C1(abs, 1)
	}
}
