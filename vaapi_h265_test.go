//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// needEncoder skips the test when ffmpeg lacks a software encoder.
func needEncoder(t testing.TB, name string) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool, " not installed")
		}
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(out), name) {
		t.Skipf("ffmpeg has no %s", name)
	}
}

func runTool(t testing.TB, name string, args ...string) []byte {
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

// splitH265AUs cuts an Annex B stream into access units.
func splitH265AUs(b []byte) [][]byte {
	var starts []int
	sawVCL := false
	for i := 0; i+4 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		at := i
		if at > 0 && b[at-1] == 0 {
			at--
		}
		t := b[i+3] >> 1 & 63
		switch {
		case t <= 9 || (t >= 16 && t <= 21):
			if i+5 < len(b) && b[i+5]&0x80 != 0 {
				if sawVCL || len(starts) == 0 {
					starts = append(starts, at)
				}
				sawVCL = false
			}
			sawVCL = true
		case t >= 32 && t <= 35 || t == 39:
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

// readFrames loads a raw I420 file.
func readFrames(t testing.TB, path string, w, h int) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := frameSize(w, h, I420)
	var out [][]byte
	for len(raw) >= size {
		out = append(out, raw[:size])
		raw = raw[size:]
	}
	return out
}

// packetTimes returns the packet timestamps of a container file.
func packetTimes(t testing.TB, path string) []time.Duration {
	t.Helper()
	var pts []time.Duration
	for _, line := range strings.Fields(string(runTool(t, "ffprobe", "-v", "error", "-select_streams", "v",
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", path))) {
		v, err := strconv.ParseFloat(strings.TrimRight(line, ","), 64)
		if err != nil {
			t.Fatal(err)
		}
		pts = append(pts, time.Duration(v*float64(time.Second)))
	}
	return pts
}

// makeH265Clip encodes with libx265 and decodes with FFmpeg's software decoder.
func makeH265Clip(t testing.TB, w, h int, params string, args ...string) *refClip {
	t.Helper()
	needEncoder(t, "libx265")
	dir := t.TempDir()
	mkv, es, yuv := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "a.hevc"), filepath.Join(dir, "a.yuv")
	cmd := []string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30:d=2", w, h),
		"-pix_fmt", "yuv420p", "-c:v", "libx265", "-preset", "fast", "-b:v", "1M", "-tag:v", "hvc1"}
	x := "log-level=error"
	if params != "" {
		x += ":" + params
	}
	cmd = append(append(cmd, "-x265-params", x), args...)
	runTool(t, "ffmpeg", append(cmd, mkv)...)
	runTool(t, "ffmpeg", "-v", "error", "-i", mkv, "-c", "copy", "-bsf:v", "hevc_mp4toannexb", "-f", "hevc", es)
	runTool(t, "ffmpeg", "-v", "error", "-c:v", "hevc", "-i", mkv, "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", yuv)
	pts := packetTimes(t, mkv)
	stream, err := os.ReadFile(es)
	if err != nil {
		t.Fatal(err)
	}
	aus := splitH265AUs(stream)
	if len(aus) != len(pts) {
		t.Fatalf("%d access units, %d timestamps", len(aus), len(pts))
	}
	c := &refClip{w: w, h: h, frames: readFrames(t, yuv, w, h)}
	for i, au := range aus {
		c.packets = append(c.packets, Packet{Data: au, PTS: pts[i]})
	}
	if len(c.frames) != len(aus) {
		t.Fatalf("%d reference frames for %d access units", len(c.frames), len(aus))
	}
	return c
}

// decodeFFmpeg decodes an elementary stream with FFmpeg's software decoder.
func decodeFFmpeg(t testing.TB, decoder, format string, stream []byte, w, h int) [][]byte {
	t.Helper()
	dir := t.TempDir()
	in, yuv := filepath.Join(dir, "in"), filepath.Join(dir, "out.yuv")
	if err := os.WriteFile(in, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	runTool(t, "ffmpeg", "-v", "error", "-f", format, "-c:v", decoder, "-i", in, "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", yuv)
	return readFrames(t, yuv, w, h)
}

// compareFrame checks an NV12 output against an I420 reference, bit for bit.
func compareFrame(t testing.TB, n int, f *VideoFrame, ref []byte) {
	t.Helper()
	i420 := NewVideoFrame(f.Width, f.Height, I420)
	if err := f.ToI420(i420); err != nil {
		t.Fatal(err)
	}
	ySize := f.Width * f.Height
	if !bytes.Equal(i420.Y[:ySize], ref[:ySize]) {
		t.Fatalf("frame %d (PTS %v): luma differs, PSNR %.1f dB", n, f.PTS,
			psnrY(i420, &VideoFrame{Width: f.Width, Height: f.Height, Y: ref, YStride: f.Width}))
	}
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	if !bytes.Equal(i420.U[:cw*ch], ref[ySize:ySize+cw*ch]) || !bytes.Equal(i420.V[:cw*ch], ref[ySize+cw*ch:]) {
		t.Fatalf("frame %d: chroma differs", n)
	}
}

// checkDecoder decodes a clip and compares every output with the reference. It
// returns the largest number of packets a frame waited for.
func checkDecoder(t testing.TB, codec Codec, c *refClip) int {
	t.Helper()
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	sentAt := map[time.Duration]int{}
	n, sent, delay := 0, 0, 0
	got := func(f *VideoFrame) {
		defer f.Release()
		if n >= len(c.frames) {
			t.Fatalf("more than %d frames", len(c.frames))
		}
		if f.Width != c.w || f.Height != c.h || f.Format != NV12 {
			t.Fatalf("frame %d is %dx%d format %d", n, f.Width, f.Height, f.Format)
		}
		compareFrame(t, n, f, c.frames[n])
		delay = max(delay, sent-sentAt[f.PTS]-1)
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

var h265Cases = []struct {
	name, params string
	w, h         int
	args         []string
	maxDelay     int // -1 when reordering is expected
}{
	{"default", "", 640, 360, nil, -1},
	{"no-bframes", "bframes=0", 640, 360, nil, 0},
	{"zerolatency", "", 640, 360, []string{"-tune", "zerolatency"}, 0},
	{"pyramid", "bframes=4:b-pyramid=1:ref=4", 640, 360, nil, -1},
	{"no-pyramid", "bframes=3:b-pyramid=0:ref=3", 640, 360, nil, -1},
	{"weighted", "bframes=3:weightp=1:weightb=1", 640, 360, nil, -1},
	{"slices", "slices=4:bframes=2", 640, 360, nil, -1},
	{"no-wpp", "wpp=0:bframes=2", 640, 360, nil, -1},
	{"scaling-list", "scaling-list=default:bframes=2", 640, 360, nil, -1},
	{"open-gop", "open-gop=1:keyint=15:min-keyint=15:bframes=3", 640, 360, nil, -1},
	{"closed-gop", "open-gop=0:keyint=12:min-keyint=12:bframes=2", 640, 360, nil, -1},
	{"short-gop", "keyint=2:bframes=0", 640, 360, nil, 0},
	{"cropped", "bframes=2", 642, 362, nil, -1},
	{"amp-rect-tskip", "amp=1:rect=1:tskip=1:bframes=2", 640, 360, nil, -1},
	{"no-sao-no-dbf", "sao=0:deblock=0:bframes=2", 640, 360, nil, -1},
	// With 16x16 CTBs and SAO, VCN differs from the reference in a few chroma
	// samples, so that combination is not compared here.
	{"small-ctu", "ctu=16:min-cu-size=8:bframes=2:sao=0", 640, 360, nil, -1},
	{"ctu32", "ctu=32:bframes=2", 640, 360, nil, -1},
	{"lossless", "lossless=1:bframes=2", 320, 240, nil, -1},
	{"strong-intra", "strong-intra-smoothing=1:cu-lossless=1", 640, 360, nil, -1},
	{"long-gop-poc-wrap", "keyint=-1:bframes=2:log2-max-poc-lsb=4", 320, 240, nil, -1},
}

// dryDecodeH265 runs a clip through all parsing and DPB logic without a device.
func dryDecodeH265(t testing.TB, c *refClip) {
	t.Helper()
	d := newH265Decoder(BackendInfo{"vaapi", H265, Decode, true})
	var pocs []int32
	d.dryEmit = func(poc int32, _ time.Duration) { pocs = append(pocs, poc) }
	for i, p := range c.packets {
		if err := d.Send(p); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(pocs) != len(c.frames) {
		t.Fatalf("%d frames output, want %d", len(pocs), len(c.frames))
	}
	// Within a coded video sequence the output must be in POC order.
	for i := 1; i < len(pocs); i++ {
		if pocs[i] <= pocs[i-1] && pocs[i] != 0 {
			t.Fatalf("output %d has POC %d after %d", i, pocs[i], pocs[i-1])
		}
	}
}

// TestH265ParserClips checks parsing and output order without touching the GPU.
func TestH265ParserClips(t *testing.T) {
	for _, tc := range h265Cases {
		t.Run(tc.name, func(t *testing.T) {
			dryDecodeH265(t, makeH265Clip(t, tc.w, tc.h, tc.params, tc.args...))
		})
	}
}

// TestVAAPIH265Conformance compares against FFmpeg's software decoder.
func TestVAAPIH265Conformance(t *testing.T) {
	needVAAPI(t, H265, Decode)
	for _, tc := range h265Cases {
		t.Run(tc.name, func(t *testing.T) {
			c := makeH265Clip(t, tc.w, tc.h, tc.params, tc.args...)
			dryDecodeH265(t, c) // never send what the parser would not accept
			delay := checkDecoder(t, H265, c)
			t.Logf("%d frames, largest output delay %d packets", len(c.frames), delay)
			if tc.maxDelay >= 0 && delay > tc.maxDelay {
				t.Errorf("output delay %d packets, want at most %d", delay, tc.maxDelay)
			}
		})
	}
}

func h265NalTypes(b []byte) []byte {
	var out []byte
	for nal, rest := vaNextNAL(b); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) > 0 {
			out = append(out, nal[0]>>1&63)
		}
	}
	return out
}

// TestVAAPIH265StartAtCRA joins an open-GOP stream at a CRA picture, where the
// RASL pictures that follow it must be dropped.
func TestVAAPIH265StartAtCRA(t *testing.T) {
	needVAAPI(t, H265, Decode)
	c := makeH265Clip(t, 640, 360, "open-gop=1:keyint=15:min-keyint=15:bframes=3")
	start := -1
	for i, p := range c.packets {
		if i > 0 && slices.Contains(h265NalTypes(p.Data), h265NalCRA) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("stream has no CRA picture")
	}
	cut := &refClip{w: c.w, h: c.h, packets: c.packets[start:]}
	var stream []byte
	for _, p := range cut.packets {
		stream = append(stream, p.Data...)
	}
	cut.frames = decodeFFmpeg(t, "hevc", "hevc", stream, c.w, c.h)
	dec := newH265Decoder(BackendInfo{"vaapi", H265, Decode, true})
	n := 0
	dec.dryEmit = func(int32, time.Duration) { n++ }
	for _, p := range cut.packets {
		if err := dec.Send(p); err != nil {
			t.Fatal(err)
		}
	}
	dec.Flush()
	if n != len(cut.frames) {
		t.Fatalf("parser outputs %d frames, FFmpeg %d", n, len(cut.frames))
	}
	if len(cut.frames) >= len(cut.packets) {
		t.Fatalf("no RASL pictures were dropped")
	}
	checkDecoder(t, H265, cut)
}

// TestVAAPIH265ResolutionChange switches size at an IDR, with new parameter sets.
func TestVAAPIH265ResolutionChange(t *testing.T) {
	needVAAPI(t, H265, Decode)
	sizes := [][2]int{{640, 360}, {320, 240}, {400, 300}}
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: H265})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	type want struct {
		w, h  int
		frame []byte
	}
	var wants []want
	n := 0
	got := func(f *VideoFrame) {
		defer f.Release()
		if n >= len(wants) {
			t.Fatal("too many frames")
		}
		w := wants[n]
		if f.Width != w.w || f.Height != w.h {
			t.Fatalf("frame %d is %dx%d, want %dx%d", n, f.Width, f.Height, w.w, w.h)
		}
		compareFrame(t, n, f, w.frame)
		n++
	}
	for _, s := range sizes {
		c := makeH265Clip(t, s[0], s[1], "bframes=2:keyint=30")
		for _, fr := range c.frames {
			wants = append(wants, want{s[0], s[1], fr})
		}
		for _, p := range c.packets {
			if err := dec.Send(p); err != nil {
				t.Fatal(err)
			}
			drain(t, dec, got)
		}
	}
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, dec, got)
	if n != len(wants) {
		t.Fatalf("decoded %d frames, want %d", n, len(wants))
	}
}

// TestH265ParserMutation feeds damaged access units to the parsers and DPB logic
// without a device. They must fail cleanly, never panic.
func TestH265ParserMutation(t *testing.T) {
	c := makeH265Clip(t, 320, 240, "bframes=3:ref=3:keyint=20:slices=2:scaling-list=default")
	rng := rand.New(rand.NewSource(1))
	iters := 300
	if testing.Short() {
		iters = 50
	}
	for it := 0; it < iters; it++ {
		d := newH265Decoder(BackendInfo{"vaapi", H265, Decode, true})
		for i, p := range c.packets {
			if i > 15 {
				break
			}
			b := bytes.Clone(p.Data)
			if rng.Intn(3) != 0 {
				for j := 0; j < 1+rng.Intn(6); j++ {
					at := rng.Intn(min(len(b), 80)) // headers live at the start
					switch rng.Intn(3) {
					case 0:
						b[at] ^= 1 << rng.Intn(8)
					case 1:
						b[at] = byte(rng.Intn(256))
					case 2:
						b = b[:at+1+rng.Intn(max(len(b)-at-1, 1))]
					}
					if len(b) == 0 {
						b = []byte{0}
					}
				}
			}
			if err := d.Send(Packet{Data: b}); err != nil && !errors.Is(err, ErrCorrupt) &&
				!errors.Is(err, errH265Profile) && !errors.Is(err, errH265Format) &&
				!errors.Is(err, errH265Size) && !errors.Is(err, errH265Tools) && !errors.Is(err, errVANoSurface) {
				t.Fatalf("iteration %d packet %d: %v", it, i, err)
			}
		}
		d.Flush()
	}
}

// FuzzH265Decoder is the parser and DPB fuzz entry point. It never uses a device.
func FuzzH265Decoder(f *testing.F) {
	f.Add([]byte{0, 0, 1, 0x40, 1, 0xff, 0xff})
	f.Add([]byte{0, 0, 1, 0x42, 1, 1, 1, 0x60, 0, 0, 0, 0x90, 0, 0})
	f.Add([]byte{0, 0, 1, 0x26, 1, 0xaf, 0x10, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := newH265Decoder(BackendInfo{"vaapi", H265, Decode, true})
		if len(data) > 0 {
			d.Send(Packet{Data: data})
			d.Send(Packet{Data: data})
		}
		d.Flush()
	})
}

func TestVAAPIH265EncoderStream(t *testing.T) {
	needVAAPI(t, H265, Encode)
	const w, h, frames, every, forced = 322 &^ 1, 242, 40, 15, 7
	cfg := VideoConfig{Codec: H265, Width: w, Height: h, Bitrate: 2_000_000, KeyframeEvery: every}
	enc, err := New(Options{Backend: "vaapi"}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var pkts []Packet
	for i := 0; i < frames; i++ {
		f := testFrame(w, h, i)
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
	since := 0
	var stream []byte
	for i, p := range pkts {
		stream = append(stream, p.Data...)
		wantKey := i == 0 || i == forced || since == every
		if wantKey {
			since = 0
		}
		since++
		if p.Keyframe != wantKey {
			t.Errorf("packet %d: keyframe %v, want %v", i, p.Keyframe, wantKey)
		}
		types := h265NalTypes(p.Data)
		if wantKey {
			if !bytes.Equal(types, []byte{h265NalVPS, h265NalSPS, h265NalPPS, h265NalIDRW}) {
				t.Errorf("packet %d: NAL types %v", i, types)
			}
		} else if !bytes.Equal(types, []byte{vaHEVCNalTrailR}) {
			t.Errorf("packet %d: NAL types %v", i, types)
		}
	}
	// FFmpeg must read the stream, at the right size and close to the source.
	dec := decodeFFmpeg(t, "hevc", "hevc", stream, w, h)
	if len(dec) != frames {
		t.Fatalf("FFmpeg decoded %d frames, want %d", len(dec), frames)
	}
	for i := 0; i < frames; i += 7 {
		ref := &VideoFrame{Width: w, Height: h, Y: dec[i], YStride: w}
		if q := psnrY(testFrame(w, h, i), ref); q < 28 {
			t.Errorf("frame %d: PSNR %.1f dB", i, q)
		}
	}
	// Each decoder must emit every frame as soon as it is sent, and be able to
	// join at a later keyframe.
	for _, di := range New(Options{}).Backends(H265, Decode) {
		t.Run(di.Name, func(t *testing.T) {
			d, err := New(Options{Backend: di.Name}).NewVideoDecoder(VideoConfig{Codec: H265})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			n := 0
			for i, p := range pkts[forced:] {
				if err := d.Send(p); err != nil {
					t.Fatal(err)
				}
				drain(t, d, func(f *VideoFrame) {
					if f.Width != w || f.Height != h {
						t.Errorf("frame is %dx%d", f.Width, f.Height)
					}
					if q := psnrY(testFrame(w, h, forced+n), f); q < 25 {
						t.Errorf("frame %d: PSNR %.1f dB", n, q)
					}
					n++
					f.Release()
				})
				if di.Name == "vaapi" && n != i+1 {
					t.Fatalf("after packet %d: %d frames out", i, n)
				}
			}
			if err := d.Flush(); err != nil {
				t.Fatal(err)
			}
			drain(t, d, func(f *VideoFrame) { n++; f.Release() })
			if n != frames-forced {
				t.Fatalf("decoded %d frames, want %d", n, frames-forced)
			}
		})
	}
}

func TestVAAPIH265EncoderBitrate(t *testing.T) {
	needVAAPI(t, H265, Encode)
	const fps, bitrate = 30, 1_000_000
	cfg := VideoConfig{Codec: H265, Width: 640, Height: 360, FPS: fps, Bitrate: bitrate}
	enc, err := New(Options{Backend: "vaapi"}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	f := NewVideoFrame(cfg.Width, cfg.Height, I420)
	total := 0
	for i := 0; i < 3*fps; i++ {
		for y := 0; y < f.Height; y++ {
			row := f.Y[y*f.YStride:]
			for x := 0; x < f.Width; x++ {
				row[x] = byte(64 + ((x+3*i)*(x+3*i)/7^(y+2*i)*(y+2*i)/5)&127)
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

// TestVAAPIH265Corrupt only feeds input the parser rejects before any submission.
func TestVAAPIH265Corrupt(t *testing.T) {
	needVAAPI(t, H265, Decode)
	needVAAPI(t, H265, Encode)
	cfg := VideoConfig{Codec: H265, Width: 320, Height: 240, KeyframeEvery: 10}
	pkts := encodeClip(t, "vaapi", cfg, 25)
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: H265})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	release := func(f *VideoFrame) { f.Release() }
	for _, junk := range [][]byte{{1, 2, 3}, {0, 0, 1, 0x40, 1, 0xff}, {0, 0, 1, 0x42, 1, 0}, {0, 0, 1, 0x44, 1, 0xff},
		{0, 0, 1, 0x26, 1, 0xff}, {0, 0, 1, 0x02, 1, 0x80, 0xff}} {
		err := dec.Send(Packet{Data: junk})
		if len(junk) == 3 || junk[3] != 0x40 {
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("Send(%x) = %v, want ErrCorrupt", junk, err)
			}
		}
		drain(t, dec, release)
	}
	// A P frame before any keyframe has nothing to reference.
	if err := dec.Send(pkts[1]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("P frame first: %v, want ErrCorrupt", err)
	}
	// Truncating into a header is rejected, and the stream stays usable.
	last := bytes.LastIndex(pkts[0].Data, []byte{0, 0, 1})
	if err := dec.Send(Packet{Data: pkts[0].Data[:last+5]}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated slice header: %v, want ErrCorrupt", err)
	}
	drain(t, dec, release)
	n := 0
	for _, p := range pkts[10:] {
		if err := dec.Send(p); err != nil {
			t.Fatal(err)
		}
		drain(t, dec, func(f *VideoFrame) { n++; f.Release() })
	}
	if n != 15 {
		t.Errorf("%d frames after recovery, want 15", n)
	}
}

func allocCase(t *testing.T, codec Codec, encName, decName string, w, h, runs int) {
	cfg := VideoConfig{Codec: codec, Width: w, Height: h}
	enc, err := New(Options{Backend: encName}).NewVideoEncoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	defer enc.Close()
	dec, err := New(Options{Backend: decName}).NewVideoDecoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	defer dec.Close()
	src := testFrame(w, h, 0)
	onFrame := func(f *VideoFrame) { f.Release() }
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
	allocs := testing.AllocsPerRun(runs, step)
	t.Logf("%.2f allocations per frame", allocs)
	if allocs != 0 {
		t.Errorf("%.2f allocations per frame", allocs)
	}
}

func TestVAAPIH265SteadyStateAllocs(t *testing.T) {
	needVAAPI(t, H265, Encode)
	t.Run("240p", func(t *testing.T) { allocCase(t, H265, "vaapi", "vaapi", 320, 240, 200) })
	t.Run("1080p", func(t *testing.T) { allocCase(t, H265, "vaapi", "vaapi", 1920, 1080, 50) })
}

// TestH265DecoderSteadyStateAllocs covers B-frames, which the encoder test does
// not, with a software-encoded clip decoded in a loop.
func TestVAAPIH265DecodeAllocs(t *testing.T) {
	needVAAPI(t, H265, Decode)
	for _, s := range [][2]int{{320, 240}, {1920, 1080}} {
		t.Run(strconv.Itoa(s[1])+"p", func(t *testing.T) {
			c := makeH265Clip(t, s[0], s[1], "bframes=3:ref=3:keyint=15")
			decodeLoopAllocs(t, H265, c.packets)
		})
	}
}

// decodeLoopAllocs measures allocations per packet while looping over a clip.
func decodeLoopAllocs(t *testing.T, codec Codec, pkts []Packet) {
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	onFrame := func(f *VideoFrame) { f.Release() }
	i := 0
	step := func() {
		if err := dec.Send(pkts[i%len(pkts)]); err != nil {
			t.Fatal(err)
		}
		i++
		drain(t, dec, onFrame)
	}
	for j := 0; j < 2*len(pkts); j++ {
		step()
	}
	allocs := testing.AllocsPerRun(3*len(pkts), step)
	t.Logf("%.2f allocations per packet", allocs)
	if allocs != 0 {
		t.Errorf("%.2f allocations per packet", allocs)
	}
}

// benchDecodeClip measures hardware decoding of a prepared 1080p clip.
func benchDecodeClip(b *testing.B, codec Codec, pkts []Packet) {
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: codec})
	if err != nil {
		b.Skip(err)
	}
	defer dec.Close()
	onFrame := func(f *VideoFrame) { f.Release() }
	i := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := dec.Send(pkts[i%len(pkts)]); err != nil {
			b.Fatal(err)
		}
		i++
		drain(b, dec, onFrame)
	}
}

func BenchmarkLatencyH265VAAPI(b *testing.B) {
	needVAAPI(b, H265, Encode)
	benchLatency(b, H265, "vaapi", "vaapi")
}

func BenchmarkH265VAAPIDecode1080p(b *testing.B) {
	needVAAPI(b, H265, Decode)
	benchDecodeClip(b, H265, makeH265Clip(b, 1920, 1080, "bframes=3:keyint=30").packets)
}
