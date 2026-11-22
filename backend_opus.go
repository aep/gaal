package gaal

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// libopus, loaded from the system. This is a software codec.

type opusAPI struct {
	decCreate  func(fs, ch int32, err *int32) uintptr
	decDestroy func(uintptr)
	encCreate  func(fs, ch, app int32, err *int32) uintptr
	encDestroy func(uintptr)
	encCtl     uintptr
	strerror   func(int32) string
	decode     uintptr
	encode     uintptr
}

var loadOpus = sync.OnceValues(func() (*opusAPI, error) {
	h, err := dl.Open(libNames("opus", "0")...)
	if err != nil {
		return nil, err
	}
	a := &opusAPI{}
	err = dl.BindAll(h, dl.Table{
		"opus_decoder_create": &a.decCreate, "opus_decoder_destroy": &a.decDestroy, "opus_decode": &a.decode,
		"opus_encoder_create": &a.encCreate, "opus_encoder_destroy": &a.encDestroy, "opus_encode": &a.encode,
		"opus_encoder_ctl": &a.encCtl, "opus_strerror": &a.strerror,
	})
	return a, err
})

var (
	opusDecInfo = BackendInfo{"libopus", Opus, Decode, false}
	opusEncInfo = BackendInfo{"libopus", Opus, Encode, false}
)

func opusBackends() []backend {
	avail := func(*Library) error { _, err := loadOpus(); return err }
	return []backend{
		{BackendInfo: opusDecInfo, avail: avail, adec: newOpusDecoder},
		{BackendInfo: opusEncInfo, avail: avail, aenc: newOpusEncoder},
	}
}

const opusInvalidPacket = -4

type opusDecoder struct {
	c          dl.Caller
	lastFrames int
	api        *opusAPI
	st         uintptr
	cfg        AudioConfig
	frames     audioPool
	out        fifo[*AudioFrame]
	flushed    bool
}

func newOpusDecoder(_ *Library, cfg AudioConfig) (AudioDecoder, error) {
	a, err := loadOpus()
	if err != nil {
		return nil, err
	}
	var code int32
	st := a.decCreate(int32(cfg.SampleRate), int32(cfg.Channels), &code)
	if code != 0 || st == 0 {
		return nil, fmt.Errorf("opus: %s", a.strerror(code))
	}
	return &opusDecoder{api: a, st: st, cfg: cfg}, nil
}

func (d *opusDecoder) Send(p Packet) error {
	maxFrames := d.cfg.SampleRate * 120 / 1000
	f := d.frames.get(maxFrames * d.cfg.Channels)
	var data, size uintptr
	frames := maxFrames
	if len(p.Data) > 0 {
		data, size = uintptr(unsafe.Pointer(&p.Data[0])), uintptr(len(p.Data))
	} else {
		// A lost packet: conceal one frame of the last seen duration.
		frames = d.lastFrames
		if frames == 0 {
			frames = d.cfg.SampleRate / 50
		}
	}
	r := d.c.C6(d.api.decode, d.st, data, size, uintptr(unsafe.Pointer(&f.Samples[0])), uintptr(frames), 0)
	runtime.KeepAlive(p.Data)
	n := int32(r)
	if n < 0 {
		f.Release()
		if n == opusInvalidPacket {
			return fmt.Errorf("%w: opus: %s", ErrCorrupt, d.api.strerror(n))
		}
		return fmt.Errorf("opus: %s", d.api.strerror(n))
	}
	d.lastFrames = int(n)
	f.Samples = f.Samples[:int(n)*d.cfg.Channels]
	f.SampleRate, f.Channels, f.PTS = d.cfg.SampleRate, d.cfg.Channels, p.PTS
	d.out.push(f)
	return nil
}

func (d *opusDecoder) Receive() (*AudioFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}
func (d *opusDecoder) Flush() error         { d.flushed = true; return nil }
func (d *opusDecoder) Backend() BackendInfo { return opusDecInfo }
func (d *opusDecoder) Close() error {
	if d.st != 0 {
		d.api.decDestroy(d.st)
		d.st = 0
	}
	return nil
}

const opusMaxPacket = 4000

type opusEncoder struct {
	c       dl.Caller
	api     *opusAPI
	st      uintptr
	cfg     AudioConfig
	packets packetPool
	out     fifo[*EncodedPacket]
	pending []int16 // holds less than one 20 ms frame between calls
	pts     time.Duration
	flushed bool
}

func newOpusEncoder(_ *Library, cfg AudioConfig) (AudioEncoder, error) {
	a, err := loadOpus()
	if err != nil {
		return nil, err
	}
	const applicationLowDelay, setBitrate = 2051, 4002
	var code int32
	st := a.encCreate(int32(cfg.SampleRate), int32(cfg.Channels), applicationLowDelay, &code)
	if code != 0 || st == 0 {
		return nil, fmt.Errorf("opus: %s", a.strerror(code))
	}
	if cfg.Bitrate > 0 {
		var caller dl.Caller
		if c := int32(caller.Variadic(a.encCtl, st, setBitrate, uintptr(cfg.Bitrate))); c != 0 {
			a.encDestroy(st)
			return nil, fmt.Errorf("opus: set bitrate: %s", a.strerror(c))
		}
	}
	size := cfg.SampleRate / 50 * cfg.Channels
	return &opusEncoder{api: a, st: st, cfg: cfg, pending: make([]int16, 0, size)}, nil
}

func (e *opusEncoder) emit(pcm []int16) error {
	p := e.packets.get(opusMaxPacket)
	r := e.c.C5(e.api.encode, e.st, uintptr(unsafe.Pointer(&pcm[0])), uintptr(len(pcm)/e.cfg.Channels),
		uintptr(unsafe.Pointer(&p.Data[0])), opusMaxPacket)
	runtime.KeepAlive(pcm)
	n := int32(r)
	if n < 0 {
		p.Release()
		return fmt.Errorf("opus: %s", e.api.strerror(n))
	}
	p.Data, p.PTS, p.Keyframe = p.Data[:n], e.pts, true
	e.out.push(p)
	e.pts += 20 * time.Millisecond
	return nil
}

// Send accepts any number of samples and produces 20 ms packets.
func (e *opusEncoder) Send(f *AudioFrame) error {
	if f.SampleRate != e.cfg.SampleRate || f.Channels != e.cfg.Channels || len(f.Samples)%e.cfg.Channels != 0 {
		return fmt.Errorf("%w: audio frame does not match encoder format", ErrInvalid)
	}
	size := cap(e.pending)
	samples := f.Samples
	if len(e.pending) == 0 {
		e.pts = f.PTS
	}
	if len(e.pending) > 0 {
		n := min(size-len(e.pending), len(samples))
		e.pending = append(e.pending, samples[:n]...)
		samples = samples[n:]
		if len(e.pending) < size {
			return nil
		}
		if err := e.emit(e.pending); err != nil {
			return err
		}
		e.pending = e.pending[:0]
	}
	for len(samples) >= size {
		if err := e.emit(samples[:size]); err != nil {
			return err
		}
		samples = samples[size:]
	}
	e.pending = append(e.pending, samples...)
	return nil
}

func (e *opusEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

// Flush pads any partial frame with silence and encodes it.
func (e *opusEncoder) Flush() error {
	e.flushed = true
	if len(e.pending) == 0 {
		return nil
	}
	n := len(e.pending)
	e.pending = e.pending[:cap(e.pending)]
	clear(e.pending[n:])
	err := e.emit(e.pending)
	e.pending = e.pending[:0]
	return err
}
func (e *opusEncoder) Backend() BackendInfo { return opusEncInfo }
func (e *opusEncoder) Close() error {
	if e.st != 0 {
		e.api.encDestroy(e.st)
		e.st = 0
	}
	return nil
}
