//go:build linux && !android && (amd64 || arm64)

package camera

import (
	"testing"
	"time"
)

// metaBuf is a metadata buffer of one picture: payload headers with a PTS
// and an SCR, had by the host at the given times.
func metaBuf(pts uint32, headers ...struct {
	host time.Duration
	scr  uint32
}) []byte {
	var b []byte
	for _, h := range headers {
		e := make([]byte, metaEntry+10)
		put64(e, 0, uint64(h.host))
		e[10], e[11] = 12, uvcHeaderPTS|uvcHeaderSCR
		put32(e, metaEntry, pts)
		put32(e, metaEntry+4, h.scr)
		b = append(b, e...)
	}
	return b
}

// A picture is timed by the camera's clock: its capture started (SCR - PTS)
// before the packet that says so came, at the clock's rate as measured, even
// across a wrap of the 32-bit clock. Until the rate is known, nothing is said.
func TestUVCClock(t *testing.T) {
	const rate = 0.0303 // ticks per ns: 30.3 MHz
	const frame = time.Second / 60
	const sent = 14 * time.Millisecond                                                          // capture start to the packet
	tick := func(d time.Duration) uint32 { return uint32(int64(float64(d)*rate)) + 0xfff00000 } // wraps after ~35ms
	host0 := 1000 * time.Second
	var k uvcClock
	said, worst := 0, time.Duration(0)
	for i := range 300 {
		start := time.Duration(i) * frame
		jitter := time.Duration(i%5) * 50 * time.Microsecond // USB delivery: 0-200µs
		pkt := func(after time.Duration) struct {
			host time.Duration
			scr  uint32
		} {
			return struct {
				host time.Duration
				scr  uint32
			}{host0 + start + after + jitter, tick(start + after)}
		}
		at, ok := k.picture(metaBuf(tick(start), pkt(sent), pkt(sent+time.Millisecond)))
		if !ok {
			if start >= clockSpan+frame {
				t.Fatalf("picture %d: no capture time %v after the first packet", i, start)
			}
			continue
		}
		said++
		if d := (at - (host0 + start)).Abs(); d > worst {
			worst = d
		}
	}
	if said < 200 || worst > 300*time.Microsecond {
		t.Fatalf("%d pictures timed, off by up to %v", said, worst)
	}
	t.Logf("%d pictures timed, off by up to %v", said, worst)
}

// Headers without an SCR or a PTS time nothing; a short buffer is not
// misread.
func TestParseUVCMeta(t *testing.T) {
	b := metaBuf(7, struct {
		host time.Duration
		scr  uint32
	}{5, 9})
	b[11] = uvcHeaderPTS // no SCR
	hs := parseUVCMeta(b)
	if len(hs) != 1 || !hs[0].hasPTS || hs[0].hasSCR || hs[0].pts != 7 || hs[0].host != 5 {
		t.Fatalf("%+v", hs)
	}
	if hs := parseUVCMeta(b[:len(b)-1]); len(hs) != 0 {
		t.Fatalf("a cut entry: %+v", hs)
	}
	var k uvcClock
	if _, ok := k.picture(b); ok {
		t.Fatal("timed without an SCR")
	}
}

// parseUVCMeta splits a metadata buffer into its payload headers.
func parseUVCMeta(b []byte) []uvcHeader {
	var out []uvcHeader
	for {
		h, rest, ok := nextUVCHeader(b)
		if !ok {
			return out
		}
		out = append(out, h)
		b = rest
	}
}
