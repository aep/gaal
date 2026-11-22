//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func needVAAPI(t testing.TB, c Codec, d Direction) {
	t.Helper()
	for _, b := range New(Options{Backend: "vaapi"}).Backends(c, d) {
		if b.Name == "vaapi" {
			return
		}
	}
	t.Skipf("no vaapi %s %s on this machine", c, d)
}

// TestVAAPISteadyStateAllocs extends TestSteadyStateAllocs to this backend.
func TestVAAPISteadyStateAllocs(t *testing.T) {
	for _, tc := range []struct {
		codec    Codec
		enc, dec string
		w, h     int
	}{
		{H264, "vaapi", "vaapi", 320, 240},
		{H264, "vaapi", "vaapi", 1920, 1080},
		{H264, "vaapi", "nvdec", 320, 240},
		{H264, "nvenc", "vaapi", 320, 240},
		{H264, "openh264", "vaapi", 320, 240},
		{JPEG, "turbojpeg", "vaapi", 320, 240},
	} {
		t.Run(fmt.Sprintf("%s/%s-%s/%d", tc.codec, tc.enc, tc.dec, tc.h), func(t *testing.T) {
			cfg := VideoConfig{Codec: tc.codec, Width: tc.w, Height: tc.h}
			enc, err := New(Options{Backend: tc.enc}).NewVideoEncoder(cfg)
			if err != nil {
				t.Skip(err)
			}
			defer enc.Close()
			dec, err := New(Options{Backend: tc.dec}).NewVideoDecoder(cfg)
			if err != nil {
				t.Skip(err)
			}
			defer dec.Close()
			src := testFrame(tc.w, tc.h, 0)
			out := 0
			onFrame := func(f *VideoFrame) { out++; f.Release() }
			onPacket := func(p *EncodedPacket) {
				if err := dec.Send(p.Packet); err != nil {
					t.Fatal(err)
				}
				p.Release()
				drain(t, dec, onFrame)
			}
			step := func() {
				src.PTS += time.Millisecond
				if err := enc.Send(src); err != nil {
					t.Fatal(err)
				}
				drain(t, enc, onPacket)
			}
			for i := 0; i < 30; i++ {
				step()
			}
			if out == 0 {
				t.Fatal("no frames decoded")
			}
			runs := 200
			if tc.h > 500 {
				runs = 50
			}
			allocs := testing.AllocsPerRun(runs, step)
			t.Logf("%.2f allocations per frame", allocs)
			if allocs != 0 {
				t.Errorf("%.2f allocations per frame", allocs)
			}
		})
	}
}

// splitAccessUnits cuts an Annex B stream into access units.
func splitAccessUnits(b []byte) [][]byte {
	var starts []int
	sawVCL := false
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		at := i
		if at > 0 && b[at-1] == 0 {
			at--
		}
		t := b[i+3] & 31
		switch {
		case t == 1 || t == 5:
			if b[i+4]&0x80 != 0 { // first_mb_in_slice is zero
				if sawVCL || len(starts) == 0 {
					starts = append(starts, at)
				}
				sawVCL = false
			}
			sawVCL = true
		case t >= 6 && t <= 9:
			if sawVCL || len(starts) == 0 {
				starts = append(starts, at)
			}
			sawVCL = false
		}
		i += 3
	}
	out := make([][]byte, len(starts))
	for i, s := range starts {
		end := len(b)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out[i] = b[s:end]
	}
	return out
}

func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out
}

type refClip struct {
	packets []Packet
	frames  [][]byte // I420, display order
	w, h    int
}

// makeRefClip encodes with libx264 and decodes with FFmpeg's software decoder,
// which gives the bit-exact expected output in display order.
func makeRefClip(t *testing.T, w, h int, x264 ...string) *refClip {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool, " not installed")
		}
	}
	if !strings.Contains(string(run(t, "ffmpeg", "-hide_banner", "-encoders")), "libx264") {
		t.Skip("ffmpeg has no libx264")
	}
	dir := t.TempDir()
	mkv, h264, yuv := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "a.h264"), filepath.Join(dir, "a.yuv")
	args := []string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30:d=2", w, h),
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "medium", "-b:v", "1M"}
	args = append(append(args, x264...), mkv)
	run(t, "ffmpeg", args...)
	run(t, "ffmpeg", "-v", "error", "-i", mkv, "-c", "copy", "-bsf:v", "h264_mp4toannexb", "-f", "h264", h264)
	run(t, "ffmpeg", "-v", "error", "-i", mkv, "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "yuv420p", yuv)
	var pts []time.Duration
	for _, line := range strings.Fields(string(run(t, "ffprobe", "-v", "error", "-select_streams", "v",
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", mkv))) {
		v, err := strconv.ParseFloat(strings.TrimRight(line, ","), 64)
		if err != nil {
			t.Fatal(err)
		}
		pts = append(pts, time.Duration(v*float64(time.Second)))
	}
	stream, err := os.ReadFile(h264)
	if err != nil {
		t.Fatal(err)
	}
	aus := splitAccessUnits(stream)
	if len(aus) != len(pts) {
		t.Fatalf("%d access units, %d timestamps", len(aus), len(pts))
	}
	c := &refClip{w: w, h: h}
	for i, au := range aus {
		c.packets = append(c.packets, Packet{Data: au, PTS: pts[i]})
	}
	raw, err := os.ReadFile(yuv)
	if err != nil {
		t.Fatal(err)
	}
	size := frameSize(w, h, I420)
	for len(raw) >= size {
		c.frames = append(c.frames, raw[:size])
		raw = raw[size:]
	}
	if len(c.frames) != len(aus) {
		t.Fatalf("%d reference frames for %d access units", len(c.frames), len(aus))
	}
	return c
}

// withoutSPS removes sequence parameter sets from an access unit.
func withoutSPS(au []byte) []byte {
	var out []byte
	for nal, rest := vaNextNAL(au); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) > 0 && nal[0]&31 != 7 {
			out = append(append(out, 0, 0, 1), nal...)
		}
	}
	return out
}

// forgetReorderLimit makes the decoder treat the stream as if its SPS had no
// bitstream restriction, by loading the SPS once and adjusting the parsed copy.
func forgetReorderLimit(t *testing.T, c *refClip, d *vaH264Decoder) {
	t.Helper()
	au := c.packets[0].Data
	for nal, rest := vaNextNAL(au); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) > 0 && nal[0]&31 == 7 {
			if err := d.handleNAL(nal); err != nil {
				t.Fatal(err)
			}
			s := d.sps[nal2SPSID(t, nal)]
			s.restricted, s.reorder = false, -1
			s.dpbFrames = max(min(vaMaxDpbMbs(s)/(s.mbW*s.mbH), 16), s.numRef, 1)
		}
	}
	for i := range c.packets {
		c.packets[i].Data = withoutSPS(c.packets[i].Data)
	}
}

func nal2SPSID(t *testing.T, nal []byte) int {
	var s vaSPS
	if err := parseSPS(nal, &s); err != nil {
		t.Fatal(err)
	}
	return s.id
}

// checkClip decodes a clip and compares every output with the reference.
// It returns the largest number of packets a frame waited for.
func checkClip(t *testing.T, c *refClip, prepare ...func(*vaH264Decoder)) int {
	t.Helper()
	a, err := newVAH264Decoder(BackendInfo{"vaapi", H264, Decode, true})
	if err != nil {
		t.Fatal(err)
	}
	dec := a.(*vaH264Decoder)
	defer dec.Close()
	for _, fn := range prepare {
		fn(dec)
	}
	want := make([]time.Duration, len(c.packets))
	for i, p := range c.packets {
		want[i] = p.PTS
	}
	slices.Sort(want)
	sentAt := map[time.Duration]int{}
	n, sent, delay := 0, 0, 0
	i420 := NewVideoFrame(c.w, c.h, I420)
	got := func(f *VideoFrame) {
		defer f.Release()
		if n >= len(c.frames) {
			t.Fatalf("more than %d frames", len(c.frames))
		}
		if f.Width != c.w || f.Height != c.h || f.Format != NV12 {
			t.Fatalf("frame %d is %dx%d format %d", n, f.Width, f.Height, f.Format)
		}
		if f.PTS != want[n] {
			t.Errorf("frame %d: PTS %v, want %v", n, f.PTS, want[n])
		}
		delay = max(delay, sent-sentAt[f.PTS]-1)
		if err := f.ToI420(i420); err != nil {
			t.Fatal(err)
		}
		ref := c.frames[n]
		ySize := c.w * c.h
		if !bytes.Equal(i420.Y[:ySize], ref[:ySize]) {
			t.Fatalf("frame %d (PTS %v): luma differs from the reference, PSNR %.1f dB", n, f.PTS,
				psnrY(i420, &VideoFrame{Width: c.w, Height: c.h, Y: ref, YStride: c.w}))
		}
		cw, ch := (c.w+1)/2, (c.h+1)/2
		if !bytes.Equal(i420.U[:cw*ch], ref[ySize:ySize+cw*ch]) || !bytes.Equal(i420.V[:cw*ch], ref[ySize+cw*ch:]) {
			t.Fatalf("frame %d: chroma differs from the reference", n)
		}
		n++
	}
	for _, p := range c.packets {
		sentAt[p.PTS] = sent
		sent++
		if err := dec.Send(p); err != nil {
			t.Fatalf("packet %d: %v", sent-1, err)
		}
		drain(t, dec, got)
	}
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, dec, got)
	if n != len(c.frames) {
		t.Fatalf("decoded %d frames, want %d", n, len(c.frames))
	}
	return delay
}

// TestVAAPIDecodeConformance compares against FFmpeg's software decoder frame by
// frame across the coding tools that affect reference handling and ordering.
func TestVAAPIDecodeConformance(t *testing.T) {
	needVAAPI(t, H264, Decode)
	for _, tc := range []struct {
		name     string
		w, h     int
		args     []string
		maxDelay int // -1 when reordering is expected and not bounded here
	}{
		{"bframes", 640, 360, []string{"-bf", "3", "-refs", "4"}, -1},
		{"bpyramid-weighted", 640, 360, []string{"-bf", "4", "-refs", "5", "-x264-params", "b-pyramid=normal:weightb=1:weightp=2:b-adapt=2"}, -1},
		{"cavlc", 640, 360, []string{"-bf", "2", "-x264-params", "cabac=0"}, -1},
		{"main", 640, 360, []string{"-profile:v", "main", "-bf", "2"}, -1},
		{"baseline", 640, 360, []string{"-profile:v", "baseline"}, 0},
		{"no-bframes", 640, 360, []string{"-bf", "0"}, 0},
		{"intra", 640, 360, []string{"-g", "1"}, 0},
		{"slices", 640, 360, []string{"-bf", "2", "-x264-params", "slices=4"}, -1},
		{"scaling-lists", 640, 360, []string{"-bf", "2", "-x264-params", "cqm=jvt"}, -1},
		{"no-8x8", 640, 360, []string{"-bf", "1", "-x264-params", "8x8dct=0"}, -1},
		{"open-gop", 640, 360, []string{"-bf", "3", "-g", "12", "-x264-params", "open-gop=1"}, -1},
		{"short-gop", 640, 360, []string{"-bf", "2", "-g", "5"}, -1},
		{"cropped", 642, 362, []string{"-bf", "2"}, -1},
		{"mbaff", 640, 360, []string{"-bf", "2", "-flags", "+ildct+ilme", "-x264-params", "interlaced=1"}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := makeRefClip(t, tc.w, tc.h, tc.args...)
			delay := checkClip(t, c)
			t.Logf("%d frames, largest output delay %d packets", len(c.frames), delay)
			if tc.maxDelay >= 0 && delay > tc.maxDelay {
				t.Errorf("output delay %d packets, want at most %d", delay, tc.maxDelay)
			}
		})
	}
}

// TestVAAPINoReorderLimit decodes B-frames without max_num_reorder_frames, which
// exercises the DPB bumping path used for streams that do not state a limit.
func TestVAAPINoReorderLimit(t *testing.T) {
	needVAAPI(t, H264, Decode)
	c := makeRefClip(t, 640, 360, "-bf", "3", "-refs", "3")
	delay := checkClip(t, c, func(d *vaH264Decoder) { forgetReorderLimit(t, c, d) })
	t.Logf("largest output delay %d packets", delay)
}

// TestVAAPIPOCType1 checks picture order against values worked out by hand from
// clause 8.2.1.2: a cycle of two reference frames with offsets 2 and 4, with
// non-reference frames at offset -1.
func TestVAAPIPOCType1(t *testing.T) {
	sps := &vaSPS{log2FrameNum: 4, pocType: 1, cycle: 2, offsetNonRef: -1, offsetTopBottom: 1}
	sps.offsetRef[0], sps.offsetRef[1] = 2, 4
	d := &vaH264Decoder{}
	for i, tc := range []struct {
		frameNum int32
		refIdc   uint8
		nalType  uint8
		top, bot int32
	}{
		{0, 3, 5, 0, 1},
		{1, 2, 1, 2, 3},
		{2, 0, 1, 1, 2}, // non-reference frame after frame_num 1
		{2, 2, 1, 6, 7},
		{3, 2, 1, 8, 9},
		{15, 2, 1, 44, 45}, // absFrameNum 15: 7 cycles (42) + 2
		{0, 2, 1, 48, 49},  // frame_num wrapped, absFrameNum 16: 7 cycles + 2 + 4
	} {
		p := &vaPic{}
		d.computePOC(&vaSlice{frameNum: tc.frameNum, refIdc: tc.refIdc, nalType: tc.nalType}, sps, p)
		if p.top != tc.top || p.bottom != tc.bot {
			t.Errorf("case %d: POC %d/%d, want %d/%d", i, p.top, p.bottom, tc.top, tc.bot)
		}
	}
}

// TestVAAPIFrameNumWrap runs past the 8-bit frame_num the encoder uses.
func TestVAAPIFrameNumWrap(t *testing.T) {
	needVAAPI(t, H264, Encode)
	const frames = 300
	cfg := VideoConfig{Codec: H264, Width: 160, Height: 120, KeyframeEvery: 10 * frames}
	pkts := encodeClip(t, "vaapi", cfg, frames)
	for _, di := range New(Options{}).Backends(H264, Decode) {
		t.Run(di.Name, func(t *testing.T) {
			dec, err := New(Options{Backend: di.Name}).NewVideoDecoder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			n := 0
			got := func(f *VideoFrame) {
				if q := psnrY(testFrame(160, 120, n), f); q < 25 {
					t.Errorf("frame %d: PSNR %.1f dB", n, q)
				}
				n++
				f.Release()
			}
			for _, p := range pkts {
				if err := dec.Send(p); err != nil {
					t.Fatal(err)
				}
				drain(t, dec, got)
			}
			if err := dec.Flush(); err != nil {
				t.Fatal(err)
			}
			drain(t, dec, got)
			if n != frames {
				t.Fatalf("decoded %d frames, want %d", n, frames)
			}
		})
	}
}

// TestVAAPIJPEGUnsupported checks that images the hardware path must not receive
// are refused with a plain error, so that failover can use another backend, and
// that they leave the decoder usable. The headers are edited, so nothing
// unsupported ever reaches the device.
func TestVAAPIJPEGUnsupported(t *testing.T) {
	needVAAPI(t, JPEG, Decode)
	src := testFrame(64, 48, 0)
	good, err := New(Options{Backend: "go-image-jpeg"}).EncodeJPEG(src, 90)
	if err != nil {
		t.Skip(err)
	}
	defer good.Release()
	sof := bytes.Index(good.Data, []byte{0xFF, 0xC0})
	if sof < 0 {
		t.Fatal("no SOF0 marker")
	}
	edit := func(fn func(b []byte)) []byte {
		b := bytes.Clone(good.Data)
		fn(b)
		return b
	}
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: JPEG})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for name, data := range map[string][]byte{
		"4:2:2":       edit(func(b []byte) { b[sof+11] = 0x21 }),
		"4:4:4":       edit(func(b []byte) { b[sof+11] = 0x11 }),
		"progressive": edit(func(b []byte) { b[sof+1] = 0xC2 }),
		"tiny":        edit(func(b []byte) { b[sof+5], b[sof+6] = 0, 8 }),
	} {
		err := dec.Send(Packet{Data: data})
		if err == nil || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want a plain error", name, err)
		}
	}
	if err := dec.Send(good.Packet); err != nil {
		t.Fatal(err)
	}
	f, err := dec.Receive()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if q := psnrY(src, f); q < 30 {
		t.Errorf("PSNR %.1f dB", q)
	}
}

func nalTypes(b []byte) []byte {
	var out []byte
	for nal, rest := vaNextNAL(b); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) > 0 {
			out = append(out, nal[0]&31)
		}
	}
	return out
}

func TestVAAPIEncoderStream(t *testing.T) {
	needVAAPI(t, H264, Encode)
	const w, h, frames, every = 322, 242 &^ 1, 40, 15
	cfg := VideoConfig{Codec: H264, Width: w &^ 1, Height: h, Bitrate: 2_000_000, KeyframeEvery: every}
	enc, err := New(Options{Backend: "vaapi"}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	const forced = 7
	var pkts []Packet
	for i := 0; i < frames; i++ {
		f := testFrame(cfg.Width, cfg.Height, i)
		f.ForceKeyframe = i == forced
		if err := enc.Send(f); err != nil {
			t.Fatal(err)
		}
		n := len(pkts)
		drain(t, enc, func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() })
		if len(pkts) != n+1 {
			t.Fatalf("frame %d produced %d packets", i, len(pkts)-n)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Receive(); err == nil {
		t.Fatal("output after the last frame")
	}
	since := 0
	for i, p := range pkts {
		wantKey := i == 0 || i == forced || since == every
		if wantKey {
			since = 0
		}
		since++
		if p.Keyframe != wantKey {
			t.Errorf("packet %d: keyframe %v, want %v", i, p.Keyframe, wantKey)
		}
		types := nalTypes(p.Data)
		if wantKey {
			if !bytes.Equal(types, []byte{7, 8, 5}) {
				t.Errorf("packet %d: NAL types %v, want SPS, PPS, IDR", i, types)
			}
		} else if !bytes.Equal(types, []byte{1}) {
			t.Errorf("packet %d: NAL types %v", i, types)
		}
		if p.PTS != time.Duration(i)*time.Second/30 {
			t.Errorf("packet %d: PTS %v", i, p.PTS)
		}
	}
	// Each decoder must emit every frame as soon as it is sent, and be able to
	// join at a later keyframe.
	for _, di := range New(Options{}).Backends(H264, Decode) {
		t.Run(di.Name, func(t *testing.T) {
			dec, err := New(Options{Backend: di.Name}).NewVideoDecoder(VideoConfig{Codec: H264})
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			n := 0
			for i, p := range pkts[forced:] {
				if err := dec.Send(p); err != nil {
					t.Fatal(err)
				}
				drain(t, dec, func(f *VideoFrame) {
					if f.Width != cfg.Width || f.Height != cfg.Height {
						t.Errorf("frame is %dx%d", f.Width, f.Height)
					}
					src := testFrame(cfg.Width, cfg.Height, forced+n)
					if q := psnrY(src, f); q < 25 {
						t.Errorf("frame %d: PSNR %.1f dB", n, q)
					}
					n++
					f.Release()
				})
				if di.Name == "vaapi" && n != i+1 {
					t.Fatalf("after packet %d: %d frames out", i, n)
				}
			}
			if err := dec.Flush(); err != nil {
				t.Fatal(err)
			}
			drain(t, dec, func(f *VideoFrame) { n++; f.Release() })
			if n != frames-forced {
				t.Fatalf("decoded %d frames, want %d", n, frames-forced)
			}
		})
	}
}

func TestVAAPIEncoderBitrate(t *testing.T) {
	needVAAPI(t, H264, Encode)
	const fps, bitrate = 30, 1_000_000
	cfg := VideoConfig{Codec: H264, Width: 640, Height: 360, FPS: fps, Bitrate: bitrate}
	enc, err := New(Options{Backend: "vaapi"}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	f := NewVideoFrame(cfg.Width, cfg.Height, I420)
	total := 0
	for i := 0; i < 3*fps; i++ {
		// A moving texture with detail in every macroblock.
		for y := 0; y < f.Height; y++ {
			row := f.Y[y*f.YStride:]
			for x := 0; x < f.Width; x++ {
				v := (x+3*i)*(x+3*i)/7 ^ (y+2*i)*(y+2*i)/5
				row[x] = byte(64 + v&127)
			}
		}
		for j := range f.U {
			f.U[j], f.V[j] = 128, 128
		}
		if err := enc.Send(f); err != nil {
			t.Fatal(err)
		}
		drain(t, enc, func(p *EncodedPacket) { total += len(p.Data); p.Release() })
	}
	got := float64(total*8) / 3
	t.Logf("%.0f kbit/s for a target of %d", got/1000, bitrate/1000)
	if got > 1.5*bitrate || got < 0.5*bitrate {
		t.Errorf("bitrate %.0f, target %d", got, bitrate)
	}
}

// TestVAAPICorruptInput checks that damage is reported as ErrCorrupt, does not
// wedge the device and that decoding recovers at the next keyframe.
func TestVAAPICorruptInput(t *testing.T) {
	needVAAPI(t, H264, Decode)
	needVAAPI(t, H264, Encode)
	cfg := VideoConfig{Codec: H264, Width: 320, Height: 240, KeyframeEvery: 10, Bitrate: 1_000_000}
	pkts := encodeClip(t, "vaapi", cfg, 30)
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	release := func(f *VideoFrame) { f.Release() }
	for _, junk := range [][]byte{{1, 2, 3}, {0, 0, 1, 0x65, 0xff}, {0, 0, 1, 0x67, 0}, {0, 0, 1, 0x68, 0xff}, {0, 0, 1, 0x41, 0x80}} {
		if err := dec.Send(Packet{Data: junk}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("Send(%x) = %v, want ErrCorrupt", junk, err)
		}
		drain(t, dec, release)
	}
	// A P frame before any keyframe has nothing to reference.
	if err := dec.Send(pkts[1]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("P frame first: %v, want ErrCorrupt", err)
	}
	seed := uint32(7)
	for i, p := range pkts[:20] {
		d := bytes.Clone(p.Data)
		if !p.Keyframe {
			for j := 0; j < 8; j++ {
				seed = seed*1664525 + 1013904223
				d[8+int(seed>>8)%(len(d)-8)] ^= byte(seed >> 24)
			}
		}
		if err := dec.Send(Packet{Data: d, PTS: p.PTS}); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("packet %d: %v", i, err)
		}
		drain(t, dec, release)
	}
	n := 0
	for _, p := range pkts[20:] {
		if err := dec.Send(p); err != nil {
			t.Fatal(err)
		}
		drain(t, dec, func(f *VideoFrame) {
			if q := psnrY(testFrame(320, 240, 20+n), f); q < 25 {
				t.Errorf("frame %d after recovery: PSNR %.1f dB", n, q)
			}
			n++
			f.Release()
		})
	}
	if n != 10 {
		t.Errorf("%d frames after recovery, want 10", n)
	}
}

func TestVAAPIResolutionChange(t *testing.T) {
	needVAAPI(t, H264, Decode)
	needVAAPI(t, H264, Encode)
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: H264})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	sizes := [][2]int{{320, 240}, {640, 360}, {176, 144}}
	var got [][2]int
	for _, s := range sizes {
		for _, p := range encodeClip(t, "vaapi", VideoConfig{Codec: H264, Width: s[0], Height: s[1]}, 3) {
			if err := dec.Send(p); err != nil {
				t.Fatal(err)
			}
			drain(t, dec, func(f *VideoFrame) { got = append(got, [2]int{f.Width, f.Height}); f.Release() })
		}
	}
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, dec, func(f *VideoFrame) { got = append(got, [2]int{f.Width, f.Height}); f.Release() })
	if len(got) != 9 {
		t.Fatalf("%d frames, want 9", len(got))
	}
	for i, g := range got {
		if g != sizes[i/3] {
			t.Errorf("frame %d is %v, want %v", i, g, sizes[i/3])
		}
	}
}

func TestVAAPIConcurrentStreams(t *testing.T) {
	needVAAPI(t, H264, Encode)
	needVAAPI(t, H264, Decode)
	errs := make(chan error, 4)
	for g := 0; g < 4; g++ {
		go func() {
			errs <- func() error {
				cfg := VideoConfig{Codec: H264, Width: 320, Height: 240}
				l := New(Options{Backend: "vaapi"})
				enc, err := l.NewVideoEncoder(cfg)
				if err != nil {
					return err
				}
				defer enc.Close()
				dec, err := l.NewVideoDecoder(cfg)
				if err != nil {
					return err
				}
				defer dec.Close()
				src := testFrame(320, 240, g)
				n := 0
				for i := 0; i < 50; i++ {
					if err := enc.Send(src); err != nil {
						return err
					}
					p, err := enc.Receive()
					if err != nil {
						return err
					}
					err = dec.Send(p.Packet)
					p.Release()
					if err != nil {
						return err
					}
					f, err := dec.Receive()
					if err != nil {
						return err
					}
					if q := psnrY(src, f); q < 25 {
						return fmt.Errorf("PSNR %.1f dB", q)
					}
					f.Release()
					n++
				}
				return nil
			}()
		}()
	}
	for g := 0; g < 4; g++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func BenchmarkH264VAAPI1080p(b *testing.B) { benchTranscode(b, H264, "vaapi", "vaapi", 1920, 1080) }
func BenchmarkH264VAAPINVDEC1080p(b *testing.B) {
	benchTranscode(b, H264, "vaapi", "nvdec", 1920, 1080)
}
func BenchmarkH264NVENCVAAPI1080p(b *testing.B) {
	benchTranscode(b, H264, "nvenc", "vaapi", 1920, 1080)
}
func BenchmarkJPEGTurboVAAPI1080p(b *testing.B) {
	benchTranscode(b, JPEG, "turbojpeg", "vaapi", 1920, 1080)
}

// BenchmarkVAAPIReadback compares the two ways of reading decoded surfaces.
func BenchmarkVAAPIReadback(b *testing.B) {
	defer func(v bool) { vaReadDerive = v }(vaReadDerive)
	for _, mode := range []struct {
		name   string
		derive bool
	}{{"derive", true}, {"getimage", false}} {
		b.Run(mode.name, func(b *testing.B) {
			vaReadDerive = mode.derive
			benchTranscode(b, H264, "vaapi", "vaapi", 1920, 1080)
		})
	}
}
