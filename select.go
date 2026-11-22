package gaal

import (
	"errors"
	"fmt"
	"io"
)

// backend is one way of providing one codec in one direction. Exactly one
// open function is set, matching the codec kind and direction.
type backend struct {
	BackendInfo
	avail func(*Library) error // nil means always available
	vdec  func(*Library, VideoConfig) (VideoDecoder, error)
	venc  func(*Library, VideoConfig) (VideoEncoder, error)
	adec  func(*Library, AudioConfig) (AudioDecoder, error)
	aenc  func(*Library, AudioConfig) (AudioEncoder, error)
}

// errHWFallback is wrapped by the error a decoder returns when its hardware
// failed mid-stream and it continues in software. Pictures up to the next
// keyframe are lost, so it also wraps ErrCorrupt.
var errHWFallback = fmt.Errorf("%w: hardware decoder failed, continuing in software", ErrCorrupt)

func (l *Library) candidates(c Codec, d Direction) ([]backend, *NoBackendError) {
	var out []backend
	nb := &NoBackendError{Codec: c, Dir: d}
	for _, b := range allBackends() {
		if b.Codec != c || b.Dir != d {
			continue
		}
		if l.opts.Backend != "" && l.opts.Backend != b.Name {
			continue
		}
		if (l.opts.Hardware == RequireHardware && !b.Hardware) ||
			(l.opts.Hardware == RequireSoftware && b.Hardware) {
			continue
		}
		if b.avail != nil {
			if err := b.avail(l); err != nil {
				nb.Tried = append(nb.Tried, b.Name+": "+err.Error())
				continue
			}
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		nb.Hint = l.hint(c)
	}
	return out, nb
}

func (l *Library) hint(c Codec) string {
	if c == H264 && !l.opts.AllowDownload && downloadableH264() {
		return "set Options.AllowDownload to fetch OpenH264 from Cisco"
	}
	return ""
}

// Backends lists the usable backends for a codec, best first.
func (l *Library) Backends(c Codec, d Direction) []BackendInfo {
	bs, _ := l.candidates(c, d)
	out := make([]BackendInfo, len(bs))
	for i, b := range bs {
		out[i] = b.BackendInfo
	}
	return out
}

func checkVideo(cfg VideoConfig) error {
	switch cfg.Codec {
	case H264, H265, VP8, VP9, JPEG:
		return nil
	}
	return fmt.Errorf("%w: %s is not a video codec", ErrInvalid, cfg.Codec)
}

func clonePacket(p Packet) Packet {
	p.Data = append([]byte(nil), p.Data...)
	return p
}

func cloneFrame(f *VideoFrame) *VideoFrame {
	c := NewVideoFrame(f.Width, f.Height, f.Format)
	f.CopyTo(c)
	return c
}

// NewVideoDecoder opens the best available decoder for cfg.Codec.
//
// An H.264 decoder never returns a picture it had to guess. Until the first
// IDR picture, and after a reference picture is lost (frame_num skips a value)
// until the next one, Send returns an error matching ErrCorrupt for each
// picture and passes it to no backend, so no picture comes out for it. The
// stream stays usable. Packets without a picture, such as parameter sets, are
// always passed on. Damage that a backend detects in a picture is also
// reported as ErrCorrupt where the backend says so.
//
// JPEG images without Huffman tables, as Motion-JPEG sends them, are decoded
// with the standard tables.
func (l *Library) NewVideoDecoder(cfg VideoConfig) (VideoDecoder, error) {
	if err := checkVideo(cfg); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	d, err := build(l, cfg.Codec, Decode, func(b backend) (VideoDecoder, error) { return b.vdec(l, cfg) }, clonePacket, decoderReplay, packetSize)
	if err != nil {
		return nil, err
	}
	c := &colorDecoder{VideoDecoder: d, tracker: newColorTracker(cfg.Codec)}
	if cfg.Codec == H264 {
		c.gaps = &frameNumGuard{}
	}
	if cfg.LowDelay {
		c.guard = newReorderGuard(cfg.Codec)
	}
	return c, nil
}

// NewVideoEncoder opens the best available encoder for cfg.Codec.
func (l *Library) NewVideoEncoder(cfg VideoConfig) (VideoEncoder, error) {
	if err := checkVideo(cfg); err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, fmt.Errorf("%w: encoder size %dx%d must be positive and even", ErrInvalid, cfg.Width, cfg.Height)
	}
	cfg = cfg.withDefaults()
	s, err := build(l, cfg.Codec, Encode, func(b backend) (VideoEncoder, error) { return b.venc(l, cfg) }, cloneFrame, encoderReplay, nil)
	if err != nil {
		return nil, err
	}
	// Replacement backends open with the latest bitrate.
	f := s.(*failover[*VideoFrame, *EncodedPacket])
	f.onBitrate = func(bps int) { cfg.Bitrate = bps }
	if cfg.Codec == H264 {
		// No encoder gaal configures uses B-frames, but not every one says so.
		// Without bitstream_restriction a decoder may hold each picture back as
		// far as the level allows; browsers do. An SPS that lacks it gets it.
		w := &spsRewriter{rewrite: appendNoReorderSPS}
		f.onOut = func(p *EncodedPacket) *EncodedPacket {
			if len(p.Data) == 0 {
				return p
			}
			if b := w.apply(p.Data); &b[0] != &p.Data[0] || len(b) != len(p.Data) {
				p.store = append(p.store[:0], b...)
				p.Data = p.store
			}
			return p
		}
	}
	return s, nil
}

func checkAudio(cfg AudioConfig) error {
	switch cfg.Codec {
	case Opus:
		switch cfg.SampleRate {
		case 8000, 12000, 16000, 24000, 48000:
		default:
			return fmt.Errorf("%w: opus sample rate %d", ErrInvalid, cfg.SampleRate)
		}
		if cfg.Channels != 1 && cfg.Channels != 2 {
			return fmt.Errorf("%w: %d channels", ErrInvalid, cfg.Channels)
		}
		return nil
	case AAC, MP3, FLAC, Vorbis, ALAC, AC3, PCMU, PCMA:
		if cfg.SampleRate < 8000 || cfg.SampleRate > 192000 || cfg.Channels < 1 || cfg.Channels > 8 {
			return fmt.Errorf("%w: %s at %d Hz with %d channels", ErrInvalid, cfg.Codec, cfg.SampleRate, cfg.Channels)
		}
		return nil
	}
	return fmt.Errorf("%w: %s is not an audio codec", ErrInvalid, cfg.Codec)
}

// NewAudioDecoder opens the best available decoder for cfg.Codec.
func (l *Library) NewAudioDecoder(cfg AudioConfig) (AudioDecoder, error) {
	cfg = cfg.withDefaults()
	if err := checkAudio(cfg); err != nil {
		return nil, err
	}
	return build(l, cfg.Codec, Decode, func(b backend) (AudioDecoder, error) { return b.adec(l, cfg) }, clonePacket, decoderReplay, packetSize)
}

// NewAudioEncoder opens the best available encoder for cfg.Codec.
func (l *Library) NewAudioEncoder(cfg AudioConfig) (AudioEncoder, error) {
	cfg = cfg.withDefaults()
	if err := checkAudio(cfg); err != nil {
		return nil, err
	}
	return build(l, cfg.Codec, Encode, func(b backend) (AudioEncoder, error) { return b.aenc(l, cfg) },
		func(f *AudioFrame) *AudioFrame {
			return &AudioFrame{Samples: append([]int16(nil), f.Samples...), SampleRate: f.SampleRate, Channels: f.Channels, PTS: f.PTS}
		}, audioReplay, nil)
}

// oneShot sends one input and returns the first output.
func oneShot[In, Out any](s Stream[In, Out], in In) (Out, error) {
	var zero Out
	if err := s.Send(in); err != nil {
		return zero, err
	}
	if err := s.Flush(); err != nil {
		return zero, err
	}
	out, err := s.Receive()
	if err == io.EOF {
		return zero, errors.New("gaal: codec produced no output")
	}
	return out, err
}

// DecodeJPEG decodes one image. The caller must Release the frame. It reuses
// the decoders of earlier calls, so calling it for every image of a stream
// costs about what a VideoDecoder does. Like every JPEG decoder of this
// package, it accepts images without Huffman tables, as Motion-JPEG sends
// them, and uses the standard ones (see AppendCompleteJPEG).
func (l *Library) DecodeJPEG(data []byte) (*VideoFrame, error) {
	var d VideoDecoder
	select {
	case d = <-l.jpegs:
	default:
		var err error
		if d, err = l.NewVideoDecoder(VideoConfig{Codec: JPEG}); err != nil {
			return nil, err
		}
	}
	f, reuse, err := decodeImage(d, data)
	if !reuse {
		_ = d.Close()
		return f, err
	}
	select {
	case l.jpegs <- d:
	default:
		_ = d.Close()
	}
	return f, err
}

// decodeImage decodes one JPEG image with d, and says whether d can decode
// another: not after an error, nor after a backend that held the picture back
// had to be flushed for it.
func decodeImage(d VideoDecoder, data []byte) (*VideoFrame, bool, error) {
	if err := d.Send(Packet{Data: data, Keyframe: true}); err != nil {
		return nil, false, err
	}
	f, err := d.Receive()
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, ErrAgain) {
		return nil, false, err
	}
	if err := d.Flush(); err != nil {
		return nil, false, err
	}
	f, err = d.Receive()
	if err == io.EOF {
		err = errors.New("gaal: codec produced no output")
	}
	return f, false, err
}

// EncodeJPEG encodes one image of even size. Quality is 1-100, and 0 selects
// the default. The caller must Release the packet.
func (l *Library) EncodeJPEG(f *VideoFrame, quality int) (*EncodedPacket, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	e, err := l.NewVideoEncoder(VideoConfig{Codec: JPEG, Width: f.Width, Height: f.Height, Quality: quality})
	if err != nil {
		return nil, err
	}
	defer e.Close()
	return oneShot(e, f)
}

// replayLimit bounds the inputs kept for failover before the first output.
// Exceeding it turns failover off for the stream. Raw video is large, so
// encoders keep only a few frames. Zero bytes means no byte limit.
type replayLimit struct{ items, bytes int }

var (
	decoderReplay = replayLimit{items: 64, bytes: 8 << 20}
	encoderReplay = replayLimit{items: 3}
	audioReplay   = replayLimit{items: 16}
)

func packetSize(p Packet) int { return len(p.Data) }

var errNoOutput = errors.New("no output produced")

// failover runs the first working backend. Hardware codecs often fail only
// once they see data, so inputs are copied until the first output and replayed
// into the next backend on failure. Once a stream produces output this type
// adds no copies or allocations.
type failover[In, Out any] struct {
	backends []backend
	open     func(backend) (Stream[In, Out], error)
	clone    func(In) In
	size     func(In) int // nil when limit.bytes is zero
	limit    replayLimit
	nb       *NoBackendError
	cur      Stream[In, Out]
	replay   []In
	produced bool
	flushed  bool
	closed   bool

	replayBytes int
	inputErr    error // last ErrCorrupt or ErrInvalid

	onBitrate func(int)     // video encoders: records a bitrate change
	onOut     func(Out) Out // edits each output, when set
}

func build[In, Out any](l *Library, c Codec, d Direction, open func(backend) (Stream[In, Out], error), clone func(In) In, limit replayLimit, size func(In) int) (Stream[In, Out], error) {
	bs, nb := l.candidates(c, d)
	if len(bs) == 0 {
		return nil, nb
	}
	f := &failover[In, Out]{backends: bs, open: open, clone: clone, size: size, limit: limit, nb: nb}
	if err := f.next(); err != nil {
		return nil, err
	}
	return f, nil
}

// next opens the next backend that starts successfully.
func (f *failover[In, Out]) next() error {
	if f.cur != nil {
		f.cur.Close()
		f.cur = nil
	}
	for len(f.backends) > 0 {
		b := f.backends[0]
		f.backends = f.backends[1:]
		s, err := f.open(b)
		if err != nil {
			f.note(b.Name, err)
			continue
		}
		f.cur = s
		return nil
	}
	return f.nb
}

func (f *failover[In, Out]) note(name string, err error) {
	f.nb.Tried = append(f.nb.Tried, name+": "+err.Error())
}

// recover moves to the next backend and replays retained input into it.
func (f *failover[In, Out]) recover(cause error) error {
	f.note(f.cur.Backend().Name, cause)
	for {
		if err := f.next(); err != nil {
			return err
		}
		err := f.feed()
		if err == nil {
			return nil
		}
		f.note(f.cur.Backend().Name, err)
	}
}

// isInputError reports a problem with the data, not with the backend.
func isInputError(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, ErrInvalid)
}

func (f *failover[In, Out]) feed() error {
	kept := f.replay[:0]
	for i, in := range f.replay {
		err := f.cur.Send(in)
		if err == nil {
			kept = append(kept, in)
			continue
		}
		if isInputError(err) {
			f.inputErr = err
			continue
		}
		f.replay = append(kept, f.replay[i:]...)
		return err
	}
	f.replay = kept
	if f.flushed {
		return f.cur.Flush()
	}
	return nil
}

func (f *failover[In, Out]) ready() error {
	if f.closed {
		return ErrClosed
	}
	if f.cur == nil {
		return f.nb
	}
	return nil
}

// settle ends failover once the stream is known to work.
func (f *failover[In, Out]) settle() {
	f.produced, f.replay, f.backends = true, nil, nil
}

func (f *failover[In, Out]) Send(in In) error {
	if err := f.ready(); err != nil {
		return err
	}
	retained, size := false, 0
	if !f.produced && len(f.backends) > 0 {
		if f.size != nil {
			size = f.size(in)
		}
		if len(f.replay) < f.limit.items && (f.limit.bytes == 0 || f.replayBytes+size <= f.limit.bytes) {
			f.replay = append(f.replay, f.clone(in))
			f.replayBytes += size
			retained = true
		} else {
			f.replay, f.backends = nil, nil
		}
	}
	err := f.cur.Send(in)
	if err == nil {
		return nil
	}
	if isInputError(err) {
		// Bad data would fail on every backend, so keep this one.
		if retained {
			f.replay = f.replay[:len(f.replay)-1]
			f.replayBytes -= size
		}
		f.inputErr = err
		return err
	}
	if !retained {
		return err
	}
	return f.recover(err)
}

func (f *failover[In, Out]) Receive() (Out, error) {
	var zero Out
	if err := f.ready(); err != nil {
		return zero, err
	}
	for {
		out, err := f.cur.Receive()
		switch {
		case err == nil:
			if !f.produced {
				f.settle()
			}
			if f.onOut != nil {
				out = f.onOut(out)
			}
			return out, nil
		case err == io.EOF && !f.produced && len(f.replay) == 0 && f.inputErr != nil:
			return zero, f.inputErr
		case err == ErrAgain, f.produced, len(f.backends) == 0, isInputError(err):
			return zero, err
		case err == io.EOF:
			if len(f.replay) == 0 {
				return zero, err
			}
			err = errNoOutput
		}
		if rerr := f.recover(err); rerr != nil {
			return zero, rerr
		}
	}
}

func (f *failover[In, Out]) Flush() error {
	if err := f.ready(); err != nil {
		return err
	}
	f.flushed = true
	err := f.cur.Flush()
	if err == nil || f.produced || len(f.backends) == 0 {
		return err
	}
	return f.recover(err)
}

func (f *failover[In, Out]) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if f.cur == nil {
		return nil
	}
	return f.cur.Close()
}

func (f *failover[In, Out]) Backend() BackendInfo {
	if f.cur == nil {
		return BackendInfo{Codec: f.nb.Codec, Dir: f.nb.Dir}
	}
	return f.cur.Backend()
}
