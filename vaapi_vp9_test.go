//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// readIVF splits an IVF file into packets.
func readIVF(t testing.TB, path string) []Packet {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 32 || string(b[:4]) != "DKIF" {
		t.Fatal("not an IVF file")
	}
	b = b[binary.LittleEndian.Uint16(b[6:]):]
	var out []Packet
	for len(b) >= 12 {
		n := int(binary.LittleEndian.Uint32(b))
		pts := binary.LittleEndian.Uint64(b[4:])
		if n > len(b)-12 {
			t.Fatal("truncated IVF frame")
		}
		out = append(out, Packet{Data: b[12 : 12+n], PTS: time.Duration(pts) * time.Second / 30})
		b = b[12+n:]
	}
	return out
}

// makeVP9Clip encodes with libvpx-vp9 and decodes with FFmpeg's software decoder.
func makeVP9Clip(t testing.TB, w, h int, args ...string) *refClip {
	t.Helper()
	needEncoder(t, "libvpx-vp9")
	dir := t.TempDir()
	ivf, yuv := filepath.Join(dir, "a.ivf"), filepath.Join(dir, "a.yuv")
	cmd := []string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30:d=2", w, h),
		"-pix_fmt", "yuv420p", "-c:v", "libvpx-vp9", "-b:v", "1M", "-threads", "8"}
	if len(args) > 0 && args[0] == "2pass" {
		args = args[1:]
		log := filepath.Join(dir, "pass")
		cmd = append(cmd, "-passlogfile", log)
		runTool(t, "ffmpeg", append(append(append([]string{}, cmd...), args...), "-pass", "1", "-f", "null", os.DevNull)...)
		cmd = append(cmd, "-pass", "2")
	}
	runTool(t, "ffmpeg", append(append(cmd, args...), "-f", "ivf", ivf)...)
	runTool(t, "ffmpeg", "-v", "error", "-c:v", "vp9", "-i", ivf, "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", yuv)
	return &refClip{w: w, h: h, packets: readIVF(t, ivf), frames: readFrames(t, yuv, w, h)}
}

// vp9Stats counts what a stream contains, from the parser's point of view.
type vp9Stats struct {
	frames, hidden, superframes, showExisting, shown, keys, intraOnly, seg, tiled int
}

// dryDecodeVP9 runs a clip through all parsing and state tracking without a
// device, and reports what it saw.
func dryDecodeVP9(t testing.TB, c *refClip) vp9Stats {
	t.Helper()
	d := newVP9Decoder(BackendInfo{"vaapi", VP9, Decode, true})
	var st vp9Stats
	d.dryEmit = func(int, int, time.Duration) { st.shown++ }
	for i, p := range c.packets {
		if err := d.Send(p); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		st.collect(p.Data)
	}
	if st.shown != len(c.frames) {
		t.Fatalf("%d frames shown, want %d", st.shown, len(c.frames))
	}
	return st
}

func (st *vp9Stats) collect(b []byte) {
	m := b[len(b)-1]
	frames := [][]byte{b}
	if m&0xe0 == 0xc0 {
		n, mag := int(m&7)+1, int(m>>3&3)+1
		if sz := 2 + mag*n; len(b) >= sz && b[len(b)-sz] == m {
			st.superframes++
			frames = frames[:0]
			idx, at := b[len(b)-sz+1:], 0
			for i := 0; i < n; i++ {
				s := 0
				for j := 0; j < mag; j++ {
					s |= int(idx[i*mag+j]) << (8 * j)
				}
				frames = append(frames, b[at:at+s])
				at += s
			}
		}
	}
	// Reference slots do not matter for these flags, so all are marked valid.
	var slots [vp9NumSlots]vp9Slot
	for i := range slots {
		slots[i] = vp9Slot{valid: true, w: 64, h: 64}
	}
	var prev vp9State
	prev.reset()
	for _, f := range frames {
		var h vp9Header
		if parseVP9Header(f, &h, &prev, &slots) != nil {
			continue
		}
		st.frames++
		switch {
		case h.showExisting:
			st.showExisting++
		case !h.show:
			st.hidden++
		}
		if h.key {
			st.keys++
		}
		if h.intraOnly {
			st.intraOnly++
		}
		if h.segOn {
			st.seg++
		}
		if h.tileColsLog2+h.tileRowsLog2 > 0 {
			st.tiled++
		}
	}
}

var vp9Cases = []struct {
	name string
	w, h int
	args []string
	want func(*vp9Stats) bool
}{
	{"altref", 640, 360, []string{"2pass", "-deadline", "good", "-cpu-used", "4", "-auto-alt-ref", "1", "-lag-in-frames", "25"},
		func(s *vp9Stats) bool { return s.hidden > 0 && s.superframes > 0 }},
	{"multi-altref", 640, 360, []string{"2pass", "-deadline", "good", "-cpu-used", "2", "-auto-alt-ref", "6", "-lag-in-frames", "25"},
		func(s *vp9Stats) bool { return s.hidden > 0 }},
	{"realtime", 640, 360, []string{"-deadline", "realtime", "-cpu-used", "8", "-lag-in-frames", "0"}, nil},
	{"tiles", 1280, 720, []string{"-deadline", "realtime", "-cpu-used", "8", "-tile-columns", "2", "-tile-rows", "1", "-threads", "8"},
		func(s *vp9Stats) bool { return s.tiled > 0 }},
	{"cyclic-aq", 640, 360, []string{"-deadline", "realtime", "-cpu-used", "7", "-aq-mode", "3", "-lag-in-frames", "0"},
		func(s *vp9Stats) bool { return s.seg > 0 }},
	{"variance-aq", 640, 360, []string{"-deadline", "good", "-cpu-used", "5", "-aq-mode", "1", "-lag-in-frames", "10"},
		func(s *vp9Stats) bool { return s.seg > 0 }},
	{"error-resilient", 640, 360, []string{"-deadline", "realtime", "-cpu-used", "8", "-error-resilient", "1", "-lag-in-frames", "0"}, nil},
	{"frame-parallel", 640, 360, []string{"-deadline", "good", "-cpu-used", "5", "-frame-parallel", "1"}, nil},
	{"short-gop", 640, 360, []string{"-deadline", "realtime", "-cpu-used", "8", "-g", "7", "-lag-in-frames", "0"},
		func(s *vp9Stats) bool { return s.keys > 3 }},
	{"lossless", 320, 240, []string{"-deadline", "good", "-cpu-used", "8", "-lossless", "1"}, nil},
	{"odd-size", 642, 362, []string{"-deadline", "realtime", "-cpu-used", "8", "-lag-in-frames", "0"}, nil},
}

// TestVP9ParserClips checks parsing and frame accounting without the GPU.
func TestVP9ParserClips(t *testing.T) {
	for _, tc := range vp9Cases {
		t.Run(tc.name, func(t *testing.T) {
			c := makeVP9Clip(t, tc.w, tc.h, tc.args...)
			st := dryDecodeVP9(t, c)
			t.Logf("%+v", st)
			if tc.want != nil && !tc.want(&st) {
				t.Errorf("stream does not exercise the case: %+v", st)
			}
		})
	}
}

// TestVAAPIVP9Conformance compares against FFmpeg's software decoder.
func TestVAAPIVP9Conformance(t *testing.T) {
	needVAAPI(t, VP9, Decode)
	for _, tc := range vp9Cases {
		t.Run(tc.name, func(t *testing.T) {
			c := makeVP9Clip(t, tc.w, tc.h, tc.args...)
			st := dryDecodeVP9(t, c) // never send what the parser would not accept
			delay := checkDecoder(t, VP9, c)
			t.Logf("%+v, delay %d", st, delay)
			if delay != 0 {
				t.Errorf("output delay %d packets", delay)
			}
		})
	}
}

// TestVAAPIVP9ResolutionChange changes size at keyframes.
func TestVAAPIVP9ResolutionChange(t *testing.T) {
	needVAAPI(t, VP9, Decode)
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: VP9})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	sizes := [][2]int{{640, 360}, {320, 240}, {424, 240}}
	type want struct {
		w, h int
		data []byte
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
		compareFrame(t, n, f, w.data)
		n++
	}
	for _, s := range sizes {
		c := makeVP9Clip(t, s[0], s[1], "-deadline", "realtime", "-cpu-used", "8", "-lag-in-frames", "0")
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
	if n != len(wants) {
		t.Fatalf("decoded %d frames, want %d", n, len(wants))
	}
}

// TestVP9ParserMutation feeds damaged frames to the parser and state tracking
// without a device. They must fail cleanly, never panic.
func TestVP9ParserMutation(t *testing.T) {
	c := makeVP9Clip(t, 320, 240, "-deadline", "good", "-cpu-used", "5", "-aq-mode", "1", "-auto-alt-ref", "1", "-lag-in-frames", "10", "-g", "12")
	rng := rand.New(rand.NewSource(2))
	iters := 500
	if testing.Short() {
		iters = 50
	}
	for it := 0; it < iters; it++ {
		d := newVP9Decoder(BackendInfo{"vaapi", VP9, Decode, true})
		for i, p := range c.packets {
			if i > 20 {
				break
			}
			b := bytes.Clone(p.Data)
			if rng.Intn(3) != 0 {
				for j := 0; j < 1+rng.Intn(5); j++ {
					at := rng.Intn(len(b))
					if rng.Intn(2) == 0 {
						at = rng.Intn(min(len(b), 24))
					} else if rng.Intn(4) == 0 {
						at = len(b) - 1 - rng.Intn(min(len(b), 8)) // superframe index
					}
					switch rng.Intn(3) {
					case 0:
						b[at] ^= 1 << rng.Intn(8)
					case 1:
						b[at] = byte(rng.Intn(256))
					case 2:
						b = b[:at+1]
					}
				}
			}
			err := d.Send(Packet{Data: b})
			if err != nil && !errors.Is(err, ErrCorrupt) && !errors.Is(err, errVP9Profile) &&
				!errors.Is(err, errVP9Size) && !errors.Is(err, errVP9Scaling) && !errors.Is(err, errVP9Headers) {
				t.Fatalf("iteration %d packet %d: %v", it, i, err)
			}
		}
	}
}

// FuzzVP9Decoder is the parser fuzz entry point. It never uses a device.
func FuzzVP9Decoder(f *testing.F) {
	f.Add([]byte{0x82, 0x49, 0x83, 0x42, 0, 0, 0, 0, 0})
	f.Add([]byte{0x88})
	f.Add([]byte{0x86, 0, 0, 0xc1, 1, 2, 0xc1})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := newVP9Decoder(BackendInfo{"vaapi", VP9, Decode, true})
		if len(data) > 0 {
			d.Send(Packet{Data: data})
			d.Send(Packet{Data: data})
		}
	})
}

func TestVP9RejectsUnsupported(t *testing.T) {
	d := newVP9Decoder(BackendInfo{"vaapi", VP9, Decode, true})
	// frame_marker 2, profile 1.
	if err := d.Send(Packet{Data: []byte{0xa2, 0, 0, 0, 0, 0}}); err == nil || errors.Is(err, ErrCorrupt) {
		t.Errorf("profile 1: %v, want a plain error", err)
	}
	if err := d.Send(Packet{Data: []byte{0x00, 0, 0}}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad marker: %v, want ErrCorrupt", err)
	}
	if err := d.Send(Packet{Data: []byte{0x88}}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("show_existing of an empty slot: %v, want ErrCorrupt", err)
	}
}

// TestVAAPIVP9Corrupt only feeds input the parser rejects before any submission.
func TestVAAPIVP9Corrupt(t *testing.T) {
	needVAAPI(t, VP9, Decode)
	c := makeVP9Clip(t, 320, 240, "-deadline", "realtime", "-cpu-used", "8", "-lag-in-frames", "0", "-g", "10")
	dec, err := New(Options{Backend: "vaapi"}).NewVideoDecoder(VideoConfig{Codec: VP9})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	release := func(f *VideoFrame) { f.Release() }
	if err := dec.Send(c.packets[1]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("inter frame first: %v, want ErrCorrupt", err)
	}
	// A frame cut inside its tile data is refused before it reaches the driver.
	key := c.packets[0].Data
	var h vp9Header
	var st vp9State
	st.reset()
	var slots [vp9NumSlots]vp9Slot
	if err := parseVP9Header(key, &h, &st, &slots); err != nil {
		t.Fatal(err)
	}
	if err := dec.Send(Packet{Data: key[:h.hdrSize+h.compSize]}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated frame: %v, want ErrCorrupt", err)
	}
	drain(t, dec, release)
	n := 0
	for _, p := range c.packets {
		if err := dec.Send(p); err != nil {
			t.Fatal(err)
		}
		drain(t, dec, func(f *VideoFrame) { n++; f.Release() })
	}
	if n != len(c.frames) {
		t.Errorf("%d frames after recovery, want %d", n, len(c.frames))
	}
}

func TestVAAPIVP9DecodeAllocs(t *testing.T) {
	needVAAPI(t, VP9, Decode)
	for _, s := range [][2]int{{320, 240}, {1920, 1080}} {
		t.Run(strconv.Itoa(s[1])+"p", func(t *testing.T) {
			c := makeVP9Clip(t, s[0], s[1], "-deadline", "realtime", "-cpu-used", "8", "-g", "15")
			decodeLoopAllocs(t, VP9, c.packets)
		})
	}
}

func BenchmarkVP9VAAPIDecode1080p(b *testing.B) {
	needVAAPI(b, VP9, Decode)
	benchDecodeClip(b, VP9, makeVP9Clip(b, 1920, 1080, "-deadline", "realtime", "-cpu-used", "8", "-g", "30").packets)
}

func BenchmarkLatencyVP9LibvpxVAAPI(b *testing.B) {
	needVAAPI(b, VP9, Decode)
	benchLatency(b, VP9, "libvpx", "vaapi")
}
