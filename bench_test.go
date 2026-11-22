package gaal

import (
	"testing"
	"time"
)

func BenchmarkWriteNV12From1080pI420(b *testing.B) {
	f := testFrame(1920, 1080, 0)
	dst := make([]byte, 1920*1080*3/2)
	b.SetBytes(int64(len(dst)))
	b.ReportAllocs()
	for b.Loop() {
		writeNV12(dst, dst[1920*1080:], 1920, f)
	}
}

func BenchmarkToI420From1080pNV12(b *testing.B) {
	n := NewVideoFrame(1920, 1080, NV12)
	i := NewVideoFrame(1920, 1080, I420)
	b.SetBytes(1920 * 1080 * 3 / 2)
	b.ReportAllocs()
	for b.Loop() {
		n.ToI420(i)
	}
}

func BenchmarkFramePool(b *testing.B) {
	var p FramePool
	b.ReportAllocs()
	for b.Loop() {
		p.Get(1920, 1080, NV12).Release()
	}
}

func benchTranscode(b *testing.B, codec Codec, encName, decName string, w, h int) {
	cfg := VideoConfig{Codec: codec, Width: w, Height: h, Bitrate: 8_000_000, FPS: 60}
	enc, err := New(Options{Backend: encName}).NewVideoEncoder(cfg)
	if err != nil {
		b.Skip(err)
	}
	defer enc.Close()
	dec, err := New(Options{Backend: decName}).NewVideoDecoder(cfg)
	if err != nil {
		b.Skip(err)
	}
	defer dec.Close()
	src := testFrame(w, h, 0)
	var encoding, decoding time.Duration
	onFrame := func(f *VideoFrame) { f.Release() }
	onPacket := func(p *EncodedPacket) {
		start := time.Now()
		if err := dec.Send(p.Packet); err != nil {
			b.Fatal(err)
		}
		drain(b, dec, onFrame)
		decoding += time.Since(start)
		p.Release()
	}
	b.ReportAllocs()
	for b.Loop() {
		src.PTS += time.Second / 60
		start := time.Now()
		if err := enc.Send(src); err != nil {
			b.Fatal(err)
		}
		encoding += time.Since(start)
		drain(b, enc, onPacket)
	}
	n := float64(b.N)
	b.ReportMetric(float64(encoding.Microseconds())/n, "enc-µs/frame")
	b.ReportMetric(float64(decoding.Microseconds())/n, "dec-µs/frame")
}

func BenchmarkH264NVENCNVDEC1080p(b *testing.B) {
	benchTranscode(b, H264, "nvenc", "nvdec", 1920, 1080)
}
func BenchmarkVP8LibvpxNVDEC1080p(b *testing.B) {
	benchTranscode(b, VP8, "libvpx", "nvdec", 1920, 1080)
}
func BenchmarkJPEGTurboNVDEC1080p(b *testing.B) {
	benchTranscode(b, JPEG, "turbojpeg", "nvdec", 1920, 1080)
}
func BenchmarkJPEGTurbo1080p(b *testing.B) {
	benchTranscode(b, JPEG, "turbojpeg", "turbojpeg", 1920, 1080)
}

func BenchmarkH264MediaCodec1080p(b *testing.B) {
	benchTranscode(b, H264, "mediacodec", "mediacodec", 1920, 1080)
}
func BenchmarkVP8MediaCodec1080p(b *testing.B) {
	benchTranscode(b, VP8, "mediacodec", "mediacodec", 1920, 1080)
}
func BenchmarkH264VideoToolbox1080p(b *testing.B) {
	benchTranscode(b, H264, "videotoolbox", "videotoolbox", 1920, 1080)
}
func BenchmarkJPEGVideoToolbox1080p(b *testing.B) {
	benchTranscode(b, JPEG, "videotoolbox", "videotoolbox", 1920, 1080)
}

// benchLatency paces one frame at a time through encode and decode, so the
// result is the delay a live stream sees rather than queue throughput.
func benchLatency(b *testing.B, codec Codec, encName, decName string) {
	const w, h = 1920, 1080
	cfg := VideoConfig{Codec: codec, Width: w, Height: h, Bitrate: 8_000_000, FPS: 60}
	enc, err := New(Options{Backend: encName}).NewVideoEncoder(cfg)
	if err != nil {
		b.Skip(err)
	}
	defer enc.Close()
	dec, err := New(Options{Backend: decName}).NewVideoDecoder(cfg)
	if err != nil {
		b.Skip(err)
	}
	defer dec.Close()
	src := testFrame(w, h, 0)
	var encTime, decTime, worst time.Duration
	b.ReportAllocs()
	for b.Loop() {
		src.PTS += time.Second / 60
		start := time.Now()
		if err := enc.Send(src); err != nil {
			b.Fatal(err)
		}
		p, err := ReceiveWait(enc, time.Second)
		if err != nil {
			b.Fatal("encoder: ", err)
		}
		encoded := time.Now()
		if err := dec.Send(p.Packet); err != nil {
			b.Fatal(err)
		}
		p.Release()
		f, err := ReceiveWait(dec, time.Second)
		if err != nil {
			b.Fatal("decoder: ", err)
		}
		f.Release()
		end := time.Now()
		encTime += encoded.Sub(start)
		decTime += end.Sub(encoded)
		worst = max(worst, end.Sub(start))
	}
	n := float64(b.N)
	b.ReportMetric(float64(encTime.Microseconds())/n, "enc-µs")
	b.ReportMetric(float64(decTime.Microseconds())/n, "dec-µs")
	b.ReportMetric(float64(worst.Microseconds()), "worst-µs")
}

func BenchmarkLatencyH264NV(b *testing.B)      { benchLatency(b, H264, "nvenc", "nvdec") }
func BenchmarkLatencyH264Android(b *testing.B) { benchLatency(b, H264, "mediacodec", "mediacodec") }
func BenchmarkLatencyVP8Android(b *testing.B)  { benchLatency(b, VP8, "mediacodec", "mediacodec") }
func BenchmarkLatencyH264VT(b *testing.B)      { benchLatency(b, H264, "videotoolbox", "videotoolbox") }

func BenchmarkLatencyH264VAAPI(b *testing.B) { benchLatency(b, H264, "vaapi", "vaapi") }
func BenchmarkLatencyH264OpenH264(b *testing.B) {
	benchLatency(b, H264, "openh264", "openh264")
}
func BenchmarkLatencyVP8Libvpx(b *testing.B) { benchLatency(b, VP8, "libvpx", "libvpx") }
