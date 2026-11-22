package gaal

import (
	"bytes"
	"testing"
)

// annexBNALs returns the NAL units of an Annex B packet.
func annexBNALs(b []byte) [][]byte {
	var out [][]byte
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 {
			return out
		}
		start := pos + i + 3
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		out = append(out, bytes.TrimRight(b[start:end], "\x00"))
		pos = end
	}
}

// h265SPSReorder reads sps_max_num_reorder_pics and sps_max_dec_pic_buffering
// of the highest sub-layer.
func h265SPSReorder(nal []byte) (reorder, buffering uint32, ok bool) {
	r := bitReader{b: appendRBSP(nil, nal[2:], len(nal))}
	subLayers, _, _, _ := h265SPSHead(&r)
	first := subLayers
	if r.bit() == 1 {
		first = 0
	}
	for i := first; i <= subLayers; i++ {
		buffering = r.ue() + 1
		reorder = r.ue()
		r.ue()
	}
	return reorder, buffering, !r.bad
}

// encodeForced encodes frames pictures and forces a keyframe at force, so the
// test does not depend on each encoder honouring the keyframe interval.
func encodeForced(t *testing.T, name string, cfg VideoConfig, frames, force int) []Packet {
	t.Helper()
	enc, err := New(Options{Backend: name}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var pkts []Packet
	keep := func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() }
	for i := 0; i < frames; i++ {
		f := testFrame(cfg.Width, cfg.Height, i)
		f.ForceKeyframe = i == force
		if err := enc.Send(f); err != nil {
			t.Fatal(err)
		}
		drain(t, enc, keep)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, enc, keep)
	return pkts
}

// TestEncodersStateNoReorder checks that every keyframe of every H.264 and
// H.265 encoder carries an SPS saying pictures are not reordered. Without it
// a decoder may hold pictures back as far as the level allows: browsers do,
// which costs a frame of latency.
func TestEncodersStateNoReorder(t *testing.T) {
	for _, c := range []Codec{H264, H265} {
		for _, b := range New(Options{}).Backends(c, Encode) {
			for _, rng := range []ColorRange{RangeUnspecified, RangeFull} {
				if rng != RangeUnspecified && confRangeUnsupported(c, b.Name) {
					continue
				}
				t.Run(c.String()+"/"+b.Name+"/"+rng.String(), func(t *testing.T) {
					// MediaCodec sets the keyframe interval in whole seconds.
					cfg := VideoConfig{Codec: c, Width: 640, Height: 360, FPS: 30, Bitrate: 2_000_000, KeyframeEvery: 30, Range: rng}
					if enc, err := New(Options{Backend: b.Name}).NewVideoEncoder(cfg); err != nil {
						confUnavailable(t, c, Encode, b.Name, err)
					} else {
						enc.Close()
					}
					keys := 0
					for i, p := range encodeForced(t, b.Name, cfg, 40, 20) {
						if !p.Keyframe {
							continue
						}
						keys++
						var sps []byte
						for _, nal := range annexBNALs(p.Data) {
							if len(nal) > 2 && ((c == H264 && nal[0]&0x1F == 7) || (c == H265 && nal[0]>>1&0x3F == 33)) {
								sps = nal
							}
						}
						if sps == nil {
							t.Fatalf("keyframe %d has no SPS", i)
						}
						if rng != RangeUnspecified {
							if got := nalSPSRange(c, sps); got != rng {
								t.Errorf("keyframe %d signals %s, want %s", i, got, rng)
							}
						}
						if c == H264 {
							s := parseH264Seq(appendRBSP(nil, sps[1:], len(sps)))
							switch {
							case !s.valid:
								t.Fatalf("keyframe %d: SPS does not parse", i)
							case !s.restricted:
								t.Fatalf("keyframe %d: SPS has no bitstream_restriction (profile %d)", i, s.profile)
							case s.maxReorder != 0:
								t.Errorf("keyframe %d: max_num_reorder_frames %d", i, s.maxReorder)
							case s.maxBuffering < s.numRef:
								t.Errorf("keyframe %d: max_dec_frame_buffering %d below %d reference frames", i, s.maxBuffering, s.numRef)
							}
							continue
						}
						if reorder, _, ok := h265SPSReorder(sps); !ok || reorder != 0 {
							t.Errorf("keyframe %d: sps_max_num_reorder_pics %d (parsed %v)", i, reorder, ok)
						}
					}
					if keys < 2 {
						t.Fatalf("%d keyframes in 40 frames with one forced, want at least 2", keys)
					}
				})
			}
		}
	}
}
