//go:build darwin && arm64

package gaal

import (
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

const vtName = "videotoolbox"

func vtLib() *Library { return New(Options{Backend: vtName}) }

func vtH265Encoder(t testing.TB, cfg VideoConfig) VideoEncoder {
	t.Helper()
	cfg.Codec = H265
	e, err := vtLib().NewVideoEncoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	return e
}

func vtDecoderFor(t testing.TB, c Codec) VideoDecoder {
	t.Helper()
	d, err := vtLib().NewVideoDecoder(VideoConfig{Codec: c})
	if err != nil {
		t.Skip(err)
	}
	return d
}

// h265Types lists the NAL unit types of an Annex B access unit.
func h265Types(b []byte) []int {
	var out []int
	for pos := 0; ; {
		s, e, ok := nextNAL(b, pos)
		if !ok {
			return out
		}
		pos = e
		if e-s >= 2 {
			out = append(out, int(b[s]>>1&0x3F))
		}
	}
}

// vtBoolProperty reads a boolean property of a session.
func vtBoolProperty(t *testing.T, session uintptr, name string) bool {
	t.Helper()
	const path = "/System/Library/Frameworks/VideoToolbox.framework/VideoToolbox"
	h, err := dl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	copyProp, err := dl.Sym(h, "VTSessionCopyProperty")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := loadApple()
	var key uintptr
	if err := constant(h, name, &key); err != nil {
		t.Fatal(err)
	}
	var c dl.Caller
	out := new(uintptr)
	if st := int32(c.C5(copyProp, session, key, 0, uintptr(unsafe.Pointer(out)), 0)); st != 0 {
		t.Fatalf("copy %s: status %d", name, st)
	}
	defer a.cfRelease(*out)
	return *out == a.kTrue
}

func TestHEVCReorderDepth(t *testing.T) {
	au := vtClip(vtTestH265)[0]
	for pos := 0; ; {
		s, e, ok := nextNAL(au, pos)
		if !ok {
			t.Fatal("clip has no SPS")
		}
		pos = e
		if au[s]>>1&0x3F == 33 {
			if got := hevcReorderDepth(au[s:e]); got == 0 || got > 4 {
				t.Errorf("x265 clip with B-frames has reorder depth %d", got)
			}
			if got := hevcReorderDepth(au[s : s+6]); got != 0 {
				t.Errorf("truncated SPS gave %d", got)
			}
			return
		}
	}
}

func vtEncoderOf(e VideoEncoder) *vtEncoder {
	return e.(*failover[*VideoFrame, *EncodedPacket]).cur.(*vtEncoder)
}

func vtDecoderOf(d VideoDecoder) *vtDecoder {
	return d.(*colorDecoder).VideoDecoder.(*failover[Packet, *VideoFrame]).cur.(*vtDecoder)
}

// TestVTH265Stream checks the bitstream layout, key frames, order and pictures.
func TestVTH265Stream(t *testing.T) {
	const w, h, frames, forced = 1920, 1080, 40, 17
	cfg := VideoConfig{Width: w, Height: h, Bitrate: 12_000_000, KeyframeEvery: 1000}
	e := vtH265Encoder(t, cfg)
	defer e.Close()
	if !e.Backend().Hardware {
		t.Error("encoder is not reported as hardware")
	}
	if !vtBoolProperty(t, vtEncoderOf(e).session, "kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder") {
		t.Error("session does not use the hardware encoder")
	}
	d := vtDecoderFor(t, H265)
	defer d.Close()

	var pkts []Packet
	keep := func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() }
	src := make([]*VideoFrame, frames)
	for i := range src {
		src[i] = testFrame(w, h, i)
		src[i].ForceKeyframe = i == forced
		if err := e.Send(src[i]); err != nil {
			t.Fatal(err)
		}
		drain(t, e, keep)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, e, keep)
	if len(pkts) != frames {
		t.Fatalf("got %d packets, want %d", len(pkts), frames)
	}
	for i, p := range pkts {
		if p.PTS != src[i].PTS {
			t.Errorf("packet %d: pts %v, want %v", i, p.PTS, src[i].PTS)
		}
		if want := i == 0 || i == forced; p.Keyframe != want {
			t.Errorf("packet %d: keyframe %v, want %v", i, p.Keyframe, want)
		}
		if string(p.Data[:4]) != string(annexBStart[:]) {
			t.Fatalf("packet %d is not Annex B: %x", i, p.Data[:8])
		}
		types := h265Types(p.Data)
		if p.Keyframe {
			if len(types) < 4 || !slices.Equal(types[:3], []int{32, 33, 34}) {
				t.Errorf("keyframe %d has NAL types %v", i, types)
			}
			if !slices.ContainsFunc(types, func(n int) bool { return n >= 16 && n <= 21 }) {
				t.Errorf("keyframe %d has no IRAP picture: %v", i, types)
			}
		} else if slices.ContainsFunc(types, func(n int) bool { return n >= 32 && n <= 34 }) {
			t.Errorf("packet %d repeats parameter sets: %v", i, types)
		}
	}

	got := 0
	check := func(f *VideoFrame) {
		if f.Width != w || f.Height != h || f.Format != NV12 {
			t.Errorf("frame %d: %dx%d format %d", got, f.Width, f.Height, f.Format)
		}
		if f.PTS != src[got].PTS {
			t.Errorf("frame %d: pts %v, want %v", got, f.PTS, src[got].PTS)
		}
		if got%10 == 0 {
			if q, qc := psnrY(src[got], f), psnrChroma(src[got], f); q < 32 || qc < 32 {
				t.Errorf("frame %d: PSNR luma %.1f chroma %.1f", got, q, qc)
			}
		}
		got++
		f.Release()
	}
	for _, p := range pkts {
		if err := d.Send(p); err != nil {
			t.Fatal(err)
		}
		drain(t, d, check)
	}
	if !vtBoolProperty(t, vtDecoderOf(d).session, "kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder") {
		t.Error("session does not use the hardware decoder")
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, check)
	if got != frames {
		t.Errorf("decoded %d of %d frames", got, frames)
	}
}

// The decoder must start at any keyframe, as a late joiner does.
func TestVTH265JoinAtKeyframe(t *testing.T) {
	const w, h = 320, 240
	e := vtH265Encoder(t, VideoConfig{Width: w, Height: h, KeyframeEvery: 10})
	defer e.Close()
	var pkts []Packet
	keep := func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() }
	for i := 0; i < 15; i++ {
		if err := e.Send(testFrame(w, h, i)); err != nil {
			t.Fatal(err)
		}
		drain(t, e, keep)
	}
	if !pkts[10].Keyframe {
		t.Fatal("frame 10 is not a keyframe")
	}
	d := vtDecoderFor(t, H265)
	defer d.Close()
	if err := d.Send(pkts[1]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("picture before parameter sets: %v", err)
	}
	n := 0
	for _, p := range pkts[10:] {
		if err := d.Send(p); err != nil {
			t.Fatal(err)
		}
		drain(t, d, func(f *VideoFrame) { n++; f.Release() })
	}
	if n != 5 {
		t.Errorf("decoded %d of 5 frames", n)
	}
}

// A new resolution arrives as new parameter sets on the same decoder.
func TestVTH265ResolutionChange(t *testing.T) {
	d := vtDecoderFor(t, H265)
	defer d.Close()
	var sizes [][2]int
	note := func(f *VideoFrame) { sizes = append(sizes, [2]int{f.Width, f.Height}); f.Release() }
	for _, s := range [][2]int{{320, 240}, {640, 360}} {
		e := vtH265Encoder(t, VideoConfig{Width: s[0], Height: s[1]})
		for i := 0; i < 3; i++ {
			if err := e.Send(testFrame(s[0], s[1], i)); err != nil {
				t.Fatal(err)
			}
			drain(t, e, func(p *EncodedPacket) {
				if err := d.Send(p.Packet); err != nil {
					t.Fatal(err)
				}
				p.Release()
				drain(t, d, note)
			})
		}
		e.Close()
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, note)
	if len(sizes) != 6 || sizes[0] != [2]int{320, 240} || sizes[5] != [2]int{640, 360} {
		t.Errorf("sizes %v", sizes)
	}
}

func TestVTH265Corrupt(t *testing.T) {
	d := vtDecoderFor(t, H265)
	defer d.Close()
	if err := d.Send(Packet{Data: []byte("this is certainly not video")}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: %v", err)
	}
	bad := []byte{0, 0, 0, 1, 32 << 1, 1, 9, 0, 0, 0, 1, 33 << 1, 1, 9, 0, 0, 0, 1, 34 << 1, 1, 9, 0, 0, 0, 1, 19 << 1, 1, 9}
	if err := d.Send(Packet{Data: bad}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad parameter sets: %v", err)
	}
}

func vtSteadyAllocs(t *testing.T, enc VideoEncoder, dec VideoDecoder, w, h int) {
	src := testFrame(w, h, 0)
	out := 0
	onFrame := func(f *VideoFrame) { out++; f.Release() }
	onPacket := func(p *EncodedPacket) {
		if err := dec.Send(p.Packet); err != nil && !hwFallback(t, err) {
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
	a := allocsPerRun(200, step)
	t.Logf("%dx%d: %.2f allocations per encoded and decoded frame", w, h, a)
	if a != 0 {
		t.Errorf("%.2f allocations per frame", a)
	}
}

func TestVTH265Allocs(t *testing.T) {
	for _, s := range [][2]int{{320, 240}, {1920, 1080}} {
		e := vtH265Encoder(t, VideoConfig{Width: s[0], Height: s[1], FPS: 60, Bitrate: 8_000_000})
		d := vtDecoderFor(t, H265)
		vtSteadyAllocs(t, e, d, s[0], s[1])
		e.Close()
		d.Close()
	}
}

// vtLatency measures one frame at a time through the codecs, on changing
// pictures, and logs the mean and worst of each stage.
func vtLatency(t *testing.T, name string, enc VideoEncoder, dec VideoDecoder, w, h int) {
	src := make([]*VideoFrame, 16)
	for i := range src {
		src[i] = testFrame(w, h, i*3)
	}
	const frames, warmup = 600, 30
	var eSum, dSum, eMax, dMax time.Duration
	size, measured := 0, 0
	// After a hardware decoder fails and recovers, it waits for a keyframe.
	// One is forced, and the pictures until then are not measured.
	recovering := false
	for i := 0; i < frames+warmup; i++ {
		f := src[i%len(src)]
		f.PTS = time.Duration(i) * time.Second / 60
		f.ForceKeyframe = recovering
		start := time.Now()
		if err := enc.Send(f); err != nil {
			t.Fatal(err)
		}
		p, err := ReceiveWait(enc, time.Second)
		if err != nil {
			t.Fatal("encoder: ", err)
		}
		mid := time.Now()
		size += len(p.Data)
		if err := dec.Send(p.Packet); err != nil {
			if !hwFallback(t, err) {
				t.Fatal(err)
			}
			recovering = true
		}
		p.Release()
		out, err := ReceiveWait(dec, time.Second)
		if hwFallback(t, err) || (recovering && err == ErrTimeout) {
			recovering = true
			continue
		}
		if err != nil {
			t.Fatal("decoder: ", err)
		}
		out.Release()
		end := time.Now()
		if i >= warmup && !recovering {
			ed, dd := mid.Sub(start), end.Sub(mid)
			eSum, dSum, eMax, dMax = eSum+ed, dSum+dd, max(eMax, ed), max(dMax, dd)
			measured++
		}
		recovering = false
	}
	if measured == 0 {
		t.Fatal("no picture was measured")
	}
	n := time.Duration(measured)
	t.Logf("%s %dx%d: encode mean %v max %v, decode mean %v max %v, %d bytes/frame", name, w, h,
		(eSum / n).Round(time.Microsecond), eMax.Round(time.Microsecond),
		(dSum / n).Round(time.Microsecond), dMax.Round(time.Microsecond), size/(frames+warmup))
}

func TestVTH265Latency1080p(t *testing.T) {
	const w, h = 1920, 1080
	e := vtH265Encoder(t, VideoConfig{Width: w, Height: h, FPS: 60, Bitrate: 8_000_000})
	defer e.Close()
	d := vtDecoderFor(t, H265)
	defer d.Close()
	vtLatency(t, "h265", e, d, w, h)
}

// --- VP9 ---

func vp9Encoder(t testing.TB, w, h int) VideoEncoder {
	t.Helper()
	e, err := New(Options{Backend: "libvpx"}).NewVideoEncoder(VideoConfig{Codec: VP9, Width: w, Height: h, Bitrate: 4_000_000})
	if err != nil {
		t.Skip(err)
	}
	return e
}

func TestVTVP9Decode(t *testing.T) {
	const w, h, frames = 640, 360, 30
	e := vp9Encoder(t, w, h)
	defer e.Close()
	d := vtDecoderFor(t, VP9)
	defer d.Close()
	if !d.Backend().Hardware {
		t.Error("decoder is not reported as hardware")
	}
	got := 0
	var sent []time.Duration // libvpx rounds timestamps to its time base
	check := func(f *VideoFrame) {
		src := testFrame(w, h, got)
		if f.Width != w || f.Height != h || f.PTS != sent[got] {
			t.Errorf("frame %d: %dx%d pts %v, want %v", got, f.Width, f.Height, f.PTS, sent[got])
		}
		if q, qc := psnrY(src, f), psnrChroma(src, f); q < 30 || qc < 30 {
			t.Errorf("frame %d: PSNR luma %.1f chroma %.1f", got, q, qc)
		}
		got++
		f.Release()
	}
	onPacket := func(p *EncodedPacket) {
		sent = append(sent, p.PTS)
		if err := d.Send(p.Packet); err != nil {
			t.Fatal(err)
		}
		p.Release()
		drain(t, d, check)
	}
	for i := 0; i < frames; i++ {
		if err := e.Send(testFrame(w, h, i)); err != nil {
			t.Fatal(err)
		}
		drain(t, e, onPacket)
	}
	if !vtBoolProperty(t, vtDecoderOf(d).session, "kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder") {
		t.Error("session does not use the hardware decoder")
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, e, onPacket)
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, check)
	if got != frames {
		t.Errorf("decoded %d of %d frames", got, frames)
	}
}

func TestVTVP9Allocs(t *testing.T) {
	for _, s := range [][2]int{{320, 240}, {1920, 1080}} {
		e := vp9Encoder(t, s[0], s[1])
		d := vtDecoderFor(t, VP9)
		vtSteadyAllocs(t, e, d, s[0], s[1])
		e.Close()
		d.Close()
	}
}

func TestVTVP9Latency1080p(t *testing.T) {
	const w, h = 1920, 1080
	e := vp9Encoder(t, w, h)
	defer e.Close()
	d := vtDecoderFor(t, VP9)
	defer d.Close()
	vtLatency(t, "vp9 (libvpx encode, videotoolbox decode)", e, d, w, h)
}

func TestVTVP9Corrupt(t *testing.T) {
	d := vtDecoderFor(t, VP9)
	defer d.Close()
	if err := d.Send(Packet{Data: []byte{0x00, 1, 2, 3}}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: %v", err)
	}
	if err := d.Send(Packet{Data: []byte{0x86, 0, 0, 0}}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("inter frame before a keyframe: %v", err)
	}
}

// Hidden alternate reference frames travel in superframes with a shown frame.
func TestVTVP9Superframes(t *testing.T) {
	d := vtDecoderFor(t, VP9)
	defer d.Close()
	var pts []time.Duration
	note := func(f *VideoFrame) {
		if f.Width != 160 || f.Height != 120 {
			t.Errorf("frame size %dx%d", f.Width, f.Height)
		}
		pts = append(pts, f.PTS)
		f.Release()
	}
	pkts := vtClip(vtTestVP9)
	for i, p := range pkts {
		if err := d.Send(Packet{Data: p, PTS: time.Duration(i) * time.Millisecond, Keyframe: i == 0}); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		drain(t, d, note)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, note)
	if len(pts) != len(pkts) {
		t.Errorf("decoded %d of %d frames", len(pts), len(pkts))
	}
	if !slices.IsSorted(pts) {
		t.Errorf("output order %v", pts)
	}
}

// vtReceiveAll drains d and returns the frames and the first error.
func vtReceiveAll(d VideoDecoder) (n int, first error) {
	for {
		f, err := d.Receive()
		switch {
		case err == nil:
			n++
			f.Release()
		case err == ErrAgain || err == io.EOF:
			return n, first
		case first == nil:
			first = err
		}
	}
}

// Damaged pictures must not end the stream: the next keyframe recovers it.
func TestVTCorruptPictureRecovers(t *testing.T) {
	for _, tc := range []struct {
		codec Codec
		clip  []string
	}{{H265, vtTestH265}, {VP9, vtTestVP9}} {
		t.Run(tc.codec.String(), func(t *testing.T) {
			d := vtDecoderFor(t, tc.codec)
			defer d.Close()
			pkts := vtClip(tc.clip)
			key := Packet{Data: pkts[0], Keyframe: true}
			if err := d.Send(key); err != nil {
				t.Fatal(err)
			}
			vtReceiveAll(d)
			bad := slices.Clone(pkts[0])
			for i := len(bad) / 2; i < len(bad); i++ {
				bad[i] ^= 0x5a
			}
			sendErr := d.Send(Packet{Data: bad, PTS: time.Millisecond, Keyframe: true})
			_, recvErr := vtReceiveAll(d)
			t.Logf("damaged picture: send %v, receive %v", sendErr, recvErr)
			key.PTS = time.Second
			if err := d.Send(key); err != nil {
				t.Fatalf("keyframe after damage: %v", err)
			}
			if err := d.Flush(); err != nil {
				t.Fatal(err)
			}
			if n, err := vtReceiveAll(d); n == 0 {
				t.Errorf("no picture after damage: %v", err)
			}
		})
	}
}

// show_existing_frame repeats a decoded picture and carries no picture data.
func TestVTVP9ShowExisting(t *testing.T) {
	d := vtDecoderFor(t, VP9)
	defer d.Close()
	pkts := vtClip(vtTestVP9)
	if err := d.Send(Packet{Data: pkts[0], Keyframe: true}); err != nil {
		t.Fatal(err)
	}
	vtReceiveAll(d)
	err := d.Send(Packet{Data: []byte{0x88}, PTS: time.Second})
	if flush := d.Flush(); flush != nil {
		t.Fatal(flush)
	}
	n, rerr := vtReceiveAll(d)
	t.Logf("show_existing_frame: send %v, %d frames, receive %v", err, n, rerr)
	if err != nil || rerr != nil || n != 1 {
		t.Errorf("send %v, %d frames, receive %v", err, n, rerr)
	}
}

func vtClip(list []string) [][]byte {
	out := make([][]byte, len(list))
	for i, s := range list {
		out[i], _ = base64.StdEncoding.DecodeString(s)
	}
	return out
}

// Streams from other encoders use B-frames. Pictures must leave the decoder in
// presentation order.
func TestVTH265BFrames(t *testing.T) {
	d := vtDecoderFor(t, H265)
	defer d.Close()
	var pts []time.Duration
	note := func(f *VideoFrame) { pts = append(pts, f.PTS); f.Release() }
	pkts := vtClip(vtTestH265)
	for i, p := range pkts {
		err := d.Send(Packet{Data: p, PTS: time.Duration(vtTestH265PTS[i]) * time.Millisecond, Keyframe: i == 0})
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		drain(t, d, note)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, note)
	if len(pts) != len(pkts) {
		t.Errorf("decoded %d of %d frames", len(pts), len(pkts))
	}
	if !slices.IsSorted(pts) {
		t.Errorf("output order %v", pts)
	}
}

func BenchmarkLatencyH265VT(b *testing.B) { benchLatency(b, H265, vtName, vtName) }
func BenchmarkLatencyVP9VT(b *testing.B)  { benchLatency(b, VP9, "libvpx", vtName) }

// Fixtures: 160x120 gradient, made with ffmpeg. The H.265 clip comes from x265
// with three B-frames, in decode order. The VP9 clip comes from libvpx with
// alternate reference frames, so it holds superframes.

// vtTestH265PTS is the presentation time of each access unit, in milliseconds.
var vtTestH265PTS = []int{0, 133, 67, 33, 100, 267, 200, 167, 233, 400, 333, 300, 367, 467, 433}

var vtTestH265 = []string{
	"AAAAAUABDAH//wFgAAADAJAAAAMAAAMAPJWUCQAAAAFCAQEBYAAAAwCQAAADAAADADygFCAgfLllZUpMLwFoCAAAAwAIAAADAPBAAAAAAUQBwHPBiQAAAAFOAQX///////////4sot4JtRdH27tVpP5/wvxOeDI2NSAoYnVpbGQgMjE3KSAtIDQuMzpbTGludXhdW0dDQyAxNi4xLjFdWzY0IGJpdF0gOGJpdCsxMGJpdCsxMmJpdCAtIEguMjY1L0hFVkMgY29kZWMgLSBDb3B5cmlnaHQgMjAxMy0yMDE4IChjKSBNdWx0aWNvcmV3YXJlLCBJbmMgLSBodHRwOi8veDI2NS5vcmcgLSBvcHRpb25zOiBjcHVpZD0xMTExMDM5IGZyYW1lLXRocmVhZHM9NSB3cHAgbm8tcG1vZGUgbm8tcG1lIG5vLXBzbnIgbm8tc3NpbSBsb2ctbGV2ZWw9MCBiaXRkZXB0aD04IGlucHV0LWNzcD0xIGZwcz0zMC8xIGlucHV0LXJlcz0xNjB4MTIwIGludGVybGFjZT0wIHRvdGFsLWZyYW1lcz0wIGxldmVsLWlkYz0wIGhpZ2gtdGllcj0xIHVoZC1iZD0wIHJlZj0xIG5vLWFsbG93LW5vbi1jb25mb3JtYW5jZSByZXBlYXQtaGVhZGVycyBhbm5leGIgbm8tYXVkIG5vLWVvYiBuby1lb3Mgbm8taHJkIGluZm8gaGFzaD0wIHRlbXBvcmFsLWxheWVycz0wIG9wZW4tZ29wIG1pbi1rZXlpbnQ9MyBrZXlpbnQ9MzAgZ29wLWxvb2thaGVhZD0wIGJmcmFtZXM9MyBiLWFkYXB0PTAgYi1weXJhbWlkIGJmcmFtZS1iaWFzPTAgcmMtbG9va2FoZWFkPTUgbG9va2FoZWFkLXNsaWNlcz0wIHNjZW5lY3V0PTAgbm8taGlzdC1zY2VuZWN1dCByYWRsPTAgbm8tc3BsaWNlIG5vLWludHJhLXJlZnJlc2ggY3R1PTMyIG1pbi1jdS1zaXplPTE2IG5vLXJlY3Qgbm8tYW1wIG1heC10dS1zaXplPTMyIHR1LWludGVyLWRlcHRoPTEgdHUtaW50cmEtZGVwdGg9MSBsaW1pdC10dT0wIHJkb3EtbGV2ZWw9MCBkeW5hbWljLXJkPTAuMDAgbm8tc3NpbS1yZCBuby1zaWduaGlkZSBuby10c2tpcCBuci1pbnRyYT0wIG5yLWludGVyPTAgbm8tY29uc3RyYWluZWQtaW50cmEgc3Ryb25nLWludHJhLXNtb290aGluZyBtYXgtbWVyZ2U9MiBsaW1pdC1yZWZzPTAgbm8tbGltaXQtbW9kZXMgbWU9MCBzdWJtZT0wIG1lcmFuZ2U9NTcgdGVtcG9yYWwtbXZwIG5vLWZyYW1lLWR1cCBuby1obWUgbm8td2VpZ2h0cCBuby13ZWlnaHRiIG5vLWFuYWx5emUtc3JjLXBpY3MgZGVibG9jaz0wOjAgbm8tc2FvIG5vLXNhby1ub24tZGVibG9jayByZD0yIHNlbGVjdGl2ZS1zYW89MCBlYXJseS1za2lwIHJza2lwIGZhc3QtaW50cmEgbm8tdHNraXAtZmFzdCBuby1jdS1sb3NzbGVzcyBuby1iLWludHJhIG5vLXNwbGl0cmQtc2tpcCByZHBlbmFsdHk9MCBwc3ktcmQ9Mi4wMCBwc3ktcmRvcT0wLjAwIG5vLXJkLXJlZmluZSBuby1sb3NzbGVzcyBjYnFwb2Zmcz0wIGNycXBvZmZzPTAgcmM9Y3JmIGNyZj0zMi4wIHFjb21wPTAuNjAgcXBzdGVwPTQgc3RhdHMtd3JpdGU9MCBzdGF0cy1yZWFkPTAgaXByYXRpbz0xLjQwIHBicmF0aW89MS4zMCBhcS1tb2RlPTEgYXEtc3RyZW5ndGg9MC4wMCBjdXRyZWUgem9uZS1jb3VudD0wIG5vLXN0cmljdC1jYnIgcWctc2l6ZT0zMiBuby1yYy1ncmFpbiBxcG1heD02OSBxcG1pbj0wIG5vLWNvbnN0LXZidiBzYXI9MSBvdmVyc2Nhbj0wIHZpZGVvZm9ybWF0PTUgcmFuZ2U9MCBjb2xvcnByaW09MiB0cmFuc2Zlcj0yIGNvbG9ybWF0cml4PTIgY2hyb21hbG9jPTAgZGlzcGxheS13aW5kb3c9MCBjbGw9MCwwIG1pbi1sdW1hPTAgbWF4LWx1bWE9MjU1IGxvZzItbWF4LXBvYy1sc2I9OCB2dWktdGltaW5nLWluZm8gdnVpLWhyZC1pbmZvIHNsaWNlcz0xIG5vLW9wdC1xcC1wcHMgbm8tb3B0LXJlZi1saXN0LWxlbmd0aC1wcHMgbm8tbXVsdGktcGFzcy1vcHQtcnBzIHNjZW5lY3V0LWJpYXM9MC4wNSBuby1vcHQtY3UtZGVsdGEtcXAgbm8tYXEtbW90aW9uIG5vLWhkcjEwIG5vLWhkcjEwLW9wdCBuby1kaGRyMTAtb3B0IG5vLWlkci1yZWNvdmVyeS1zZWkgYW5hbHlzaXMtcmV1c2UtbGV2ZWw9MCBhbmFseXNpcy1zYXZlLXJldXNlLWxldmVsPTAgYW5hbHlzaXMtbG9hZC1yZXVzZS1sZXZlbD0wIHNjYWxlLWZhY3Rvcj0wIHJlZmluZS1pbnRyYT0wIHJlZmluZS1pbnRlcj0wIHJlZmluZS1tdj0xIHJlZmluZS1jdHUtZGlzdG9ydGlvbj0wIG5vLWxpbWl0LXNhbyBjdHUtaW5mbz0wIG5vLWxvd3Bhc3MtZGN0IHJlZmluZS1hbmFseXNpcy10eXBlPTAgY29weS1waWM9MSBtYXgtYXVzaXplLWZhY3Rvcj0xLjAgbm8tZHluYW1pYy1yZWZpbmUgbm8tc2luZ2xlLXNlaSBuby1oZXZjLWFxIG5vLXN2dCBuby1maWVsZCBxcC1hZGFwdGF0aW9uLXJhbmdlPTEuMDAgc2NlbmVjdXQtYXdhcmUtcXA9MGNvbmZvcm1hbmNlLXdpbmRvdy1vZmZzZXRzIHJpZ2h0PTAgYm90dG9tPTAgZGVjb2Rlci1tYXgtcmF0ZT0wIG5vLXZidi1saXZlLW11bHRpLXBhc3Mgbm8tbWNzdGYgbm8tc2JyYyBuby1mcmFtZS1yY4AAAAABQAEMAf//AWAAAAMAkAAAAwAAAwA8lZQJAAAAAUIBAQFgAAADAJAAAAMAAAMAPKAUICB8uWVlSkwvAWgIAAADAAgAAAMA8EAAAAABRAHAc8GJAAAAAU4BBf///////////iyi3gm1F0fbu1Wk/n/C/E54MjY1IChidWlsZCAyMTcpIC0gNC4zOltMaW51eF1bR0NDIDE2LjEuMV1bNjQgYml0XSA4Yml0KzEwYml0KzEyYml0IC0gSC4yNjUvSEVWQyBjb2RlYyAtIENvcHlyaWdodCAyMDEzLTIwMTggKGMpIE11bHRpY29yZXdhcmUsIEluYyAtIGh0dHA6Ly94MjY1Lm9yZyAtIG9wdGlvbnM6IGNwdWlkPTExMTEwMzkgZnJhbWUtdGhyZWFkcz01IHdwcCBuby1wbW9kZSBuby1wbWUgbm8tcHNuciBuby1zc2ltIGxvZy1sZXZlbD0wIGJpdGRlcHRoPTggaW5wdXQtY3NwPTEgZnBzPTMwLzEgaW5wdXQtcmVzPTE2MHgxMjAgaW50ZXJsYWNlPTAgdG90YWwtZnJhbWVzPTAgbGV2ZWwtaWRjPTAgaGlnaC10aWVyPTEgdWhkLWJkPTAgcmVmPTEgbm8tYWxsb3ctbm9uLWNvbmZvcm1hbmNlIHJlcGVhdC1oZWFkZXJzIGFubmV4YiBuby1hdWQgbm8tZW9iIG5vLWVvcyBuby1ocmQgaW5mbyBoYXNoPTAgdGVtcG9yYWwtbGF5ZXJzPTAgb3Blbi1nb3AgbWluLWtleWludD0zIGtleWludD0zMCBnb3AtbG9va2FoZWFkPTAgYmZyYW1lcz0zIGItYWRhcHQ9MCBiLXB5cmFtaWQgYmZyYW1lLWJpYXM9MCByYy1sb29rYWhlYWQ9NSBsb29rYWhlYWQtc2xpY2VzPTAgc2NlbmVjdXQ9MCBuby1oaXN0LXNjZW5lY3V0IHJhZGw9MCBuby1zcGxpY2Ugbm8taW50cmEtcmVmcmVzaCBjdHU9MzIgbWluLWN1LXNpemU9MTYgbm8tcmVjdCBuby1hbXAgbWF4LXR1LXNpemU9MzIgdHUtaW50ZXItZGVwdGg9MSB0dS1pbnRyYS1kZXB0aD0xIGxpbWl0LXR1PTAgcmRvcS1sZXZlbD0wIGR5bmFtaWMtcmQ9MC4wMCBuby1zc2ltLXJkIG5vLXNpZ25oaWRlIG5vLXRza2lwIG5yLWludHJhPTAgbnItaW50ZXI9MCBuby1jb25zdHJhaW5lZC1pbnRyYSBzdHJvbmctaW50cmEtc21vb3RoaW5nIG1heC1tZXJnZT0yIGxpbWl0LXJlZnM9MCBuby1saW1pdC1tb2RlcyBtZT0wIHN1Ym1lPTAgbWVyYW5nZT01NyB0ZW1wb3JhbC1tdnAgbm8tZnJhbWUtZHVwIG5vLWhtZSBuby13ZWlnaHRwIG5vLXdlaWdodGIgbm8tYW5hbHl6ZS1zcmMtcGljcyBkZWJsb2NrPTA6MCBuby1zYW8gbm8tc2FvLW5vbi1kZWJsb2NrIHJkPTIgc2VsZWN0aXZlLXNhbz0wIGVhcmx5LXNraXAgcnNraXAgZmFzdC1pbnRyYSBuby10c2tpcC1mYXN0IG5vLWN1LWxvc3NsZXNzIG5vLWItaW50cmEgbm8tc3BsaXRyZC1za2lwIHJkcGVuYWx0eT0wIHBzeS1yZD0yLjAwIHBzeS1yZG9xPTAuMDAgbm8tcmQtcmVmaW5lIG5vLWxvc3NsZXNzIGNicXBvZmZzPTAgY3JxcG9mZnM9MCByYz1jcmYgY3JmPTMyLjAgcWNvbXA9MC42MCBxcHN0ZXA9NCBzdGF0cy13cml0ZT0wIHN0YXRzLXJlYWQ9MCBpcHJhdGlvPTEuNDAgcGJyYXRpbz0xLjMwIGFxLW1vZGU9MSBhcS1zdHJlbmd0aD0wLjAwIGN1dHJlZSB6b25lLWNvdW50PTAgbm8tc3RyaWN0LWNiciBxZy1zaXplPTMyIG5vLXJjLWdyYWluIHFwbWF4PTY5IHFwbWluPTAgbm8tY29uc3QtdmJ2IHNhcj0xIG92ZXJzY2FuPTAgdmlkZW9mb3JtYXQ9NSByYW5nZT0wIGNvbG9ycHJpbT0yIHRyYW5zZmVyPTIgY29sb3JtYXRyaXg9MiBjaHJvbWFsb2M9MCBkaXNwbGF5LXdpbmRvdz0wIGNsbD0wLDAgbWluLWx1bWE9MCBtYXgtbHVtYT0yNTUgbG9nMi1tYXgtcG9jLWxzYj04IHZ1aS10aW1pbmctaW5mbyB2dWktaHJkLWluZm8gc2xpY2VzPTEgbm8tb3B0LXFwLXBwcyBuby1vcHQtcmVmLWxpc3QtbGVuZ3RoLXBwcyBuby1tdWx0aS1wYXNzLW9wdC1ycHMgc2NlbmVjdXQtYmlhcz0wLjA1IG5vLW9wdC1jdS1kZWx0YS1xcCBuby1hcS1tb3Rpb24gbm8taGRyMTAgbm8taGRyMTAtb3B0IG5vLWRoZHIxMC1vcHQgbm8taWRyLXJlY292ZXJ5LXNlaSBhbmFseXNpcy1yZXVzZS1sZXZlbD0wIGFuYWx5c2lzLXNhdmUtcmV1c2UtbGV2ZWw9MCBhbmFseXNpcy1sb2FkLXJldXNlLWxldmVsPTAgc2NhbGUtZmFjdG9yPTAgcmVmaW5lLWludHJhPTAgcmVmaW5lLWludGVyPTAgcmVmaW5lLW12PTEgcmVmaW5lLWN0dS1kaXN0b3J0aW9uPTAgbm8tbGltaXQtc2FvIGN0dS1pbmZvPTAgbm8tbG93cGFzcy1kY3QgcmVmaW5lLWFuYWx5c2lzLXR5cGU9MCBjb3B5LXBpYz0xIG1heC1hdXNpemUtZmFjdG9yPTEuMCBuby1keW5hbWljLXJlZmluZSBuby1zaW5nbGUtc2VpIG5vLWhldmMtYXEgbm8tc3Z0IG5vLWZpZWxkIHFwLWFkYXB0YXRpb24tcmFuZ2U9MS4wMCBzY2VuZWN1dC1hd2FyZS1xcD0wY29uZm9ybWFuY2Utd2luZG93LW9mZnNldHMgcmlnaHQ9MCBib3R0b209MCBkZWNvZGVyLW1heC1yYXRlPTAgbm8tdmJ2LWxpdmUtbXVsdGktcGFzcyBuby1tY3N0ZiBuby1zYnJjIG5vLWZyYW1lLXJjgAAAAAEoAawxIV51QCxCucaBp82S+IltpodD3ISGtFL5njrLGmpcBrqU6TSucaFoQuIqNXUNLw==",
	"AAAAAQIB0CFJiBiRwmAWrJPKgKX+YxxiRiA=",
	"AAAAAQIB4ESVYIGhErDSjxpwISC0lQ==",
	"AAAAAQAB4CT9eiQOSJWAhQglYCtgMmA=",
	"AAAAAQAB4Ga14IHBEzCub62/gKhOR8w=",
	"AAAAAQIB0ECSVWIGBEtAknlMnHqwYH+G1ADokwqT3UVa0JGA",
	"AAAAAQIB4MIlVVggakUs2qJANew+9LTKYA==",
	"AAAAAQAB4Kb116JA4IlYrm8aECEgJuA=",
	"AAAAAQAB4OItV4IHBErAhQhQDHd0bkw=",
	"AAAAAQIB0GCyVVYgYEZMgJJ5Nkx69stIzKL+jgeAYA==",
	"AAAAAQIB4UIlUlYIGhErhzgioH0NrFmA",
	"AAAAAQAB4Sb116JA5IlYhQglYHd0ocw=",
	"AAAAAQAB4WItV4IHJErAhQg6BHd0ocw=",
	"AAAAAQIB0HC1VViBiRkuknlMnHqHIrip0tA=",
	"AAAAAQAB4aItSeCBwRKwhQhQDHd0ocw=",
}

var vtTestVP9 = []string{
	"gkmDQgAJ8Ad2CDgkHBgUAAFwfD07frSAIyeXvGzVKbr4o4MUhevgIABq0P//y2Slyr+LAn/9hh/6O32iCh+k1cG7VNPr29Hzhn7susE+crPkubwV5jzAyz/7YLizOKHaBjNx7xnywv1tRW8EwwcBiCbpTiw88dxshcrp0vPDkKCmD9c7qyvl9RMC4UlAhmNLbVJKhbaLI0UTzbT5MCzyxJ6HM/pI55VvW0D0ldQjqmzbD/ZLWZdYOqA5Jr8B2PoKxZbl432iAs68WiaclfoKtj6a22MNYThMlT6pdldNou6JfC8GVFgsCTN+u6SRn2l1H5M6W/EcbUh4l5rv052wPCjt/oOQlfmScXW/jgURhPTI4seNpBETVPsQzeWVI1351D6qrqf8BAb8qe+8SzyBbB2ovUUnpD9dQbqkkd9a7/AaZwp3nE/cefjgWTNJ/3V+Rp+/XqGVf0UD1o0uQDKvbMfARHQOBxfaQFtVJsZOAs49UiQ7aG8WTnRADUf3EFLKpIuu2ohhzI/r4d0ytuNmoP/6dpkpvhaM1g2hwHCH+/WbDE5zLe4mMA==",
	"hACASXpAhEAAHHKpIa/3iph/VUBBE0gAfwUirl4rbnvbgBvPjr7WsQ/2ayATNTzo/Kf/0hhGNqkYgx5Mc/A4ZdVu0uq9pk4Itnqmzo/39oEY4Jj0dl+JSgOWbbjlvoFGUwn0bp1Q2lW3/1KTJSy36jL8jvJuoW1he6m8K9oMLmMoUcwn/wZrMA5OKoOIGpaE9B8UsufY6MF4HNw84Gv66/IxVizJzpZNkObQcA8nEdNIlaLhtb7/klw1+CreV1hBg5xZBXMpxum+id/4BX/QwIPQ5qrVktoa9nyHwV1GuVaDOivylpLTrfTwDlL5/rTI/UlKrHiyWDz0+n98t9o7WfxlVn3VBZR8Gk5gGY97Ma+Lw+2OipZ5U8AwcquEPsXOnBkz3scmuZ/8a0nZRJKJTBG0K7yvOFMmSpcDzGoeKeid1N9IehE8sQZ6UglDXXRek8ckKzn9yzgFn2xRYIx4oT5TWmlvPIlaqNFXdoMubkzFBce6z5BlUS2CCACGAECW8IEYAAAMYAAAfYrCo6LIahNMLPDgTfsC22CiHiSKL3M29edhRv1rf1VlFfFOHIHn1IDJfAE2AMk=",
	"hgBAlvCBGAAADGAAAHacV+7D67g7IbOWyoAb8+FWcU112mY+B0bNOW6hsTkdhn4sdawNgbBM1YGIv3g=",
	"hgBAlowgReAABWAZ6AAAdYWlg/NENWR3ZtBy8qtko8C9Fwj6oFbWRN9drOuylrNye7HhTfCSgA==",
	"hgBAlvCBF4AADGAAAHSrzDgb6AgkBG0h1u36EoKNVu7NF3H+VEunynh3ssACnlEAFGMO9oA=",
	"hgBAlowgRcAAA3AAAHcsslOzUdvbCsaheZcUoJSuLu7ZhOAhP1zeEpqjL/umOb4lbt2SRNIKlTMmm0tDTDMLELgKGI8EenwRwAA=",
	"hgBAlowgRcAAA2AAAH39fiUOJK5eYGxM5ivGQHQyrt68/v2aV54+wfY4UTZ2uPSbIfG8fvYnXcaKFsqcoA==",
	"hgBAlowgRaAAA3AAAH2KlD48jB0+M5fcglyqoapV9DMHIg62I9F01Y3jZh7B29bzOvfP9reLqypJsazIpTMBVAA=",
	"hgBAlowgRaAAA2AAAH2BH/3cCSouidLmyEJC52q4dL2oMsKmBKQ5oFSM3e/qVO3jRProPnHAoQKKwPGNEhQ=",
	"hgBAlowgRYAAA2AAAHPYJ297N8UAAzDTcG5aAlF2xyjC4qyXJM8EJkW/i+7E2e7QMkIOVdyRDD5A4rg9KuSbAA==",
	"hgBAlowgRYAAA3AAAHqJB0c5rS2Rr2PAUYCCkfto52qip3LFPB8C3PvFZum0mCLIBD0F1LXrl8Ud2Gu396YBZsGC10TsnAA=",
	"hgBAlvCBFYAADHAAAH2HiFE739JJkystVQUTUnEIvQVXL9BycoPDhvpZz44ZH9UBBQNmRFOibBKcDFiFELHoKoA=",
	"hgEAkpwASgAAAyAAAG10Uq4A",
	"hABAhU6QIDAACwB8i/VYsAB72mA4jtF9xfV/S5VaDAAAetTUGLh7//9RzOMa5W6b9/7KMcAqaQHjOXE9GCPuChyUq43kOFXkv+QAAQf//WnULbm10SmWFb//5NTTASzj//tHT3t/6npXOuvYWHkRyK5DOe+XzweWRACij/rw6D2T6X00jOCm0EHAAADUrN3ekigsz2M+7duqdEDNE0xP/5BFP7wMCRP7z/8QT2P+rD6ABxxY4/zV5WZlyYGEAGYlpe5gP/hyTwXeEMkv+5FLnwt0+A+6PN3oGlHUwemPi42GudVwENGC/K9blhXccva6V2fESS0VxEQ+tp6GetOUUnbvP/4jqNa//rkR+pG+Rjrj9vNL4hfhX6bW9oKfZNZ+EQ2v4xXKnOpA2auyBC3LwzTtg9DxjVpsOAiHKaIt/Ua0IgQnboIlt4GrsvqiBbI74jDw0YepMIjKoSSr6h87/TWlnZTVKPPxQmywYpXMuQ6qiPat6vHn8z3hv/YCezIUJx99iE7ophSKm5r+bISb9idJ5pu+pBnlcAT/YaArFWP/ESvCuIpP/WsqU6f1vC0exIp798uFim65jU0z7XhLJwa2Ekh2WUHEkIotkJa2XvLeAgP+5TV/7X6MuBgTD/Qo/7Amkc+Pm7qjqcH/pcZA2XLnetAB8xeM2554V8p3KLZoo9LWc13mS92F4H0WIeOSekgIYmyB422e11rg/EyUvbdSYGG2wsAUMxAl4O6LRhkplib8OK/MpgctQEXPKe6lyYIidmr5uYwo1btdPW2Joye5izW3vfNvbxM7ini5fH4Dvs8pdsmbLqEFt9p26YGn58VjHm+36xBNK91TTrREXngAhgBBDvCBFoAADGAAAHt4JSpQ/E9kBrKzTmQvgASZY2yMcgJyUUaNO62Z/0y5hvdSWpMnY6ixa7HT5QDJggI7AMk=",
	"hgBBDvCBFoAADHAAAHn/4mB6pZYwofbvAXK4mg/h0VaC5wUjuP22vRuMvGB/T8So7jubyljKCv3BxOSQ/2if+qdy5SAA",
	"hgBBDvCBFgAADHAAAHpLHOlOgcSpnz5rotfbnD12Oehc8vMq3B53NRbWH3MyPBPMA5gHTP3T3Cd53rMv9gA=",
	"hgBBDvCBFgAADGAAAHow6Q55IRF5CcF/uPnQZnLqoEQqc8cgLPa2VRwmVbAUKvVvlGw71O/1lqYmN1DfTwgS82IA",
	"hgBBDvCBFYAADHAAADbMvA/5NJgNrk9oqdYxYc6fWH+i2f3Fu3ag0vC1U7UQiTBTV5rZjAd717fvFeQeHrtfLnOcEF1oIjkiMA==",
	"hgBBDowgRWAAA3AAAGHr/jDdgQ0mC6KfLHDVrfqmWyEyT67ncZkf9SoPaiAnvduPegShjeZznTvqYQzFRad9jvWTq39crA==",
	"hgBBDvCBFQAADHAAAGGioDOnB7r/VtOrfcI7x/W1bKJ42hgfAEqPc9aKRScQx1e07s8zAMIOTWpJNIRq02ZBMbOSxoA=",
	"hgBBDvCBFQAAFHACsNgAYCDSIMvohgCvk2UazJ3fV0jN+mFz/NeSa7pjdeYqJJyxgvpnUYwHXRcphHe+C7Cu1WqTXx2IAA==",
	"hgBBDvCBFIAAIHHTDtaQAAAAYcj1/MyXR4ER5xB164h6t7f2EqCN4AbqWEx/MZGDCJeXHMeksA21lV+ulXkbn18+oY0S1D6WQPqWb2CQTrUDhncvzNLYbacJ1yh4Yr9CAAA=",
	"hgBBDvCBFIAADHAAAFwXgitcm07kcze8HOGHq8FKHHIkDqzNnaZTYzgWJBt6/mQ4v9Zn6tYQ2EgqY2riEqPT2Lo95gJX0oA=",
	"hgBBDowgRQAAA2AAAGBY7/rLw6dqz13VSPyyhaJHlliWs/aN7c33WKV8TVVMBDNHcM8oAA==",
	"hgCBCpwASeAAAyAAAG10Uq4A",
	"hACASU6QICAAAwBwkTCtAABpPeTR0j///9bTucNb4bgHbHdgfddvm8YxH39ypyd51fk67Rrc82D1hGx0zkE5FwPqLk6gdyx69aIKYoKYSEinSwcDfDKf////7I87nvcy4MJcYbGk1izDf0G4xsoaEkMS///79214y5LAHkNE7HQR8l50JWjLQwAjVqm7CVd7cx+zprofN9Tc15NAjDrzqohSL/uPXD0B6Xif+26Ib1Lj+v05nnpJyJ5wfZS3mLzwsWh5EUNAMnyMaiNKVIcNfmPkOnXF1hKpKxAQBAe9GT/NBcU6OmBnYo2i6hnlvI4X4Ppf6x+aIMtws+YORUa70d0k5E84Pspbzlk7RLWZLzFhT/LtVM4K5fNBteyOO1q23v/btFltPeG2qZrS51dbmNGZjyErP54f/l3BWm+DEz8/3093JA+7qo3fikH8VkzuiGTClNok1q9lUGjIqgL2m08TxS0hEhGnjQx4uBkR+Lhz9kc5+USZ4VMIGRK16i0nOJ//w1h08aqSPjO39CeQ6XP6OAzYE+Zom7v/rkgKVNaifADWVObAS/9KidOWeoCt0ERzKTdmyak1R9lw3I9HZwOQ+cyeMOZZPz+f/F/pYgg2FTgwEpr3pNWC5h43xXwscvVK9QX/+HS///0W3zaAfPP///F3BHgH/Pdv5z28kA//5VuAqWqOMW9DuhRLGnYjMnN6i3//HUgIl7vilyNQsmaeXOzvAAAKGl+Cqdqga//7D9V98pLKd+IYGAPAQC8+vsgYAwMoE1ccgheYBwFpjkjHw6bXgTf1mr0bGYBfY1Bx3/+MW7/snW91GtBQhqBu7/T47CwYD+fXXxRzOsjShl2ccAXlQRdj+XButnMFghGU2fQrDY/J7d2tRvknq/1ie9Eu/pdRPk9aIHKtX42kfQ4HrvQuhacH0Dv+ATBOFeYufiGzdZbNpyBPYHQII7teifm4/AhIQ2YL614OYG+CuACGAECW8IEUgAAMYAAAaQmQpHbG9ZkC5K5Q+A4zfyD7tuqDykHpRak/VDjqliEfyNoAyeECMQDJ",
	"hgBAlvCBFIAAFGALH6AAWegAvM+nLtf7mFiYbhk3Z04CIpGLHeVYVa9kZ2B8fcUYh1ayIIA=",
	"hgBAlvCBFAAADGAAAFmLhvTqLZgcdTvcKZAFsaiYk6Cta/S6/+RGnVCzC5vW5Dmy/D6c7LdHCA/8N3H5M4gzUFtgS90GeA==",
	"hgBAlvCBFAAAFGALDOAAWc0/WuUdz47R+LVPw4ZrXyOi1NzRU1SAdYSd6yY3ELzIlAA=",
	"hgEAkpwASEAAA2AAAAGj///CAoAAR//8ZCawdXZg",
}
