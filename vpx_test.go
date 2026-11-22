package gaal

import (
	"testing"
	"time"
)

// libvpx encoders suit WebRTC: one packet per frame at once, keyframes at the
// interval asked for or when forced, and bitrate changes while running.
func TestVPXRealtime(t *testing.T) {
	for _, c := range []Codec{VP8, VP9} {
		t.Run(c.String(), func(t *testing.T) {
			const every, force = 10, 14
			cfg := VideoConfig{Codec: c, Width: 320, Height: 240, FPS: 30, Bitrate: 500_000, KeyframeEvery: every}
			enc, err := New(Options{Backend: "libvpx"}).NewVideoEncoder(cfg)
			if err != nil {
				confUnavailable(t, c, Encode, "libvpx", err)
			}
			defer enc.Close()
			clip := benchClip(cfg.Width, cfg.Height, 8)
			last := -1
			for i := 0; i < 40; i++ {
				if i == 20 {
					if err := enc.(BitrateSetter).SetBitrate(250_000); err != nil {
						t.Fatal(err)
					}
				}
				f := clip[i%len(clip)]
				f.PTS, f.ForceKeyframe = time.Duration(i)*confStep, i == force
				err := enc.Send(f)
				f.ForceKeyframe = false
				if err != nil {
					t.Fatal(err)
				}
				p, err := enc.Receive()
				if err != nil {
					t.Fatalf("frame %d: no packet at once: %v", i, err)
				}
				if p.PTS != f.PTS {
					t.Fatalf("frame %d: packet has PTS %v", i, p.PTS)
				}
				if p.Keyframe != confIsKey(c, p.Data) {
					t.Fatalf("frame %d: Keyframe %v disagrees with the bitstream", i, p.Keyframe)
				}
				if p.Keyframe {
					last = i
				}
				switch {
				case i == 0 && !p.Keyframe, i == force && !p.Keyframe:
					t.Fatalf("frame %d is not a keyframe", i)
				case i-last >= every:
					t.Fatalf("frame %d: no keyframe since %d", i, last)
				}
				p.Release()
				if _, err := enc.Receive(); err != ErrAgain {
					t.Fatalf("frame %d: second output: %v", i, err)
				}
			}
		})
	}
}
