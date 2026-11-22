package gaal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Every usable video backend, and every encoder and decoder pair, must pass
// these tests. Set GAAL_REQUIRE to a comma-separated list such as
// "h264/encode/nvenc,vp9/decode/libvpx" to fail if a backend is missing, so a
// CI machine cannot silently skip what it is meant to cover.

const (
	confW, confH = 640, 360
	confFrames   = 90
	confKeyEvery = 30
	confForced   = 13
	confDropped  = 20
	confStep     = 33 * time.Millisecond
	confWait     = 2 * time.Second
	confMinPSNR  = 24
)

var confCodecs = []Codec{H264, H265, VP8, VP9, JPEG}

func confNames(c Codec, d Direction) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range New(Options{}).Backends(c, d) {
		if !seen[b.Name] {
			seen[b.Name] = true
			out = append(out, b.Name)
		}
	}
	return out
}

func TestRequiredBackends(t *testing.T) {
	for _, want := range strings.Split(os.Getenv("GAAL_REQUIRE"), ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		found := false
		for _, c := range append(append([]Codec(nil), confCodecs...), Opus, AAC, MP3, FLAC, Vorbis, ALAC, AC3, PCMU, PCMA) {
			for _, d := range []Direction{Decode, Encode} {
				for _, n := range confNames(c, d) {
					found = found || want == fmt.Sprintf("%s/%s/%s", c, d, n)
				}
			}
		}
		if !found {
			t.Errorf("required backend %s is not available", want)
		}
	}
}

// confUnavailable handles a listed backend that cannot start, as happens on
// virtual machines without media hardware. A backend named in GAAL_REQUIRE must
// start, so CI cannot lose coverage this way.
func confUnavailable(t *testing.T, c Codec, d Direction, name string, err error) {
	t.Helper()
	id := fmt.Sprintf("%s/%s/%s", c, d, name)
	for _, want := range strings.Split(os.Getenv("GAAL_REQUIRE"), ",") {
		if strings.TrimSpace(want) == id {
			t.Fatalf("required backend cannot start: %v", err)
		}
	}
	t.Skipf("backend cannot start here: %v", err)
}

func confNALs(c Codec, b []byte) map[int]bool {
	out := map[int]bool{}
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 || pos+i+3 >= len(b) {
			return out
		}
		h := b[pos+i+3]
		if c == H264 {
			out[int(h&0x1F)] = true
		} else {
			out[int(h>>1&0x3F)] = true
		}
		pos += i + 3
	}
}

// confIsKey reports whether the bitstream itself contains a keyframe.
func confIsKey(c Codec, b []byte) bool {
	switch c {
	case H264:
		return confNALs(c, b)[5]
	case H265:
		n := confNALs(c, b)
		return n[16] || n[17] || n[18] || n[19] || n[20] || n[21]
	case VP8:
		return len(b) > 0 && b[0]&1 == 0
	case VP9:
		f, ok := parseVP9Frame(b)
		return ok && f.Key
	}
	return true
}

func confHasParams(c Codec, b []byte) bool {
	n := confNALs(c, b)
	switch c {
	case H264:
		return n[7] && n[8]
	case H265:
		return n[32] && n[33] && n[34]
	}
	return true
}

func confEncode(t *testing.T, name string, cfg VideoConfig, clip []*VideoFrame, n, force int) []Packet {
	t.Helper()
	enc, err := New(Options{Backend: name}).NewVideoEncoder(cfg)
	if err != nil {
		confUnavailable(t, cfg.Codec, Encode, name, err)
	}
	defer enc.Close()
	var out []Packet
	for i := 0; i < n; i++ {
		f := clip[i%len(clip)]
		f.PTS, f.ForceKeyframe, f.Range = time.Duration(i)*confStep, i == force, cfg.Range
		err := enc.Send(f)
		f.ForceKeyframe = false
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		p, err := ReceiveWait(enc, confWait)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		out = append(out, clonePacket(p.Packet))
		p.Release()
	}
	return out
}

type confResult struct {
	frames []confFrame
	maxLag int
}

type confFrame struct {
	index     int
	psnr      float64
	rng       ColorRange
	w, h      int
	duplicate bool
}

// confDecode feeds pkts, skipping index skip, one at a time.
// confDecode decodes pkts with one decoder. A hardware decoder that fails
// mid-stream goes on in software and loses pictures up to the next keyframe,
// so the clip is decoded again once from the start; a second failure counts.
func confDecode(t *testing.T, name string, c Codec, pkts []Packet, clip []*VideoFrame, skip int) confResult {
	t.Helper()
	r, fell := confDecodeOnce(t, name, c, pkts, clip, skip, false)
	if fell {
		r, _ = confDecodeOnce(t, name, c, pkts, clip, skip, true)
	}
	return r
}

func confDecodeOnce(t *testing.T, name string, c Codec, pkts []Packet, clip []*VideoFrame, skip int, last bool) (confResult, bool) {
	t.Helper()
	fell := false
	fallback := func(err error) bool {
		if !last && hwFallback(t, err) {
			fell = true
		}
		return fell
	}
	dec, err := New(Options{Backend: name}).NewVideoDecoder(VideoConfig{Codec: c})
	if err != nil {
		confUnavailable(t, c, Decode, name, err)
	}
	defer dec.Close()
	var r confResult
	seen := map[int]bool{}
	collect := func(f *VideoFrame) {
		i := int(f.PTS / confStep)
		cf := confFrame{index: i, rng: f.Range, w: f.Width, h: f.Height, duplicate: seen[i]}
		if f.Width == confW && f.Height == confH {
			cf.psnr = psnrY(clip[i%len(clip)], f)
		}
		seen[i] = true
		r.frames = append(r.frames, cf)
		f.Release()
	}
	sent := 0
	damaged := false
	for i, p := range pkts {
		if i == skip {
			damaged = true
			continue
		}
		sent++
		if confIsKey(c, p.Data) {
			damaged = false
		}
		if err := dec.Send(p); err != nil {
			if fallback(err) {
				return r, true
			}
			if damaged {
				continue // a decoder may refuse pictures with missing references
			}
			t.Fatalf("send %d: %v", i, err)
		}
		wait := 100 * time.Millisecond
		if damaged {
			wait = 10 * time.Millisecond
		}
		f, err := ReceiveWait(dec, wait)
		if fallback(err) {
			return r, true
		}
		if err == nil {
			collect(f)
			for {
				f, err := dec.Receive()
				if fallback(err) {
					return r, true
				}
				if err != nil {
					break
				}
				collect(f)
			}
		}
		if skip < 0 {
			r.maxLag = max(r.maxLag, sent-len(r.frames))
		}
	}
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := ReceiveWait(dec, confWait)
		if fallback(err) {
			return r, true
		}
		if err == io.EOF || err == ErrTimeout {
			return r, false
		}
		if err != nil {
			t.Fatal(err)
		}
		collect(f)
	}
}

// confRangeUnsupported lists encoders with no way to signal a range.
func confRangeUnsupported(c Codec, encoder string) bool {
	return c == VP8 || c == JPEG || encoder == "mediafoundation" || strings.HasPrefix(encoder, "mediacodec") && c == VP9
}

func TestConformance(t *testing.T) {
	clip := benchClip(confW, confH, 30)
	for _, c := range confCodecs {
		for _, e := range confNames(c, Encode) {
			t.Run(fmt.Sprintf("%s/%s", c, e), func(t *testing.T) {
				cfg := VideoConfig{Codec: c, Width: confW, Height: confH, FPS: 30, Bitrate: 2_000_000, KeyframeEvery: confKeyEvery, Quality: 85}
				pkts := confEncode(t, e, cfg, clip, confFrames, confForced)
				t.Run("encoder", func(t *testing.T) {
					if !pkts[0].Keyframe || !confIsKey(c, pkts[0].Data) {
						t.Error("the first packet is not a keyframe")
					}
					if !pkts[confForced].Keyframe || !confIsKey(c, pkts[confForced].Data) {
						t.Error("ForceKeyframe was ignored")
					}
					for i, p := range pkts {
						if want := time.Duration(i) * confStep; absDuration(p.PTS-want) > time.Millisecond {
							t.Errorf("packet %d has PTS %v, want %v", i, p.PTS, want)
							break
						}
					}
					for i, p := range pkts {
						real := confIsKey(c, p.Data)
						if real != p.Keyframe {
							t.Errorf("packet %d: Keyframe=%v but bitstream keyframe=%v", i, p.Keyframe, real)
						}
						if real && !confHasParams(c, p.Data) {
							t.Errorf("keyframe %d lacks parameter sets", i)
						}
					}
				})
				t.Run("set-bitrate", func(t *testing.T) {
					if c == JPEG {
						t.Skip("JPEG has no bitrate")
					}
					confSetBitrate(t, e, c, clip)
				})
				for _, want := range []ColorRange{RangeLimited, RangeFull} {
					t.Run("signals-"+want.String(), func(t *testing.T) {
						if confRangeUnsupported(c, e) {
							t.Skip("this encoder cannot signal a range")
						}
						rc := cfg
						rc.Range = want
						p := confEncode(t, e, rc, clip, 3, -1)
						if got := StreamRange(c, p[0].Data); got != want {
							t.Errorf("stream signals %s", got)
						}
					})
				}
				for _, d := range confNames(c, Decode) {
					t.Run("to-"+d, func(t *testing.T) {
						r := confDecode(t, d, c, pkts, clip, -1)
						if len(r.frames) != len(pkts) {
							t.Fatalf("decoded %d of %d frames", len(r.frames), len(pkts))
						}
						// OpenH264 can hold one picture of streams it did not encode.
						if allowed := map[string]int{"openh264": 1}[d]; r.maxLag > allowed {
							t.Errorf("decoder held %d packets", r.maxLag)
						}
						want := StreamRange(c, pkts[0].Data)
						for i, f := range r.frames {
							if f.index != i || f.duplicate {
								t.Fatalf("output %d carries PTS of packet %d", i, f.index)
							}
							if f.w != confW || f.h != confH {
								t.Fatalf("frame %d is %dx%d", i, f.w, f.h)
							}
							if f.psnr < confMinPSNR {
								t.Errorf("frame %d PSNR %.1f dB", i, f.psnr)
							}
							if f.rng != want {
								t.Errorf("frame %d reports range %s, stream says %s", i, f.rng, want)
								break
							}
						}
						if !confRangeUnsupported(c, e) {
							full := cfg
							full.Range = RangeFull
							fp := confEncode(t, e, full, clip, 10, -1)
							for _, f := range confDecode(t, d, c, fp, clip, -1).frames {
								if f.rng != RangeFull {
									t.Errorf("full-range frame %d reports %s", f.index, f.rng)
								}
								if f.psnr < confMinPSNR {
									t.Errorf("full-range frame %d was changed, PSNR %.1f dB", f.index, f.psnr)
								}
							}
						}
						confLowDelay(t, d, c, pkts)
						if c == JPEG {
							return
						}
						next := -1
						for i := confDropped + 1; i < len(pkts); i++ {
							if confIsKey(c, pkts[i].Data) {
								next = i
								break
							}
						}
						if next < 0 {
							t.Fatal("no keyframe after the dropped packet")
						}
						after := 0
						for _, f := range confDecode(t, d, c, pkts, clip, confDropped).frames {
							if f.index >= next {
								after++
								if f.psnr < confMinPSNR {
									t.Errorf("frame %d after recovery has PSNR %.1f dB", f.index, f.psnr)
								}
							}
						}
						if after != len(pkts)-next {
							t.Errorf("recovered %d of %d frames after the keyframe", after, len(pkts)-next)
						}
					})
				}
			})
		}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// allocBudget is the allowed allocations per encode and decode round trip. The
// pure Go JPEG codec is the only path that allocates by design. Raising a value
// here is a deliberate regression and must be justified in review.
func allocBudget(enc, dec string) float64 {
	budget := 0.0
	if enc == "go-image-jpeg" {
		budget += 12
	}
	if dec == "go-image-jpeg" {
		budget += 45
	}
	return budget
}

// TestAllocationBudget covers every encoder and decoder pair of every codec.
func TestAllocationBudget(t *testing.T) {
	const w, h = 320, 240
	clip := benchClip(w, h, 8)
	for _, c := range confCodecs {
		for _, e := range confNames(c, Encode) {
			for _, d := range confNames(c, Decode) {
				t.Run(fmt.Sprintf("%s/%s/%s", c, e, d), func(t *testing.T) {
					cfg := VideoConfig{Codec: c, Width: w, Height: h, FPS: 30, Bitrate: 1_000_000, KeyframeEvery: 60}
					enc, err := New(Options{Backend: e}).NewVideoEncoder(cfg)
					if err != nil {
						confUnavailable(t, c, Encode, e, err)
					}
					defer enc.Close()
					dec, err := New(Options{Backend: d}).NewVideoDecoder(cfg)
					if err != nil {
						confUnavailable(t, c, Decode, d, err)
					}
					defer dec.Close()
					fb := hwFallbacks
					n := 0
					step := func() {
						f := clip[n%len(clip)]
						f.PTS = time.Duration(n) * confStep
						n++
						if err := enc.Send(f); err != nil {
							t.Fatal(err)
						}
						p, err := ReceiveWait(enc, confWait)
						if err != nil {
							t.Fatal(err)
						}
						if err := dec.Send(p.Packet); err != nil && !hwFallback(t, err) {
							t.Fatal(err)
						}
						p.Release()
						wait := confWait
						if d == "openh264" {
							wait = 5 * time.Millisecond // it can hold one picture
						}
						out, err := ReceiveWait(dec, wait)
						// After a fallback, packets before the next keyframe give no picture.
						if (err == ErrTimeout && (d == "openh264" || hwFallbacks != fb)) || hwFallback(t, err) {
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						out.Release()
					}
					for i := 0; i < 40; i++ {
						step()
					}
					got := allocsPerRun(100, step)
					if budget := allocBudget(e, d); got > budget {
						t.Errorf("%.2f allocations per frame, budget %.0f", got, budget)
					}
				})
			}
		}
	}
}

// confSetBitrate encodes at a high bitrate, lowers it to a quarter of the
// bitrate produced while running, and checks that the stream gets smaller without a new keyframe.
func confSetBitrate(t *testing.T, name string, c Codec, clip []*VideoFrame) {
	const half, settle = 60, 20
	cfg := VideoConfig{Codec: c, Width: confW, Height: confH, FPS: 30, Bitrate: 2_000_000, KeyframeEvery: 1000}
	enc, err := New(Options{Backend: name}).NewVideoEncoder(cfg)
	if err != nil {
		confUnavailable(t, c, Encode, name, err)
	}
	defer enc.Close()
	var bytes [2]int
	for i := 0; i < 2*half; i++ {
		if i == half {
			// A quarter of what the encoder really produced, because simple
			// content leaves some encoders well under their target.
			produced := bytes[0] * 8 * cfg.FPS / (half - settle)
			err := enc.(BitrateSetter).SetBitrate(max(produced/4, 50_000))
			if errors.Is(err, errors.ErrUnsupported) {
				t.Skipf("%v", err)
			}
			if err != nil {
				t.Fatalf("SetBitrate: %v", err)
			}
		}
		// Back and forth, because a jump back to the first frame is a scene
		// cut that some encoders answer with a keyframe.
		k := i % (2*len(clip) - 2)
		if k >= len(clip) {
			k = 2*len(clip) - 2 - k
		}
		f := clip[k]
		f.PTS, f.ForceKeyframe, f.Range = time.Duration(i)*confStep, false, RangeUnspecified
		if err := enc.Send(f); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		p, err := ReceiveWait(enc, confWait)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if i > 0 && (p.Keyframe || confIsKey(c, p.Data)) {
			t.Errorf("packet %d is a keyframe", i)
		}
		// Rate control needs some frames to settle after the start and the change.
		if i%half >= settle {
			bytes[i/half] += len(p.Data)
		}
		p.Release()
	}
	ratio := float64(bytes[1]) / float64(bytes[0])
	t.Logf("%s: %d bytes before, %d after, ratio %.2f", name, bytes[0], bytes[1], ratio)
	if ratio > 0.7 {
		t.Errorf("stream shrank only to %.0f%% after the bitrate fell to a quarter", ratio*100)
	}
}
