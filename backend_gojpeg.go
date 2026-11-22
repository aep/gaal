package gaal

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
)

// The Go standard library is the last-resort JPEG implementation, so JPEG and
// MJPEG work on every platform even without a native codec. Unlike the native
// backends it allocates per image.

func goJPEGBackends() []backend {
	var out []backend
	for _, c := range []Codec{JPEG} {
		dec := BackendInfo{"go-image-jpeg", c, Decode, false}
		enc := BackendInfo{"go-image-jpeg", c, Encode, false}
		out = append(out,
			backend{BackendInfo: dec, vdec: func(*Library, VideoConfig) (VideoDecoder, error) {
				s := &imageDecoder{info: dec}
				s.decode = func(b []byte) (*VideoFrame, error) { return goDecodeJPEG(&s.frames, b) }
				return s, nil
			}},
			backend{BackendInfo: enc, venc: func(_ *Library, cfg VideoConfig) (VideoEncoder, error) {
				s := &imageEncoder{info: enc, cfg: cfg}
				s.encode = func(f *VideoFrame) (*EncodedPacket, error) { return s.goEncode(f) }
				return s, nil
			}})
	}
	return out
}

func goDecodeJPEG(pool *FramePool, data []byte) (*VideoFrame, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	b := img.Bounds()
	f := pool.Get(b.Dx(), b.Dy(), I420)
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	if src, ok := img.(*image.YCbCr); ok && src.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		copyPlane(f.Y, f.YStride, src.Y, src.YStride, f.Width, f.Height)
		copyPlane(f.U, f.UVStride, src.Cb, src.CStride, cw, ch)
		copyPlane(f.V, f.UVStride, src.Cr, src.CStride, cw, ch)
		return f, nil
	}
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			c := color.YCbCrModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.YCbCr)
			f.Y[y*f.YStride+x] = c.Y
			if x%2 == 0 && y%2 == 0 {
				i := (y/2)*f.UVStride + x/2
				f.U[i], f.V[i] = c.Cb, c.Cr
			}
		}
	}
	return f, nil
}

// packetWriter appends into a pooled packet.
type packetWriter struct{ p *EncodedPacket }

func (w packetWriter) Write(b []byte) (int, error) {
	p := w.p
	n := len(p.Data)
	if n+len(b) > cap(p.store) {
		grown := make([]byte, n, 2*(n+len(b)))
		copy(grown, p.Data)
		p.store = grown
	}
	p.Data = append(p.store[:n], b...)
	return len(b), nil
}

func (e *imageEncoder) goEncode(f *VideoFrame) (*EncodedPacket, error) {
	src, err := e.i420(f)
	if err != nil {
		return nil, err
	}
	img := image.YCbCr{
		Y: src.Y, Cb: src.U, Cr: src.V, YStride: src.YStride, CStride: src.UVStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, src.Width, src.Height),
	}
	p := e.packets.get(0)
	if err := jpeg.Encode(packetWriter{p}, &img, &jpeg.Options{Quality: e.cfg.Quality}); err != nil {
		p.Release()
		return nil, err
	}
	return p, nil
}

// imageDecoder adapts a one-image-in, one-image-out decoder.
type imageDecoder struct {
	info    BackendInfo
	frames  FramePool
	out     fifo[*VideoFrame]
	decode  func([]byte) (*VideoFrame, error)
	release func()
	flushed bool
}

func (d *imageDecoder) Send(p Packet) error {
	f, err := d.decode(p.Data)
	if err != nil {
		return err
	}
	f.PTS = p.PTS
	d.out.push(f)
	return nil
}

func (d *imageDecoder) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}
func (d *imageDecoder) Flush() error         { d.flushed = true; return nil }
func (d *imageDecoder) Backend() BackendInfo { return d.info }
func (d *imageDecoder) Close() error {
	if d.release != nil {
		d.release()
		d.release = nil
	}
	return nil
}

func endOfQueue(flushed bool) error {
	if flushed {
		return io.EOF
	}
	return ErrAgain
}

func checkEncoderFrame(f *VideoFrame, cfg VideoConfig) error {
	if err := f.validate(); err != nil {
		return err
	}
	if f.Width != cfg.Width || f.Height != cfg.Height {
		return fmt.Errorf("%w: frame is %dx%d, encoder is %dx%d", ErrInvalid, f.Width, f.Height, cfg.Width, cfg.Height)
	}
	return nil
}

// imageEncoder adapts a one-image-in, one-image-out encoder.
type imageEncoder struct {
	info    BackendInfo
	cfg     VideoConfig
	packets packetPool
	out     fifo[*EncodedPacket]
	scratch *VideoFrame
	encode  func(*VideoFrame) (*EncodedPacket, error)
	release func()
	flushed bool
}

// i420 returns f, or a converted copy in a reused frame.
func (e *imageEncoder) i420(f *VideoFrame) (*VideoFrame, error) {
	if f.Format == I420 {
		return f, nil
	}
	if e.scratch == nil {
		e.scratch = NewVideoFrame(f.Width, f.Height, I420)
	}
	return e.scratch, f.ToI420(e.scratch)
}

func (e *imageEncoder) Send(f *VideoFrame) error {
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	p, err := e.encode(f)
	if err != nil {
		return err
	}
	p.PTS, p.Keyframe = f.PTS, true
	e.out.push(p)
	return nil
}

func (e *imageEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}
func (e *imageEncoder) Flush() error         { e.flushed = true; return nil }
func (e *imageEncoder) Backend() BackendInfo { return e.info }
func (e *imageEncoder) Close() error {
	if e.release != nil {
		e.release()
		e.release = nil
	}
	return nil
}
