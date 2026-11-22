//go:build !android && !windows && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// AC-3 decoding through liba52, if it is installed.

const (
	a52Mono     = 1
	a52Stereo   = 2
	a523F       = 3
	a522F2R     = 6
	a523F2R     = 7
	a52ChanMask = 15
	a52LFE      = 16

	a52Blocks      = 6
	a52BlockFrames = 256
)

type a52API struct {
	frame, block, samples uintptr
	init                  func(accel uint32) uintptr
	free                  func(uintptr)
	dynrng                func(state, fn, data uintptr)
}

var loadA52 = sync.OnceValues(func() (*a52API, error) {
	h, err := dl.Open(libNames("a52", "0")...)
	if err != nil {
		return nil, err
	}
	a := &a52API{}
	err = dl.BindAll(h, dl.Table{"a52_init": &a.init, "a52_free": &a.free, "a52_frame": &a.frame,
		"a52_block": &a.block, "a52_samples": &a.samples, "a52_dynrng": &a.dynrng})
	return a, err
})

func a52Backends() []backend {
	info := BackendInfo{"liba52", AC3, Decode, false}
	return []backend{{BackendInfo: info,
		avail: func(*Library) error { _, err := loadA52(); return err },
		adec:  func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newA52Decoder(cfg, info) }}}
}

// a52Layout gives the request flags for a channel count, and for each WAV channel
// the position liba52 stores it at.
func a52Layout(flags int) (order []int) {
	lfe := flags&a52LFE != 0
	var o []int
	switch flags & a52ChanMask {
	case a52Mono:
		o = []int{0}
	case a52Stereo, 0, 10: // stereo, dual mono and Dolby Surround all come as L, R
		o = []int{0, 1}
	case a523F:
		o = []int{0, 2, 1}
	case a522F2R:
		o = []int{0, 1, 2, 3}
	case a523F2R:
		o = []int{0, 2, 1, 3, 4}
	default:
		return nil
	}
	if !lfe {
		return o
	}
	// The LFE channel comes first in the output of liba52, and fourth in WAV order.
	out := make([]int, 0, len(o)+1)
	for i, v := range o {
		if i == 3 {
			out = append(out, 0)
		}
		out = append(out, v+1)
	}
	if len(o) <= 3 {
		out = append(out, 0)
	}
	return out
}

func a52Flags(channels int) int32 {
	switch channels {
	case 1:
		return a52Mono
	case 3:
		return a523F
	case 4:
		return a522F2R
	case 5:
		return a523F2R
	case 6:
		return a523F2R | a52LFE
	}
	return a52Stereo
}

type a52Decoder struct {
	audioDecBase
	c       dl.Caller
	api     *a52API
	h       uintptr
	samples []float32 // the sample buffer of liba52
	thunk   uintptr
	want    int32
	flags   int32
	level   float32
	got     int32
	order   []int
}

func newA52Decoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadA52()
	if err != nil {
		return nil, err
	}
	h := a.init(0)
	if h == 0 {
		return nil, errors.New("a52: cannot create decoder")
	}
	d := &a52Decoder{api: a, h: h, want: a52Flags(cfg.Channels)}
	d.initDec(cfg, info, ac3FrameSamples)
	_, _, _, _, d.thunk = audioThunks()
	a.dynrng(h, 0, 0) // no dynamic range compression
	p := d.c.C1(a.samples, h)
	if p == 0 {
		d.Close()
		return nil, errors.New("a52: no sample buffer")
	}
	d.samples = unsafe.Slice((*float32)(cPtr(p)), a52BlockFrames*a52Blocks)
	return d, nil
}

func (d *a52Decoder) frame(b []byte, size, rate int, f *AudioFrame, offset int) (int, error) {
	if !ac3CRCValid(b[:size]) {
		return 0, fmt.Errorf("%w: ac3: bad checksum", ErrCorrupt)
	}
	d.flags, d.level = d.want, 1
	r := int32(d.c.C5(d.thunk, d.h, uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&d.flags)),
		uintptr(unsafe.Pointer(&d.level)), d.api.frame))
	runtime.KeepAlive(b)
	if r != 0 {
		return 0, fmt.Errorf("%w: ac3: bad frame", ErrCorrupt)
	}
	if d.flags != d.got || d.order == nil {
		order := a52Layout(int(d.flags))
		if order == nil {
			return 0, fmt.Errorf("%w: ac3: unexpected channel layout %d", ErrCorrupt, d.flags)
		}
		d.got, d.order = d.flags, order
	}
	ch := len(d.order)
	d.rate, d.channels = rate, ch
	for blk := 0; blk < a52Blocks; blk++ {
		if int32(d.c.C1(d.api.block, d.h)) != 0 {
			return 0, fmt.Errorf("%w: ac3: bad block", ErrCorrupt)
		}
		dst := f.Samples[offset+blk*a52BlockFrames*ch:]
		for w, pos := range d.order {
			src := d.samples[pos*a52BlockFrames : (pos+1)*a52BlockFrames]
			out := dst[w:]
			for i, v := range src {
				out[i*ch] = clamp16(v)
			}
		}
	}
	return ac3FrameSamples * ch, nil
}

func (d *a52Decoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		d.silence(p.PTS)
		return nil
	}
	count := 0
	for rest := p.Data; len(rest) > 0; count++ {
		size, _, _, ok := parseAC3(rest)
		if !ok || size > len(rest) {
			return fmt.Errorf("%w: ac3: not a complete syncframe", ErrCorrupt)
		}
		rest = rest[size:]
	}
	f := d.frames.get(count * ac3FrameSamples * 6)
	offset, rest := 0, p.Data
	for len(rest) > 0 {
		size, rate, _, _ := parseAC3(rest)
		n, err := d.frame(rest, size, rate, f, offset)
		if err != nil {
			f.Release()
			return err
		}
		offset += n
		rest = rest[size:]
	}
	d.push(f, offset/d.channels, p.PTS)
	return nil
}

func (d *a52Decoder) Close() error {
	if !d.closed {
		d.closed = true
		d.api.free(d.h)
		d.h = 0
		d.dropOutput()
	}
	return nil
}
