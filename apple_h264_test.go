//go:build darwin && arm64

package gaal

import (
	"fmt"
	"testing"
	"time"
)

// H.264 low-latency rate control stalls after one frame if the expected rate
// exceeds what it supports, so every size must work at every requested rate.
func TestVTH264HighFrameRates(t *testing.T) {
	for _, tc := range []struct{ w, h, fps int }{
		{3840, 2160, 30}, {2560, 1440, 60}, {1920, 1080, 120}, {1920, 1080, 240}, {1280, 720, 240}, {640, 360, 240},
	} {
		t.Run(fmt.Sprintf("%dx%d@%d", tc.w, tc.h, tc.fps), func(t *testing.T) {
			cfg := VideoConfig{Codec: H264, Width: tc.w, Height: tc.h, FPS: tc.fps, Bitrate: 8_000_000, KeyframeEvery: 2 * tc.fps}
			enc, err := New(Options{Backend: vtName}).NewVideoEncoder(cfg)
			if err != nil {
				t.Skip(err)
			}
			defer enc.Close()
			clip := benchClip(tc.w, tc.h, 3)
			for i := 0; i < 12; i++ {
				f := clip[i%len(clip)]
				f.PTS = time.Duration(i) * time.Second / time.Duration(tc.fps)
				if err := enc.Send(f); err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				p, err := ReceiveWait(enc, confWait)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				p.Release()
			}
		})
	}
}
