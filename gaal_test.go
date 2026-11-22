package gaal

import (
	"errors"
	"io"
	"math"
	"testing"
	"time"
)

func fillTestFrame(f *VideoFrame, n int) {
	w, h := f.Width, f.Height
	f.PTS = time.Duration(n) * time.Second / 30
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f.Y[y*f.YStride+x] = byte(40 + (x+y+n*2)*160/(w+h+60))
		}
	}
	for y := 0; y < h/2; y++ {
		for x := 0; x < w/2; x++ {
			f.U[y*f.UVStride+x] = byte(90 + x*70/w)
			f.V[y*f.UVStride+x] = byte(100 + y*70/h)
		}
	}
}

func testFrame(w, h, n int) *VideoFrame {
	f := NewVideoFrame(w, h, I420)
	fillTestFrame(f, n)
	return f
}

// psnrChroma compares chroma after converting b to I420.
func psnrChroma(a, b *VideoFrame) float64 {
	c := NewVideoFrame(b.Width, b.Height, I420)
	b.ToI420(c)
	var sum float64
	cw, ch := a.Width/2, a.Height/2
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			du := float64(a.U[y*a.UVStride+x]) - float64(c.U[y*c.UVStride+x])
			dv := float64(a.V[y*a.UVStride+x]) - float64(c.V[y*c.UVStride+x])
			sum += du*du + dv*dv
		}
	}
	if sum == 0 {
		return 99
	}
	return 10 * math.Log10(255*255*float64(2*cw*ch)/sum)
}

func psnrY(a, b *VideoFrame) float64 {
	var sum float64
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			sum += d * d
		}
	}
	if sum == 0 {
		return 99
	}
	return 10 * math.Log10(255*255*float64(a.Width*a.Height)/sum)
}

// drain receives until ErrAgain or EOF.
func drain[In, Out any](t testing.TB, s Stream[In, Out], fn func(Out)) {
	t.Helper()
	for {
		o, err := s.Receive()
		if err == ErrAgain || err == io.EOF {
			return
		}
		if hwFallback(t, err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		fn(o)
	}
}

// hwFallbacks counts hardware decoders that failed and went on in software.
var hwFallbacks int

// hwFallback reports, and logs, an error from a hardware decoder that failed
// mid-stream and continues in software. It loses pictures up to the next
// keyframe, so loops that check every picture still see that.
func hwFallback(t testing.TB, err error) bool {
	if !errors.Is(err, errHWFallback) {
		return false
	}
	t.Helper()
	t.Logf("tolerated: %v", err)
	hwFallbacks++
	return true
}

// allocsPerRun is testing.AllocsPerRun, measured again if a hardware decoder
// fell back to software meanwhile, as building the new session allocates.
func allocsPerRun(runs int, f func()) float64 {
	n := hwFallbacks
	a := testing.AllocsPerRun(runs, f)
	if hwFallbacks != n {
		a = testing.AllocsPerRun(runs, f)
	}
	return a
}

func encodeClip(t *testing.T, name string, cfg VideoConfig, frames int) []Packet {
	t.Helper()
	enc, err := New(Options{Backend: name}).NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var pkts []Packet
	keep := func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() }
	for i := 0; i < frames; i++ {
		if err := enc.Send(testFrame(cfg.Width, cfg.Height, i)); err != nil {
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

// TestVideoBackends encodes with every usable encoder and decodes the result
// with every usable decoder.
func TestVideoBackends(t *testing.T) {
	const w, h, frames = 320, 240, 30
	for _, codec := range []Codec{H264, H265, VP8, VP9, JPEG} {
		for _, ei := range New(Options{}).Backends(codec, Encode) {
			t.Run(codec.String()+"/"+ei.Name, func(t *testing.T) {
				cfg := VideoConfig{Codec: codec, Width: w, Height: h, Bitrate: 3_000_000}
				if enc, err := New(Options{Backend: ei.Name}).NewVideoEncoder(cfg); err != nil {
					confUnavailable(t, codec, Encode, ei.Name, err)
				} else {
					enc.Close()
				}
				pkts := encodeClip(t, ei.Name, cfg, frames)
				if len(pkts) != frames {
					t.Fatalf("got %d packets, want %d", len(pkts), frames)
				}
				if !pkts[0].Keyframe {
					t.Errorf("first packet is not a keyframe")
				}
				for _, di := range New(Options{}).Backends(codec, Decode) {
					t.Run(di.Name, func(t *testing.T) {
						dec, err := New(Options{Backend: di.Name}).NewVideoDecoder(cfg)
						if err != nil {
							confUnavailable(t, codec, Decode, di.Name, err)
						}
						defer dec.Close()
						n := 0
						var q, qc float64
						var firstPTS time.Duration
						got := func(f *VideoFrame) {
							if n == 0 {
								q, firstPTS = psnrY(testFrame(w, h, 0), f), f.PTS
								qc = psnrChroma(testFrame(w, h, 0), f)
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
						if q < 25 || qc < 25 {
							t.Errorf("first frame PSNR luma %.1f chroma %.1f dB", q, qc)
						}
						if firstPTS != 0 {
							t.Errorf("first PTS %v", firstPTS)
						}
						t.Logf("first frame PSNR %.1f dB", q)
					})
				}
			})
		}
	}
}

// TestSteadyStateAllocs measures allocations per frame once pools are warm.
func TestSteadyStateAllocs(t *testing.T) {
	const w, h = 320, 240
	for _, tc := range []struct {
		codec    Codec
		enc, dec string
		limit    float64
	}{
		{H264, "nvenc", "nvdec", 0},
		{JPEG, "turbojpeg", "turbojpeg", 0},
		{JPEG, "turbojpeg", "nvdec", 0},
		{VP8, "libvpx", "libvpx", 0},
		{VP8, "libvpx", "nvdec", 0},
		{H265, "nvenc", "nvdec", 0},
		{VP9, "libvpx", "libvpx", 0},
		{VP9, "libvpx", "nvdec", 0},
		{H265, "mediacodec", "mediacodec", 0},
		{VP9, "mediacodec", "mediacodec", 0},
		{H264, "mediacodec", "mediacodec", 0},
		{VP8, "mediacodec", "mediacodec", 0},
		{H264, "videotoolbox", "videotoolbox", 0},
		{JPEG, "videotoolbox", "videotoolbox", 0},
	} {
		t.Run(tc.codec.String()+"/"+tc.enc+"-"+tc.dec, func(t *testing.T) {
			cfg := VideoConfig{Codec: tc.codec, Width: w, Height: h}
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
			allocs := allocsPerRun(200, step)
			t.Logf("%.2f allocations per frame", allocs)
			if allocs > tc.limit {
				t.Errorf("%.2f allocations per frame, limit %.0f", allocs, tc.limit)
			}
		})
	}
}

func TestOpusRoundTrip(t *testing.T) {
	for _, b := range New(Options{}).Backends(Opus, Encode) {
		t.Run(b.Name, func(t *testing.T) { opusRoundTrip(t, New(Options{Backend: b.Name})) })
	}
}

func opusRoundTrip(t *testing.T, l *Library) {
	enc, err := l.NewAudioEncoder(AudioConfig{Codec: Opus, Bitrate: 64000})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := l.NewAudioDecoder(AudioConfig{Codec: Opus})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm := make([]int16, 48000*2)
	for i := 0; i < len(pcm)/2; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*440*float64(i)/48000))
		pcm[2*i], pcm[2*i+1] = v, v
	}
	if err := enc.Send(&AudioFrame{Samples: pcm, SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	packets, size, samples := 0, 0, 0
	var energy float64
	onFrame := func(f *AudioFrame) {
		for _, s := range f.Samples {
			energy += float64(s) * float64(s)
		}
		samples += len(f.Samples)
		f.Release()
	}
	onPacket := func(p *EncodedPacket) {
		packets++
		size += len(p.Data)
		if err := dec.Send(p.Packet); err != nil {
			t.Fatal(err)
		}
		p.Release()
		drain(t, dec, onFrame)
	}
	drain(t, enc, onPacket)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, enc, onPacket)
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, dec, onFrame)
	if packets < 50 || samples < len(pcm)-1920*2 || samples > len(pcm)+1920*2 {
		t.Errorf("got %d packets and %d samples", packets, samples)
	}
	if rms := math.Sqrt(energy / float64(samples)); rms < 7000 || rms > 10000 {
		t.Errorf("RMS %.0f, want about 8485", rms)
	}
	if kbps := size * 8 / 1000; kbps < 35 || kbps > 90 {
		t.Errorf("bitrate %d kbit/s, want about 64", kbps)
	}
}

func TestOpusSteadyStateAllocs(t *testing.T) {
	l := New(Options{})
	enc, err := l.NewAudioEncoder(AudioConfig{Codec: Opus})
	if err != nil {
		t.Skip(err)
	}
	defer enc.Close()
	dec, err := l.NewAudioDecoder(AudioConfig{Codec: Opus})
	if err != nil {
		t.Skip(err)
	}
	defer dec.Close()
	in := &AudioFrame{Samples: make([]int16, 1920), SampleRate: 48000, Channels: 2}
	onFrame := func(f *AudioFrame) { f.Release() }
	onPacket := func(p *EncodedPacket) {
		if err := dec.Send(p.Packet); err != nil {
			t.Fatal(err)
		}
		p.Release()
		drain(t, dec, onFrame)
	}
	step := func() {
		if err := enc.Send(in); err != nil {
			t.Fatal(err)
		}
		drain(t, enc, onPacket)
	}
	for i := 0; i < 10; i++ {
		step()
	}
	if a := testing.AllocsPerRun(200, step); a != 0 {
		t.Errorf("%.2f allocations per frame", a)
	}
}

func TestJPEGHelpers(t *testing.T) {
	for _, name := range []string{"", "go-image-jpeg", "turbojpeg"} {
		l := New(Options{Backend: name})
		src := testFrame(64, 48, 0)
		p, err := l.EncodeJPEG(src, 90)
		if errors.Is(err, ErrNoBackend) {
			t.Logf("%q: %v", name, err)
			continue
		}
		if err != nil {
			t.Fatal(name, err)
		}
		got, err := l.DecodeJPEG(p.Data)
		if err != nil {
			t.Fatal(name, err)
		}
		if q := psnrY(src, got); q < 30 {
			t.Errorf("%q: PSNR %.1f", name, q)
		}
		got.Release()
		p.Release()
	}
}

func TestNV12ToI420(t *testing.T) {
	n := NewVideoFrame(4, 2, NV12)
	copy(n.U, []byte{1, 2, 3, 4, 5, 6, 7, 8}[:4])
	i := NewVideoFrame(4, 2, I420)
	if err := n.ToI420(i); err != nil {
		t.Fatal(err)
	}
	if i.U[0] != 1 || i.V[0] != 2 || i.U[1] != 3 || i.V[1] != 4 {
		t.Fatalf("U %v V %v", i.U, i.V)
	}
}

// fakeStream fails on Send, or outputs from its second input.
type fakeStream struct {
	name string
	fail bool
	seen int
	done int
}

func (f *fakeStream) Send(int) error {
	if f.fail {
		return errors.New("boom")
	}
	f.seen++
	return nil
}
func (f *fakeStream) Receive() (int, error) {
	if f.seen >= 2 && f.done < f.seen {
		f.done = f.seen
		return f.seen, nil
	}
	return 0, ErrAgain
}
func (f *fakeStream) Flush() error         { return nil }
func (f *fakeStream) Close() error         { return nil }
func (f *fakeStream) Backend() BackendInfo { return BackendInfo{Name: f.name} }

func TestFailoverReplaysInput(t *testing.T) {
	f := &failover[int, int]{
		backends: []backend{{BackendInfo: BackendInfo{Name: "bad"}}, {BackendInfo: BackendInfo{Name: "good"}}},
		clone:    func(i int) int { return i }, nb: &NoBackendError{}, limit: replayLimit{items: 4},
		open: func(b backend) (Stream[int, int], error) {
			return &fakeStream{name: b.Name, fail: b.Name == "bad"}, nil
		},
	}
	if err := f.next(); err != nil {
		t.Fatal(err)
	}
	if err := f.Send(1); err != nil {
		t.Fatal(err)
	}
	if err := f.Send(2); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Receive(); err != nil || v != 2 {
		t.Fatalf("got %d, %v", v, err)
	}
	if f.Backend().Name != "good" {
		t.Fatalf("backend %s", f.Backend().Name)
	}
}

// A damaged image must not disable the stream or consume its backends.
func TestCorruptInputKeepsStream(t *testing.T) {
	src := testFrame(64, 48, 0)
	good, err := New(Options{}).EncodeJPEG(src, 90)
	if err != nil {
		t.Fatal(err)
	}
	defer good.Release()
	garbage := Packet{Data: []byte("this is not a jpeg image at all")}
	for _, info := range New(Options{}).Backends(JPEG, Decode) {
		t.Run(info.Name, func(t *testing.T) {
			d, err := New(Options{Backend: info.Name}).NewVideoDecoder(VideoConfig{Codec: JPEG})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Send(garbage); err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("garbage: %v", err)
			}
			if err := d.Send(good.Packet); err != nil {
				t.Fatalf("valid image after garbage: %v", err)
			}
			if err := d.Flush(); err != nil {
				t.Fatal(err)
			}
			f, err := d.Receive()
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			f.Release()
		})
	}
	// With automatic selection, garbage alone reports corruption.
	if _, err := New(Options{}).DecodeJPEG(garbage.Data); !errors.Is(err, ErrCorrupt) {
		t.Errorf("DecodeJPEG(garbage) = %v, want ErrCorrupt", err)
	}
}

func TestOpusConcealsLostPacket(t *testing.T) {
	for _, b := range New(Options{}).Backends(Opus, Decode) {
		t.Run(b.Name, func(t *testing.T) { opusConceal(t, New(Options{Backend: b.Name})) })
	}
}

func opusConceal(t *testing.T, l *Library) {
	d, err := l.NewAudioDecoder(AudioConfig{Codec: Opus})
	if err != nil {
		t.Skip(err)
	}
	defer d.Close()
	if err := d.Send(Packet{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	f, err := d.Receive()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if len(f.Samples) == 0 || len(f.Samples) > 960*2 {
		t.Fatalf("got %d samples, want up to %d", len(f.Samples), 960*2)
	}
}

// rateStream records the bitrate it was opened with and fails on Send when
// told to; a nil setter makes it a backend without live bitrate changes.
type rateStream struct {
	fakeStream
	opened int
	setter bool
	set    int
}

func (r *rateStream) SetBitrate(bps int) error {
	if !r.setter {
		return errBitrateUnsupported(r.name)
	}
	r.set = bps
	return nil
}

func TestFailoverKeepsBitrate(t *testing.T) {
	bitrate := 1000
	var opened []*rateStream
	f := &failover[int, int]{
		backends: []backend{{BackendInfo: BackendInfo{Name: "bad"}}, {BackendInfo: BackendInfo{Name: "good"}}},
		clone:    func(i int) int { return i }, nb: &NoBackendError{}, limit: replayLimit{items: 4},
		open: func(b backend) (Stream[int, int], error) {
			s := &rateStream{fakeStream: fakeStream{name: b.Name, fail: b.Name == "bad"}, opened: bitrate, setter: b.Name == "bad"}
			opened = append(opened, s)
			return s, nil
		},
		onBitrate: func(bps int) { bitrate = bps },
	}
	if err := f.next(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetBitrate(0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SetBitrate(0): %v", err)
	}
	if err := f.SetBitrate(500); err != nil || opened[0].set != 500 {
		t.Fatalf("SetBitrate: %v, backend got %d", err, opened[0].set)
	}
	if err := f.Send(1); err != nil {
		t.Fatal(err)
	}
	if f.Backend().Name != "good" || opened[1].opened != 500 {
		t.Fatalf("replacement %s opened at %d, want good at 500", f.Backend().Name, opened[1].opened)
	}
	if err := f.SetBitrate(250); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("unsupported backend: %v", err)
	}
	if bitrate != 250 {
		t.Fatalf("failover did not remember %d", bitrate)
	}
}
