package rtsp

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"github.com/aep/gaal"
)

func TestH264Modes(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	if tr := h.tr.info; tr.Width != 1280 || tr.Height != 720 || tr.FrameRate != 25 {
		t.Fatalf("track info from SDP: %+v", tr)
	}
	sps, pps := h.tr.info.ParameterSets[0], h.tr.info.ParameterSets[1]
	idr, p := idrNAL(5000), pNAL(50)

	// IDR in FU-A without parameter sets: they must be inserted from SDP.
	frags := fuA(idr, 1200)
	for i, f := range frags {
		h.send(1000, i == len(frags)-1, f)
	}
	// P frame as a single NAL unit.
	h.send(4600, true, p)
	// STAP-A carrying everything including in-band parameter sets.
	h.send(8200, true, stapA(sps, pps, idr[:100]))
	out := h.out()
	if len(out) != 3 {
		t.Fatalf("got %d units", len(out))
	}
	if want := annexB(sps, pps, idr); !bytes.Equal(out[0].Data, want) || !out[0].Keyframe || !out[0].Discontinuity {
		t.Errorf("first unit: key=%v disc=%v size=%d want %d", out[0].Keyframe, out[0].Discontinuity, len(out[0].Data), len(want))
	}
	if want := annexB(p); !bytes.Equal(out[1].Data, want) || out[1].Keyframe || out[1].Discontinuity {
		t.Errorf("second unit wrong")
	}
	if want := annexB(sps, pps, idr[:100]); !bytes.Equal(out[2].Data, want) {
		t.Errorf("in-band parameter sets were duplicated or lost: %d vs %d", len(out[2].Data), len(want))
	}
	if out[0].PTS != 0 || out[1].PTS != 40*time.Millisecond || out[2].PTS != 80*time.Millisecond {
		t.Errorf("PTS %v %v %v", out[0].PTS, out[1].PTS, out[2].PTS)
	}
}

func TestH264InBandParameterSetsAreRemembered(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	sps2 := testH264SPS(640, 360)
	pps2 := []byte{0x68, 0xAA, 0xBB}
	h.send(0, false, sps2)
	h.send(0, false, pps2)
	h.send(0, true, idrNAL(30))
	h.send(3000, true, idrNAL(31))
	out := h.out()
	if len(out) != 2 {
		t.Fatalf("got %d units", len(out))
	}
	if want := annexB(sps2, pps2, idrNAL(31)); !bytes.Equal(out[1].Data, want) {
		t.Fatal("new parameter sets not inserted before the next keyframe")
	}
	if h.c.Tracks()[0].Width != 640 {
		t.Fatalf("size not updated: %+v", h.c.Tracks()[0])
	}
}

func TestParameterSetsWithOwnTimestampJoinNextPicture(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	sps := h.tr.info.ParameterSets[0]
	h.send(100, true, sps) // some cameras stamp and mark these separately
	h.send(200, true, idrNAL(20))
	out := h.out()
	if len(out) != 1 || !bytes.HasPrefix(out[0].Data, annexB(h.tr.info.ParameterSets[1])) && !bytes.Contains(out[0].Data, annexB(sps)) {
		t.Fatalf("got %d units", len(out))
	}
	if out[0].RTPTimestamp != 200 {
		t.Fatalf("timestamp %d", out[0].RTPTimestamp)
	}
}

func TestAccessUnitDelimiterRemoved(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0, false, []byte{0x09, 0xF0})
	h.send(0, true, idrNAL(20))
	out := h.out()
	if len(out) != 1 || bytes.Contains(out[0].Data, []byte{0, 0, 1, 0x09}) {
		t.Fatal("AUD kept")
	}
	if !bytes.HasPrefix(out[0].Data, annexB(h.tr.info.ParameterSets[0])) {
		t.Fatal("parameter sets are not first")
	}
}

func TestMissingMarkerEndsUnitOnTimestamp(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0, false, idrNAL(20))
	if len(h.out()) != 0 {
		t.Fatal("emitted early")
	}
	h.send(3000, false, pNAL(20))
	if out := h.out(); len(out) != 1 || !out[0].Keyframe {
		t.Fatalf("got %d units", len(out))
	}
}

func TestMarkerOnEverySliceIsDetected(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0, true, idrNAL(20))
	h.send(0, true, pNAL(20)) // second slice of the same picture
	h.send(3000, true, pNAL(21))
	h.send(3000, true, pNAL(22))
	h.send(6000, true, pNAL(23))
	out := h.out()
	if len(out) != 3 || out[2].RTPTimestamp != 3000 {
		t.Fatalf("got %d units", len(out))
	}
	if want := annexB(pNAL(21), pNAL(22)); !bytes.Equal(out[2].Data, want) {
		t.Fatal("slices of one picture were not merged after detection")
	}
}

func TestLossInsideUnitDropsItAndFlagsNext(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	idr := fuA(idrNAL(4000), 1000)
	h.send(0, false, idr[0])
	h.skip(1) // lose a middle fragment
	h.send(0, false, idr[2])
	h.send(0, true, idr[3])
	// The next picture begins cleanly right after a packet that was seen.
	h.send(3000, true, pNAL(20))
	h.send(6000, true, pNAL(21))
	out := h.out()
	if len(out) != 2 {
		t.Fatalf("got %d units", len(out))
	}
	if !out[0].Discontinuity || out[1].Discontinuity {
		t.Fatalf("flags %v %v", out[0].Discontinuity, out[1].Discontinuity)
	}
	s := h.c.Stats().Tracks[0]
	if s.PacketsLost != 1 || s.UnitsDropped != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestLossBetweenUnitsKeepsCleanPicture(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0, true, pNAL(20))
	h.out()
	h.skip(3)
	h.send(9000, true, pNAL(20)) // first_mb_in_slice == 0: an intact picture
	out := h.out()
	if len(out) != 1 || !out[0].Discontinuity {
		t.Fatalf("got %+v", out)
	}
}

func TestLossBeforeMidPictureSliceDropsPicture(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0, true, pNAL(20))
	h.out()
	h.skip(2)
	mid := pNAL(20)
	mid[1] = 0x40 // first_mb_in_slice != 0
	h.send(9000, true, mid)
	h.send(12000, true, pNAL(20))
	out := h.out()
	if len(out) != 1 || out[0].RTPTimestamp != 12000 || !out[0].Discontinuity {
		t.Fatalf("got %d units", len(out))
	}
}

func TestUnitSizeLimit(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{MaxUnitSize: 8000})
	frags := fuA(idrNAL(20000), 1000)
	for i, f := range frags {
		h.send(0, i == len(frags)-1, f)
	}
	if len(h.out()) != 0 {
		t.Fatal("oversized unit delivered")
	}
	if n := len(h.tr.depack.(*nalDepacketizer).b.p.store); n > 3*8000 {
		t.Fatalf("buffer grew to %d", n)
	}
	h.send(3000, true, pNAL(20))
	if out := h.out(); len(out) != 1 || !out[0].Discontinuity {
		t.Fatal("no recovery after oversized unit")
	}
}

func TestH265Modes(t *testing.T) {
	h := newHarness(t, h265SDP(), 0, Config{})
	if tr := h.tr.info; tr.Codec != gaal.H265 || tr.Width != 1920 || tr.Height != 1080 {
		t.Fatalf("%+v", tr)
	}
	ps := h.tr.info.ParameterSets
	idr := filled(19<<1, 3000)
	idr[1], idr[2] = 1, 0x80
	trail := filled(1<<1, 40)
	trail[1], trail[2] = 1, 0x80
	frags := fuH265(idr, 1000)
	for i, f := range frags {
		h.send(0, i == len(frags)-1, f)
	}
	ap := []byte{48 << 1, 1}
	for _, n := range [][]byte{trail[:20], trail[:30]} {
		ap = append(ap, byte(len(n)>>8), byte(len(n)))
		ap = append(ap, n...)
	}
	h.send(3000, true, ap)
	h.send(6000, true, trail)
	out := h.out()
	if len(out) != 3 {
		t.Fatalf("got %d units", len(out))
	}
	if want := annexB(ps[0], ps[1], ps[2], idr); !bytes.Equal(out[0].Data, want) || !out[0].Keyframe {
		t.Fatalf("keyframe unit wrong: %d vs %d", len(out[0].Data), len(want))
	}
	if want := annexB(trail[:20], trail[:30]); !bytes.Equal(out[1].Data, want) || out[1].Keyframe {
		t.Fatal("aggregation packet wrong")
	}
	if want := annexB(trail); !bytes.Equal(out[2].Data, want) {
		t.Fatal("single NAL unit wrong")
	}
}

func TestMJPEGRoundTrip(t *testing.T) {
	for _, tc := range []struct{ q, wire int }{{50, 50}, {80, 255}} {
		img := image.NewYCbCr(image.Rect(0, 0, 64, 48), image.YCbCrSubsampleRatio420)
		for i := range img.Y {
			img.Y[i] = byte(i * 7 % 240)
		}
		for i := range img.Cb {
			img.Cb[i], img.Cr[i] = byte(100+i%40), byte(150-i%40)
		}
		var enc bytes.Buffer
		if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: tc.q}); err != nil {
			t.Fatal(err)
		}
		want, _ := jpeg.Decode(bytes.NewReader(enc.Bytes()))
		scan, tables := splitJPEG(t, enc.Bytes())

		h := newHarness(t, "v=0\nm=video 0 RTP/AVP 26\na=control:t\n", 0, Config{})
		const chunk = 300
		for off, first := 0, true; off < len(scan); first = false {
			hdr := []byte{0, byte(off >> 16), byte(off >> 8), byte(off), 1, byte(tc.wire), 64 / 8, 48 / 8}
			if first && tc.wire >= 128 {
				hdr = append(hdr, 0, 0, 0, 128)
				hdr = append(hdr, tables...)
			}
			n := min(chunk, len(scan)-off)
			h.send(9000, off+n == len(scan), append(hdr, scan[off:off+n]...))
			off += n
		}
		out := h.out()
		if len(out) != 1 || !out[0].Keyframe {
			t.Fatalf("q%d: got %d units", tc.q, len(out))
		}
		got, err := jpeg.Decode(bytes.NewReader(out[0].Data))
		if err != nil {
			t.Fatalf("q%d: rebuilt JPEG does not decode: %v", tc.q, err)
		}
		for y := 0; y < 48; y++ {
			for x := 0; x < 64; x++ {
				if color.YCbCrModel.Convert(got.At(x, y)) != color.YCbCrModel.Convert(want.At(x, y)) {
					t.Fatalf("q%d: pixel %d,%d differs", tc.q, x, y)
				}
			}
		}
		if tr := h.c.Tracks()[0]; tr.Width != 64 || tr.Height != 48 {
			t.Fatalf("size %dx%d", tr.Width, tr.Height)
		}
	}
}

// splitJPEG returns the entropy coded data and the two quantization tables.
func splitJPEG(t *testing.T, b []byte) (scan, tables []byte) {
	t.Helper()
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			t.Fatal("bad marker")
		}
		m := b[i+1]
		n := int(b[i+2])<<8 | int(b[i+3])
		seg := b[i+4 : i+2+n]
		if m == 0xDB {
			for len(seg) >= 65 {
				tables = append(tables, seg[1:65]...)
				seg = seg[65:]
			}
		}
		if m == 0xDA {
			return b[i+2+n : len(b)-2], tables
		}
		i += 2 + n
	}
	t.Fatal("no scan")
	return nil, nil
}

const aacSDP = "v=0\nm=audio 0 RTP/AVP 97\na=rtpmap:97 MPEG4-GENERIC/16000/1\n" +
	"a=fmtp:97 streamtype=5;mode=AAC-hbr;config=1408;sizelength=13;indexlength=3;indexdeltalength=3\n"

func aacPayload(sizes []int, data []byte) []byte {
	bits := len(sizes) * 16
	out := []byte{byte(bits >> 8), byte(bits)}
	for _, s := range sizes {
		out = append(out, byte(s>>5), byte(s<<3))
	}
	return append(out, data...)
}

func TestAACGeneric(t *testing.T) {
	h := newHarness(t, aacSDP, 0, Config{})
	a, b := bytes.Repeat([]byte{1}, 10), bytes.Repeat([]byte{2}, 20)
	h.send(16000, true, aacPayload([]int{10, 20}, append(append([]byte{}, a...), b...)))
	big := bytes.Repeat([]byte{3}, 100)
	h.send(32000, false, aacPayload([]int{100}, big[:60]))
	h.send(32000, true, aacPayload([]int{100}, big[60:]))
	out := h.out()
	if len(out) != 3 {
		t.Fatalf("got %d frames", len(out))
	}
	if !bytes.Equal(out[0].Data, a) || !bytes.Equal(out[1].Data, b) || !bytes.Equal(out[2].Data, big) {
		t.Fatal("frame contents")
	}
	if out[1].PTS-out[0].PTS != 64*time.Millisecond || !out[0].Keyframe {
		t.Fatalf("PTS step %v", out[1].PTS-out[0].PTS)
	}
	if c := h.c.Tracks()[0].AudioConfig(); c.Codec != gaal.AAC || c.SampleRate != 16000 || c.Channels != 1 || len(c.Extra) != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestLATM(t *testing.T) {
	h := newHarness(t, "v=0\nm=audio 0 RTP/AVP 100\na=rtpmap:100 MP4A-LATM/44100/2\na=fmtp:100 cpresent=0;config=400024203FC0\n", 0, Config{})
	body := bytes.Repeat([]byte{7}, 300)
	p := append([]byte{255, 45}, body...)
	h.send(100, false, p[:100])
	h.send(100, true, p[100:])
	out := h.out()
	if len(out) != 1 || !bytes.Equal(out[0].Data, body) {
		t.Fatalf("got %d frames", len(out))
	}
}

func TestPassThroughAudio(t *testing.T) {
	for _, sdp := range []string{"v=0\nm=audio 0 RTP/AVP 0\n", "v=0\nm=audio 0 RTP/AVP 8\n", "v=0\nm=audio 0 RTP/AVP 111\na=rtpmap:111 opus/48000/2\n"} {
		h := newHarness(t, sdp, 0, Config{})
		h.send(0, false, []byte{1, 2, 3})
		h.send(960, false, []byte{4})
		out := h.out()
		if len(out) != 2 || !bytes.Equal(out[1].Data, []byte{4}) || !out[1].Keyframe {
			t.Fatalf("%q: %d frames", sdp, len(out))
		}
	}
}

func TestQueueOverflowDropsOldestAndFlags(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{QueueSize: 2})
	for i := 0; i < 4; i++ {
		h.send(uint32(i*3000), true, pNAL(20))
	}
	out := h.out()
	if len(out) != 2 || out[0].RTPTimestamp != 6000 {
		t.Fatalf("%d units", len(out))
	}
	if h.c.Stats().Tracks[0].QueueDrops != 2 {
		t.Fatalf("%+v", h.c.Stats().Tracks[0])
	}
	h.send(12000, true, pNAL(20))
	if o := h.out(); len(o) != 1 || !o[0].Discontinuity {
		t.Fatal("next packet after a queue drop is not flagged")
	}
}

func TestTimestampWrap(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.send(0xFFFFF000, true, pNAL(20))
	h.send(0x00000FA0, true, pNAL(20))
	out := h.out()
	want := time.Duration(0x1FA0) * time.Second / 90000
	if len(out) != 2 || out[1].PTS != want {
		t.Fatalf("PTS %v want %v", out[1].PTS, want)
	}
}

func TestClockHintAlignsTracks(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	h.tr.setClockHint(1000)
	h.send(1900, true, pNAL(20))
	if out := h.out(); out[0].PTS != 10*time.Millisecond {
		t.Fatalf("PTS %v", out[0].PTS)
	}
}

func TestSenderReportGivesWallclock(t *testing.T) {
	h := newHarness(t, h264SDP(), 0, Config{})
	when := time.Unix(1_700_000_000, 0)
	ntp := uint64(when.Unix()+ntpEpochOffset) << 32
	h.tr.noteSR(rtcpEvents{srNTP: ntp, srRTP: 9000}, h.now)
	h.send(18000, true, pNAL(20))
	if got := h.out()[0].Wallclock; !got.Equal(when.Add(100 * time.Millisecond)) {
		t.Fatalf("wallclock %v", got)
	}
}
