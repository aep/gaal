//go:build !android && !windows && (amd64 || arm64)

package gaal

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// MP3 encoding through libmp3lame, if it is installed.

const (
	lameQuality  = 5 // the speed and quality of the LAME default preset
	mp3DecDelay  = 529
	lameOutExtra = 7200
)

type lameAPI struct {
	close, encode, encodeStereo, flush uintptr

	init       func() uintptr
	setRate    func(uintptr, int32) int32
	setOutRate func(uintptr, int32) int32
	setChans   func(uintptr, int32) int32
	setBrate   func(uintptr, int32) int32
	setQuality func(uintptr, int32) int32
	setVBRTag  func(uintptr, int32) int32
	initParams func(uintptr) int32
	delay      func(uintptr) int32
	frameSize  func(uintptr) int32
}

var loadLame = sync.OnceValues(func() (*lameAPI, error) {
	h, err := dl.Open(libNames("mp3lame", "0")...)
	if err != nil {
		return nil, err
	}
	a := &lameAPI{}
	err = dl.BindAll(h, dl.Table{
		"lame_init": &a.init, "lame_close": &a.close, "lame_set_in_samplerate": &a.setRate,
		"lame_set_out_samplerate": &a.setOutRate, "lame_set_num_channels": &a.setChans,
		"lame_set_brate": &a.setBrate, "lame_set_quality": &a.setQuality, "lame_set_bWriteVbrTag": &a.setVBRTag,
		"lame_init_params": &a.initParams, "lame_get_encoder_delay": &a.delay, "lame_get_framesize": &a.frameSize,
		"lame_encode_buffer": &a.encode, "lame_encode_buffer_interleaved": &a.encodeStereo, "lame_encode_flush": &a.flush,
	})
	return a, err
})

func lameBackends() []backend {
	info := BackendInfo{"libmp3lame", MP3, Encode, false}
	return []backend{{BackendInfo: info,
		avail: func(*Library) error { _, err := loadLame(); return err },
		aenc:  func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newLameEncoder(cfg, info) }}}
}

func mp3RateValid(rate int) bool {
	switch rate {
	case 8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000:
		return true
	}
	return false
}

type lameEncoder struct {
	audioEncBase
	c       dl.Caller
	api     *lameAPI
	h       uintptr
	delay   time.Duration
	scratch []byte // output of LAME, which is not aligned to frames
	fill    int
}

func newLameEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadLame()
	if err != nil {
		return nil, err
	}
	if cfg.Channels > 2 || !mp3RateValid(cfg.SampleRate) {
		return nil, fmt.Errorf("%w: mp3 at %d Hz with %d channels", ErrInvalid, cfg.SampleRate, cfg.Channels)
	}
	h := a.init()
	if h == 0 {
		return nil, fmt.Errorf("lame: cannot create encoder")
	}
	e := &lameEncoder{api: a, h: h}
	e.cfg, e.info = cfg, info
	a.setRate(h, int32(cfg.SampleRate))
	a.setOutRate(h, int32(cfg.SampleRate))
	a.setChans(h, int32(cfg.Channels))
	a.setQuality(h, lameQuality)
	a.setVBRTag(h, 0)
	if cfg.Bitrate > 0 {
		a.setBrate(h, int32(cfg.Bitrate/1000))
	}
	if r := a.initParams(h); r < 0 {
		e.Close()
		return nil, fmt.Errorf("%w: lame: unsupported configuration (%d)", ErrInvalid, r)
	}
	frames := int(a.frameSize(h))
	e.delay = audioDuration(int(a.delay(h))+mp3DecDelay, cfg.SampleRate)
	e.times.step = audioDuration(frames, cfg.SampleRate)
	e.scratch = make([]byte, 4*(frames*5/4+lameOutExtra))
	e.framer.init(cfg, frames, e.encode)
	return e, nil
}

// split moves every complete MPEG frame from the scratch buffer to the output.
func (e *lameEncoder) split() error {
	pos := 0
	for {
		rest := e.scratch[pos:e.fill]
		if len(rest) < 4 {
			break
		}
		f, ok := parseMP3(rest)
		if !ok {
			return fmt.Errorf("lame: output is not an MPEG audio frame")
		}
		if f.size > len(rest) {
			break
		}
		p := e.packets.get(f.size)
		copy(p.Data, rest[:f.size])
		p.PTS, p.Keyframe = e.times.pop()-e.delay, true
		e.out.push(p)
		pos += f.size
	}
	e.fill = copy(e.scratch, e.scratch[pos:e.fill])
	return nil
}

func (e *lameEncoder) room() (uintptr, uintptr) {
	return uintptr(unsafe.Pointer(&e.scratch[e.fill])), uintptr(len(e.scratch) - e.fill)
}

func (e *lameEncoder) encode(pcm []int16, pts time.Duration) error {
	e.times.push(pts)
	dst, size := e.room()
	frames := uintptr(len(pcm) / e.cfg.Channels)
	src := uintptr(unsafe.Pointer(&pcm[0]))
	var r uintptr
	if e.cfg.Channels == 2 {
		r = e.c.C5(e.api.encodeStereo, e.h, src, frames, dst, size)
	} else {
		r = e.c.C6(e.api.encode, e.h, src, src, frames, dst, size)
	}
	runtime.KeepAlive(pcm)
	n := int32(r)
	if n < 0 {
		return fmt.Errorf("lame: encode failed (%d)", n)
	}
	e.fill += int(n)
	return e.split()
}

func (e *lameEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

// Flush pads the last frame with silence and drains the encoder.
func (e *lameEncoder) Flush() error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return nil
	}
	e.flushed = true
	if err := e.framer.flush(true); err != nil {
		return err
	}
	dst, size := e.room()
	n := int32(e.c.C3(e.api.flush, e.h, dst, size))
	if n < 0 {
		return fmt.Errorf("lame: flush failed (%d)", n)
	}
	e.fill += int(n)
	return e.split()
}

func (e *lameEncoder) Close() error {
	if !e.closed {
		e.closed = true
		e.c.C1(e.api.close, e.h)
		e.h = 0
		e.dropOutput()
	}
	return nil
}
