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

// MP3 decoding through libmpg123, if it is installed. Packets are checked here
// first, so that damaged data never reaches the resynchronising parser of the
// library, where it would swallow the good frames that follow.

const (
	mpgOK        = 0
	mpgNeedMore  = -10
	mpgNewFormat = -11
	mpgDone      = -12

	mpgParamAddFlags = 2
	mpgFlagQuiet     = 0x20
	mpgFlagGapless   = 0x40
	mpgFlagNoAhead   = 0x400000
	mpgParamRemove   = 13
	mpgEncS16        = 0xD0
	mpgMono          = 1
	mpgStereo        = 2
	mp3MaxFrames     = 1152
)

type mpg123API struct {
	decode, getFormat uintptr

	init     func() int32
	create   func(decoder uintptr, err *int32) uintptr
	del      func(uintptr)
	param    uintptr
	none     func(uintptr) int32
	format   func(h uintptr, rate int64, channels, encodings int32) int32
	openFeed func(uintptr) int32
	plainErr func(int32) string
}

var loadMpg123 = sync.OnceValues(func() (*mpg123API, error) {
	h, err := dl.Open(libNames("mpg123", "0")...)
	if err != nil {
		return nil, err
	}
	a := &mpg123API{}
	err = dl.BindAll(h, dl.Table{
		"mpg123_init": &a.init, "mpg123_new": &a.create, "mpg123_delete": &a.del, "mpg123_param": &a.param,
		"mpg123_format_none": &a.none, "mpg123_format": &a.format, "mpg123_open_feed": &a.openFeed,
		"mpg123_decode": &a.decode, "mpg123_getformat": &a.getFormat, "mpg123_plain_strerror": &a.plainErr,
	})
	if err != nil {
		return nil, err
	}
	a.init()
	return a, nil
})

func mpg123Backends() []backend {
	info := BackendInfo{"libmpg123", MP3, Decode, false}
	return []backend{{BackendInfo: info,
		avail: func(*Library) error { _, err := loadMpg123(); return err },
		adec:  func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newMpg123Decoder(cfg, info) }}}
}

type mpg123Decoder struct {
	audioDecBase
	c        dl.Caller
	api      *mpg123API
	h        uintptr
	done     uintptr
	fmtRate  int64
	fmtChans int32
	fmtEnc   int32
	pts      ptsRing
}

func newMpg123Decoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadMpg123()
	if err != nil {
		return nil, err
	}
	var code int32
	h := a.create(0, &code)
	if h == 0 {
		return nil, fmt.Errorf("mpg123: %s", a.plainErr(code))
	}
	d := &mpg123Decoder{api: a, h: h}
	d.initDec(cfg, info, mp3MaxFrames)
	d.c.C3(a.param, h, mpgParamAddFlags, mpgFlagQuiet|mpgFlagNoAhead)
	d.c.C3(a.param, h, mpgParamRemove, mpgFlagGapless)
	a.none(h)
	for _, r := range []int64{8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000} {
		a.format(h, r, mpgMono|mpgStereo, mpgEncS16)
	}
	if r := a.openFeed(h); r != mpgOK {
		d.Close()
		return nil, fmt.Errorf("mpg123: %s", a.plainErr(r))
	}
	return d, nil
}

// run feeds data, which may be empty, and collects all decoded frames.
func (d *mpg123Decoder) run(data []byte) error {
	var in, size uintptr
	if len(data) > 0 {
		in, size = uintptr(unsafe.Pointer(&data[0])), uintptr(len(data))
	}
	defer runtime.KeepAlive(data)
	for {
		f := d.frames.get(mp3MaxFrames * 2)
		dst := unsafe.Slice((*byte)(unsafe.Pointer(&f.Samples[0])), 2*len(f.Samples))
		d.done = 0
		r := int32(d.c.C6(d.api.decode, d.h, in, size, uintptr(unsafe.Pointer(&dst[0])), uintptr(len(dst)),
			uintptr(unsafe.Pointer(&d.done))))
		in, size = 0, 0
		if r == mpgNewFormat {
			d.c.C5(d.api.getFormat, d.h, uintptr(unsafe.Pointer(&d.fmtRate)), uintptr(unsafe.Pointer(&d.fmtChans)),
				uintptr(unsafe.Pointer(&d.fmtEnc)), 0)
			d.rate, d.channels = int(d.fmtRate), int(d.fmtChans)
		}
		if d.done > 0 {
			d.push(f, int(d.done)/2/d.channels, d.pts.pop())
		} else {
			f.Release()
		}
		switch r {
		case mpgOK, mpgNewFormat:
		case mpgNeedMore, mpgDone:
			return nil
		default:
			return fmt.Errorf("%w: mpg123: %s", ErrCorrupt, d.api.plainErr(r))
		}
	}
}

func (d *mpg123Decoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if d.h == 0 {
		return errors.New("mpg123: decoder closed")
	}
	if len(p.Data) == 0 {
		d.silence(p.PTS)
		return nil
	}
	// Check the whole packet before any of it reaches the library.
	pts, queued := p.PTS, 0
	for rest := p.Data; len(rest) > 0; {
		h, ok := parseMP3(rest)
		if !ok || h.size > len(rest) {
			for ; queued > 0; queued-- {
				d.pts.n--
			}
			return fmt.Errorf("%w: mp3: not a complete frame", ErrCorrupt)
		}
		if !h.isTag(rest[:h.size]) {
			d.pts.push(pts)
			queued++
			pts += audioDuration(h.samples, h.rate)
		}
		rest = rest[h.size:]
	}
	d.pts.step = audioDuration(d.lastFrames, d.rate)
	data := p.Data
	if h, _ := parseMP3(data); h.isTag(data[:h.size]) {
		data = data[h.size:]
	}
	if len(data) == 0 {
		return nil
	}
	return d.run(data)
}

// Flush outputs what the parser still holds.
func (d *mpg123Decoder) Flush() error {
	if d.closed {
		return ErrClosed
	}
	if !d.flushed {
		d.flushed = true
		return d.run(nil)
	}
	return nil
}

func (d *mpg123Decoder) Close() error {
	if !d.closed {
		d.closed = true
		d.api.del(d.h)
		d.h = 0
		d.dropOutput()
	}
	return nil
}
