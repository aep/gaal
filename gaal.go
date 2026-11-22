// Package gaal encodes and decodes media through the native codecs of the host
// platform, preferring hardware acceleration. It contains no C code.
//
// H.264 and H.265 are never implemented or shipped by this module. They are only reached
// through codecs that are already on the system or, if Options.AllowDownload
// is set, through a binary fetched from its vendor at run time.
//
// # Streaming model
//
// Every codec is a Stream: call Send with input, then call Receive until it
// returns ErrAgain. After Flush, Receive returns the remaining output and then
// io.EOF. Output comes from per-stream pools. Call Release on each output when
// finished with it, so a running stream does not allocate.
package gaal

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Codec identifies a media format.
type Codec int

// Supported codecs.
const (
	H264 Codec = iota
	VP8
	JPEG
	Opus
	H265
	VP9
	AAC
	MP3
	FLAC
	Vorbis
	ALAC
	AC3
	PCMU // G.711 mu-law
	PCMA // G.711 A-law

	// MJPEG is a sequence of JPEG images, one per packet.
	MJPEG = JPEG
)

func (c Codec) String() string {
	switch c {
	case H264:
		return "h264"
	case VP8:
		return "vp8"
	case JPEG:
		return "jpeg"
	case Opus:
		return "opus"
	case H265:
		return "h265"
	case VP9:
		return "vp9"
	case AAC:
		return "aac"
	case MP3:
		return "mp3"
	case FLAC:
		return "flac"
	case Vorbis:
		return "vorbis"
	case ALAC:
		return "alac"
	case AC3:
		return "ac3"
	case PCMU:
		return "pcmu"
	case PCMA:
		return "pcma"
	}
	return fmt.Sprintf("codec(%d)", int(c))
}

// Direction says whether a backend decodes or encodes.
type Direction int

// Directions.
const (
	Decode Direction = iota
	Encode
)

func (d Direction) String() string {
	if d == Encode {
		return "encode"
	}
	return "decode"
}

// HardwarePolicy controls which backends may be selected.
type HardwarePolicy int

// Hardware policies.
const (
	PreferHardware HardwarePolicy = iota // hardware first, then software
	RequireHardware
	RequireSoftware
)

// Options configures a Library.
type Options struct {
	Hardware HardwarePolicy
	// AllowDownload permits fetching a missing codec from its vendor.
	// Nothing is downloaded unless this is true.
	AllowDownload bool
	// CacheDir stores downloaded codecs. Empty means the user cache directory.
	CacheDir string
	// Backend restricts selection to the backend with this name.
	Backend string
}

// Library selects codec backends according to its Options.
type Library struct {
	opts  Options
	jpegs chan VideoDecoder // idle DecodeJPEG decoders
}

// jpegPool is how many idle decoders DecodeJPEG keeps for reuse: opening one
// costs more than decoding an image with it, a hardware one many times more.
const jpegPool = 4

// New returns a Library. It does not load any codec.
func New(opts Options) *Library {
	return &Library{opts: opts, jpegs: make(chan VideoDecoder, jpegPool)}
}

// BackendInfo describes one codec implementation.
type BackendInfo struct {
	Name     string
	Codec    Codec
	Dir      Direction
	Hardware bool
}

// Stream is a codec. It is not safe for concurrent use, but outputs may be
// used and released on any goroutine.
type Stream[In, Out any] interface {
	// Send submits input. The stream does not keep In after Send returns.
	Send(In) error
	// Receive returns the next output, ErrAgain if more input is needed, or
	// io.EOF once a flushed stream is empty. The caller owns the output until
	// it calls Release.
	Receive() (Out, error)
	// Flush ends the input. Only Receive and Close are valid afterwards.
	Flush() error
	Close() error
	// Backend reports the implementation in use. It can change until the
	// first output is produced, because of failover.
	Backend() BackendInfo
}

// Stream types.
type (
	VideoDecoder = Stream[Packet, *VideoFrame]
	VideoEncoder = Stream[*VideoFrame, *EncodedPacket]
	AudioDecoder = Stream[Packet, *AudioFrame]
	AudioEncoder = Stream[*AudioFrame, *EncodedPacket]
)

// Packet is compressed input. H.264 and H.265 use Annex B with one access unit
// per packet. VP8 and VP9 use one frame or superframe per packet. JPEG uses one image per packet. An
// empty Opus packet reports a lost packet, which the decoder conceals.
type Packet struct {
	Data     []byte
	PTS      time.Duration
	Keyframe bool
}

// EncodedPacket is pooled compressed output. Its embedded Packet can be passed
// directly to a decoder.
type EncodedPacket struct {
	Packet
	refs  atomic.Int32
	pool  *packetPool
	store []byte
}

// Retain adds a reference, which needs a matching Release.
func (p *EncodedPacket) Retain() { p.refs.Add(1) }

// Release drops a reference. The packet must not be used after its last one.
func (p *EncodedPacket) Release() {
	if p.refs.Add(-1) == 0 && p.pool != nil {
		p.pool.free.put(p)
	}
}

// PixelFormat is the memory layout of a VideoFrame.
type PixelFormat int

// Pixel formats. Hardware decoders output NV12 without conversion.
const (
	I420 PixelFormat = iota // Y, U and V planes
	NV12                    // Y plane and interleaved UV in U, V is nil
)

// VideoFrame is a raw picture.
type VideoFrame struct {
	Width, Height     int
	Format            PixelFormat
	Y, U, V           []byte
	YStride, UVStride int
	PTS               time.Duration
	// Range is the color range of the samples. Decoders set it from the stream.
	// gaal does not convert samples between ranges.
	Range ColorRange
	// ForceKeyframe asks an encoder to make this frame a keyframe.
	ForceKeyframe bool

	refs  atomic.Int32
	pool  *FramePool
	store []byte
}

func frameSize(w, h int, f PixelFormat) int {
	return w*h + 2*((w+1)/2)*((h+1)/2)
}

func (f *VideoFrame) shape(w, h int, format PixelFormat) {
	cw, ch := (w+1)/2, (h+1)/2
	n := frameSize(w, h, format)
	if cap(f.store) < n {
		f.store = make([]byte, n)
	}
	b := f.store[:n]
	f.Width, f.Height, f.Format, f.YStride = w, h, format, w
	f.PTS, f.ForceKeyframe, f.Range = 0, false, RangeUnspecified
	f.Y = b[: w*h : w*h]
	if format == NV12 {
		f.U, f.V, f.UVStride = b[w*h:], nil, cw*2
		return
	}
	f.U, f.V, f.UVStride = b[w*h:w*h+cw*ch:w*h+cw*ch], b[w*h+cw*ch:], cw
}

// NewVideoFrame allocates a tightly packed frame outside any pool.
func NewVideoFrame(width, height int, format PixelFormat) *VideoFrame {
	f := &VideoFrame{}
	f.shape(width, height, format)
	f.refs.Store(1)
	return f
}

// Retain adds a reference, which needs a matching Release.
func (f *VideoFrame) Retain() { f.refs.Add(1) }

// Release drops a reference. The frame must not be used after its last one.
func (f *VideoFrame) Release() {
	if f.refs.Add(-1) == 0 && f.pool != nil {
		f.pool.free.put(f)
	}
}

// CopyTo copies pixels and timing into dst, which must have the same size and
// format.
func (f *VideoFrame) CopyTo(dst *VideoFrame) error {
	if dst.Width != f.Width || dst.Height != f.Height || dst.Format != f.Format {
		return fmt.Errorf("%w: frame copy between different layouts", ErrInvalid)
	}
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	copyPlane(dst.Y, dst.YStride, f.Y, f.YStride, f.Width, f.Height)
	if f.Format == NV12 {
		copyPlane(dst.U, dst.UVStride, f.U, f.UVStride, cw*2, ch)
	} else {
		copyPlane(dst.U, dst.UVStride, f.U, f.UVStride, cw, ch)
		copyPlane(dst.V, dst.UVStride, f.V, f.UVStride, cw, ch)
	}
	dst.PTS, dst.ForceKeyframe, dst.Range = f.PTS, f.ForceKeyframe, f.Range
	return nil
}

// ToI420 writes this frame into dst, an I420 frame of the same size.
func (f *VideoFrame) ToI420(dst *VideoFrame) error {
	if dst.Format != I420 {
		return fmt.Errorf("%w: destination is not I420", ErrInvalid)
	}
	if f.Format == I420 {
		return f.CopyTo(dst)
	}
	if dst.Width != f.Width || dst.Height != f.Height {
		return fmt.Errorf("%w: frame copy between different sizes", ErrInvalid)
	}
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	copyPlane(dst.Y, dst.YStride, f.Y, f.YStride, f.Width, f.Height)
	for y := 0; y < ch; y++ {
		src := f.U[y*f.UVStride : y*f.UVStride+2*cw]
		u := dst.U[y*dst.UVStride : y*dst.UVStride+cw]
		v := dst.V[y*dst.UVStride : y*dst.UVStride+cw]
		deinterleaveUV(src, u, v)
	}
	dst.PTS, dst.ForceKeyframe, dst.Range = f.PTS, f.ForceKeyframe, f.Range
	return nil
}

func (f *VideoFrame) validate() error {
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	bad := f.Width <= 0 || f.Height <= 0 || f.YStride < f.Width ||
		len(f.Y) < f.YStride*(f.Height-1)+f.Width
	if !bad {
		if f.Format == NV12 {
			bad = f.UVStride < 2*cw || len(f.U) < f.UVStride*(ch-1)+2*cw
		} else {
			bad = f.UVStride < cw || len(f.U) < f.UVStride*(ch-1)+cw || len(f.V) < f.UVStride*(ch-1)+cw
		}
	}
	if bad {
		return fmt.Errorf("%w: malformed %dx%d frame", ErrInvalid, f.Width, f.Height)
	}
	return nil
}

func copyPlane(dst []byte, dstStride int, src []byte, srcStride, width, height int) {
	if dstStride == width && srcStride == width {
		copy(dst[:width*height], src[:width*height])
		return
	}
	for y := 0; y < height; y++ {
		copy(dst[y*dstStride:y*dstStride+width], src[y*srcStride:y*srcStride+width])
	}
}

// AudioFrame is interleaved signed 16-bit PCM.
type AudioFrame struct {
	Samples    []int16
	SampleRate int
	Channels   int
	PTS        time.Duration

	refs atomic.Int32
	pool *audioPool
}

// Retain adds a reference, which needs a matching Release.
func (f *AudioFrame) Retain() { f.refs.Add(1) }

// Release drops a reference. The frame must not be used after its last one.
func (f *AudioFrame) Release() {
	if f.refs.Add(-1) == 0 && f.pool != nil {
		f.pool.free.put(f)
	}
}

// maxPooled bounds the idle objects kept by each pool.
const maxPooled = 32

type freeList[T any] struct {
	mu    sync.Mutex
	items []*T
}

func (l *freeList[T]) get() *T {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.items)
	if n == 0 {
		return nil
	}
	v := l.items[n-1]
	l.items[n-1] = nil
	l.items = l.items[:n-1]
	return v
}

func (l *freeList[T]) put(v *T) {
	l.mu.Lock()
	if l.items == nil {
		l.items = make([]*T, 0, maxPooled)
	}
	if len(l.items) < maxPooled {
		l.items = append(l.items, v)
	}
	l.mu.Unlock()
}

// FramePool recycles video frames. Use it to capture encoder input without
// allocating. It is safe for concurrent use.
type FramePool struct{ free freeList[VideoFrame] }

// Get returns a frame with one reference. Its pixels are not cleared.
func (p *FramePool) Get(width, height int, format PixelFormat) *VideoFrame {
	f := p.free.get()
	if f == nil {
		f = &VideoFrame{pool: p}
	}
	f.shape(width, height, format)
	f.refs.Store(1)
	return f
}

type packetPool struct{ free freeList[EncodedPacket] }

func (p *packetPool) get(size int) *EncodedPacket {
	e := p.free.get()
	if e == nil {
		e = &EncodedPacket{pool: p}
	}
	if cap(e.store) < size {
		e.store = make([]byte, size+size/4)
	}
	e.Packet = Packet{Data: e.store[:size]}
	e.refs.Store(1)
	return e
}

type audioPool struct{ free freeList[AudioFrame] }

func (p *audioPool) get(samples int) *AudioFrame {
	f := p.free.get()
	if f == nil {
		f = &AudioFrame{pool: p}
	}
	if cap(f.Samples) < samples {
		f.Samples = make([]int16, samples)
	}
	f.Samples = f.Samples[:samples]
	f.refs.Store(1)
	return f
}

// fifo is an output queue that reuses its storage.
type fifo[T any] struct {
	items []T
	head  int
}

func (q *fifo[T]) push(v T) { q.items = append(q.items, v) }

func (q *fifo[T]) pop() (v T, ok bool) {
	if q.head == len(q.items) {
		return v, false
	}
	v = q.items[q.head]
	var zero T
	q.items[q.head] = zero
	q.head++
	if q.head == len(q.items) {
		q.items, q.head = q.items[:0], 0
	}
	return v, true
}

// VideoConfig configures a video codec. Decoders only need Codec.
type VideoConfig struct {
	Codec         Codec
	Width, Height int // encoder only, must be even
	FPS           int // encoder only, default 30
	Bitrate       int // encoder only, bits per second, default 2 Mbit/s
	KeyframeEvery int // encoder only, in frames, default 2 seconds
	Quality       int // JPEG and MJPEG encoder only, 1-100, default 85
	// Range is signalled in the encoded stream, encoder only. It describes the
	// frames you send, which are not converted. Pass the Range of the first
	// decoded frame when re-encoding. VP8 cannot signal it and is limited range,
	// JPEG is always full range, and VP9 signals it in each keyframe.
	Range ColorRange
	// LowDelay, for decoders, returns every picture as soon as it is decoded, in
	// decoding order and with the PTS of its own packet, however many frames the
	// stream's parameter sets allow to be reordered. Use it for streams without
	// B-frames, such as most cameras. Should H.264 or H.265 pictures turn out to
	// be reordered, the decoder restores presentation order by holding frames
	// back and sorting them by PTS: pictures that arrive after a later one was
	// returned are dropped, and ReorderReporter counts what happened.
	LowDelay bool
}

func (c VideoConfig) withDefaults() VideoConfig {
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.Bitrate <= 0 {
		c.Bitrate = 2_000_000
	}
	if c.KeyframeEvery <= 0 {
		c.KeyframeEvery = 2 * c.FPS
	}
	if c.Quality <= 0 || c.Quality > 100 {
		c.Quality = 85
	}
	return c
}

// AudioConfig configures an audio codec.
type AudioConfig struct {
	Codec      Codec
	SampleRate int // default 48000, or 8000 for PCMU and PCMA
	Channels   int // default 2, or 1 for PCMU and PCMA
	Bitrate    int // encoder only, bits per second, 0 means codec default
	// Extra is out-of-band codec setup for decoders, when the container carries
	// it: AudioSpecificConfig for AAC, the ALAC magic cookie, or the three Vorbis
	// headers in Xiph lacing.
	Extra []byte
}

func (c AudioConfig) withDefaults() AudioConfig {
	if c.Codec == PCMU || c.Codec == PCMA {
		if c.SampleRate == 0 {
			c.SampleRate = 8000
		}
		if c.Channels == 0 {
			c.Channels = 1
		}
	}
	if c.SampleRate == 0 {
		c.SampleRate = 48000
	}
	if c.Channels == 0 {
		c.Channels = 2
	}
	return c
}

// Errors returned by this package.
var (
	ErrAgain     = errors.New("gaal: more input needed")
	ErrNoBackend = errors.New("gaal: no usable codec backend")
	ErrInvalid   = errors.New("gaal: invalid argument")
	// ErrCorrupt means the input data is damaged. The stream stays usable.
	ErrCorrupt = errors.New("gaal: corrupt input")
	ErrClosed  = errors.New("gaal: codec closed")
)

// NoBackendError explains why a codec could not be opened. It matches
// ErrNoBackend with errors.Is.
type NoBackendError struct {
	Codec Codec
	Dir   Direction
	Tried []string // "backend: reason"
	Hint  string
}

func (e *NoBackendError) Error() string {
	s := fmt.Sprintf("gaal: no usable %s %s backend", e.Codec, e.Dir)
	if len(e.Tried) > 0 {
		s += " (" + strings.Join(e.Tried, "; ") + ")"
	}
	if e.Hint != "" {
		s += ": " + e.Hint
	}
	return s
}

// Is implements errors.Is.
func (e *NoBackendError) Is(target error) bool { return target == ErrNoBackend }

func timeFrom(v uint64) time.Duration { return time.Duration(v) }

// writeNV12 stores f in an NV12 surface. uv is the chroma plane of the surface.
func writeNV12(y []byte, uv []byte, pitch int, f *VideoFrame) {
	copyPlane(y, pitch, f.Y, f.YStride, f.Width, f.Height)
	writeChroma(uv, pitch, f)
}

// writeChroma stores the chroma of f as interleaved UV.
func writeChroma(uv []byte, pitch int, f *VideoFrame) {
	w, h := f.Width, f.Height
	if f.Format == NV12 {
		copyPlane(uv, pitch, f.U, f.UVStride, w, h/2)
		return
	}
	for r := 0; r < h/2; r++ {
		row := uv[r*pitch : r*pitch+w]
		u := f.U[r*f.UVStride : r*f.UVStride+w/2]
		v := f.V[r*f.UVStride : r*f.UVStride+w/2]
		interleaveUV(row, u, v)
	}
}

// vp9Frame is what the uncompressed header of a VP9 frame says. The size and
// color fields are set for keyframes only.
type vp9Frame struct {
	Profile       int
	ShowExisting  bool
	ShowIdx       int   // frame_to_show_map_idx of a ShowExisting frame
	Refresh       uint8 // refresh_frame_flags: the reference slots it replaces
	Key           bool
	Show          bool
	Width, Height int
	BitDepth      int
	ColorSpace    int
	FullRange     bool
	SubX, SubY    int
}

// parseVP9Frame reads the header at the start of b, which is a frame or a
// superframe. It reports false if b is not a valid VP9 frame.
func parseVP9Frame(b []byte) (vp9Frame, bool) {
	var f vp9Frame
	pos, bad := 0, false
	bits := func(n int) int {
		v := 0
		for ; n > 0; n-- {
			if pos>>3 >= len(b) {
				bad = true
				return 0
			}
			v = v<<1 | int(b[pos>>3]>>(7-pos&7))&1
			pos++
		}
		return v
	}
	if bits(2) != 2 {
		return f, false
	}
	low := bits(1)
	f.Profile = bits(1)<<1 | low
	if f.Profile == 3 && bits(1) != 0 {
		return f, false
	}
	if bits(1) == 1 {
		f.ShowExisting = true
		f.ShowIdx = bits(3)
		return f, !bad
	}
	f.Key = bits(1) == 0
	f.Show = bits(1) == 1
	resilient := bits(1) == 1
	if !f.Key {
		intraOnly := !f.Show && bits(1) == 1
		if !resilient {
			bits(2) // reset_frame_context
		}
		if !intraOnly {
			f.Refresh = uint8(bits(8))
			return f, !bad
		}
		if bits(24) != 0x498342 {
			return f, false
		}
		if f.Profile == 0 {
			f.Refresh = uint8(bits(8))
			return f, !bad
		}
	} else {
		f.Refresh = 0xFF
		if bits(24) != 0x498342 {
			return f, false
		}
	}
	f.BitDepth = 8
	if f.Profile >= 2 {
		f.BitDepth = 10 + 2*bits(1)
	}
	f.ColorSpace = bits(3)
	odd := f.Profile&1 == 1
	if f.ColorSpace != 7 { // not RGB
		f.FullRange = bits(1) == 1
		f.SubX, f.SubY = 1, 1
		if odd {
			f.SubX, f.SubY = bits(1), bits(1)
			bits(1)
		}
	} else {
		if !odd {
			return f, false
		}
		f.FullRange = true
		bits(1)
	}
	if !f.Key {
		f.Refresh = uint8(bits(8))
	}
	f.Width, f.Height = bits(16)+1, bits(16)+1
	return f, !bad
}

// vp9Subframes stores the frames of a VP9 packet in out, the frames of a
// superframe or the packet itself, and returns how many there are.
func vp9Subframes(b []byte, out *[8][]byte) int {
	if n := len(b); n > 0 && b[n-1]&0xE0 == 0xC0 {
		m := b[n-1]
		frames, mag := int(m&7)+1, int(m>>3&3)+1
		size := 2 + mag*frames
		if n >= size && b[n-size] == m {
			data, index := b[:n-size], b[n-size+1:n-1]
			for i := 0; i < frames; i++ {
				l := 0
				for k := mag - 1; k >= 0; k-- {
					l = l<<8 | int(index[i*mag+k])
				}
				if l > len(data) {
					return i
				}
				out[i], data = data[:l], data[l:]
			}
			return frames
		}
	}
	out[0] = b
	return 1
}
