package gaal

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// benchClip builds moving, textured frames, so encoders do real work.
func benchClip(w, h, n int) []*VideoFrame {
	rng := rand.New(rand.NewSource(7))
	const tile = 256
	noise := make([]byte, tile*tile)
	rng.Read(noise)
	smooth := make([]byte, tile*tile)
	for y := 0; y < tile; y++ {
		for x := 0; x < tile; x++ {
			s := 0
			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 4; dx++ {
					s += int(noise[(y+dy)%tile*tile+(x+dx)%tile])
				}
			}
			smooth[y*tile+x] = byte(s / 16)
		}
	}
	clip := make([]*VideoFrame, n)
	for i := range clip {
		f := NewVideoFrame(w, h, I420)
		f.PTS = time.Duration(i) * time.Second / 60
		for y := 0; y < h; y++ {
			row := f.Y[y*f.YStride : y*f.YStride+w]
			for x := range row {
				t := smooth[((y+i)&(tile-1))*tile+((x+3*i)&(tile-1))]
				row[x] = byte(32 + (x+y+4*i)*90/(w+h+240) + int(t)/3)
			}
		}
		for y := 0; y < h/2; y++ {
			for x := 0; x < w/2; x++ {
				f.U[y*f.UVStride+x] = byte(96 + (x+i)*64/w)
				f.V[y*f.UVStride+x] = byte(110 + (y+i)*64/h)
			}
		}
		clip[i] = f
	}
	return clip
}

// BenchmarkMatrix measures paced, one-frame-at-a-time latency at 1080p.
func BenchmarkMatrix(b *testing.B) {
	const w, h, fps = 1920, 1080, 60
	clip := benchClip(w, h, 30)
	for _, tc := range []struct {
		codec    Codec
		enc, dec string
	}{
		{H264, "nvenc", "nvdec"}, {H265, "nvenc", "nvdec"}, {H264, "vaapi", "vaapi"}, {H265, "vaapi", "vaapi"},
		{H264, "openh264", "openh264"}, {VP8, "libvpx", "libvpx"}, {VP9, "libvpx", "libvpx"},
		{VP8, "libvpx", "nvdec"}, {VP9, "libvpx", "nvdec"}, {VP9, "libvpx", "vaapi"},
		{JPEG, "turbojpeg", "turbojpeg"}, {JPEG, "turbojpeg", "nvdec"}, {JPEG, "turbojpeg", "vaapi"},
		{H264, "videotoolbox", "videotoolbox"}, {H265, "videotoolbox", "videotoolbox"}, {JPEG, "videotoolbox", "videotoolbox"},
		{VP9, "libvpx", "videotoolbox"},
		{H264, "mediacodec", "mediacodec"}, {H265, "mediacodec", "mediacodec"}, {VP8, "mediacodec", "mediacodec"}, {VP9, "mediacodec", "mediacodec"},
	} {
		b.Run(fmt.Sprintf("%s/%s/%s", tc.codec, tc.enc, tc.dec), func(b *testing.B) {
			cfg := VideoConfig{Codec: tc.codec, Width: w, Height: h, FPS: fps, Bitrate: 8_000_000, KeyframeEvery: 120}
			enc, err := New(Options{Backend: tc.enc}).NewVideoEncoder(cfg)
			if err != nil {
				b.Skip(err)
			}
			defer enc.Close()
			dec, err := New(Options{Backend: tc.dec}).NewVideoDecoder(cfg)
			if err != nil {
				b.Skip(err)
			}
			defer dec.Close()
			times := make([]time.Duration, 0, 4096)
			var encTime, decTime time.Duration
			bytes, i := 0, 0
			b.ReportAllocs()
			for b.Loop() {
				src := clip[i%len(clip)]
				src.PTS = time.Duration(i) * time.Second / fps
				i++
				start := time.Now()
				if err := enc.Send(src); err != nil {
					b.Fatal(err)
				}
				p, err := ReceiveWait(enc, 3*time.Second)
				if err != nil {
					b.Fatal("encoder: ", err)
				}
				encoded := time.Now()
				bytes += len(p.Data)
				if err := dec.Send(p.Packet); err != nil {
					b.Fatal(err)
				}
				p.Release()
				f, err := ReceiveWait(dec, 3*time.Second)
				if err != nil {
					b.Fatal("decoder: ", err)
				}
				f.Release()
				end := time.Now()
				encTime += encoded.Sub(start)
				decTime += end.Sub(encoded)
				if len(times) < cap(times) {
					times = append(times, end.Sub(start))
				}
			}
			slices.Sort(times)
			n := float64(i)
			b.ReportMetric(float64(encTime.Microseconds())/n, "enc-us")
			b.ReportMetric(float64(decTime.Microseconds())/n, "dec-us")
			b.ReportMetric(float64(times[len(times)/2].Microseconds()), "p50-us")
			b.ReportMetric(float64(times[len(times)*99/100].Microseconds()), "p99-us")
			b.ReportMetric(float64(bytes)*8*fps/n/1e6, "Mbit/s")
		})
	}
}
