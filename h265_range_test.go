package gaal

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// h265FirstSPS returns the first SPS NAL unit of an Annex B packet.
func h265FirstSPS(t *testing.T, b []byte) []byte {
	t.Helper()
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 {
			t.Fatal("no SPS")
		}
		start := pos + i + 3
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		if b[start]>>1&0x3F == 33 {
			return bytes.TrimRight(b[start:end], "\x00")
		}
		pos = end
	}
}

// SPS NAL units without a video_signal_type: one from the VAAPI encoder with
// a VUI holding only timing, and the same with the VUI removed. ffmpeg decodes
// both with either range inserted.
var h265SPSNoSignal = []string{
	"42010101600000030090000003000003003ca00a080f165ae490a524b88040000003004000000782",
	"42010101600000030090000003000003003ca00a080f165ae490a524b820",
}

func TestH265SignalRange(t *testing.T) {
	sps := h265FirstSPS(t, lowDelayH265B.packets(t)[0].Data)
	if _, ok := appendH265SignalRange(nil, sps, RangeFull); ok {
		t.Errorf("rewrote an SPS that already signals %s", nalSPSRange(H265, sps))
	}
	for _, h := range h265SPSNoSignal {
		sps, _ := hex.DecodeString(h)
		if got := nalSPSRange(H265, sps); got != RangeUnspecified {
			t.Fatalf("fixture signals %s", got)
		}
		for _, want := range []ColorRange{RangeLimited, RangeFull} {
			out, ok := appendH265SignalRange(nil, sps, want)
			if !ok {
				t.Fatalf("%s: not rewritten", want)
			}
			if got := nalSPSRange(H265, out); got != want {
				t.Errorf("%s: rewritten SPS signals %s", want, got)
			}
			if !bytes.Equal(out[:2], sps[:2]) {
				t.Errorf("%s: NAL header % x, want % x", want, out[:2], sps[:2])
			}
			if _, again := appendH265SignalRange(nil, out, want); again {
				t.Errorf("%s: rewrote an SPS that already signals a range", want)
			}
		}
	}
}
