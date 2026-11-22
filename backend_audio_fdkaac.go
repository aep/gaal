//go:build !android && !windows && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// AAC through libfdk-aac, if it is installed. gaal neither ships nor links this
// library: it is opened at run time like any other system codec. The Fraunhofer
// FDK licence grants no patent rights, so an application that relies on it needs
// its own AAC patent licence where one is required.

const (
	fdkEncOK  = 0
	fdkEncEOF = 0x80

	fdkParamAOT        = 0x100
	fdkParamBitrate    = 0x101
	fdkParamSampleRate = 0x103
	fdkParamChanMode   = 0x106
	fdkParamChanOrder  = 0x107
	fdkParamAfterburn  = 0x200
	fdkParamTransmux   = 0x300
	fdkParamSignaling  = 0x302

	fdkBufInAudio  = 0
	fdkBufOutBytes = 3

	fdkDecOK         = 0
	fdkDecNotEnough  = 0x1002
	fdkDecConceal    = 1
	fdkDecParamCMap  = 0x3
	fdkDecParamLimit = 0x4
	fdkDecParamCncl  = 0x100
	fdkConcealNoise  = 1
	fdkMaxFrame      = 2048
	fdkMaxChannels   = 8
	fdkInfoSize      = 96
	fdkInfoMaxOut    = 0
	fdkInfoFrameLen  = 16
	fdkInfoDelay     = 20
	fdkInfoConf      = 28
	fdkInfoConfSize  = 92
	fdkStreamRate    = 0
	fdkStreamFrame   = 4
	fdkStreamChans   = 8
	fdkDecodeErrLow  = 0x4000
	fdkDecodeErrHigh = 0x4fff
)

type fdkAPI struct {
	encOpen, encClose, encSet, encEncode, encInfo          uintptr
	decOpen, decClose, decConfig, decSet, decFill, decCode uintptr
	decInfo                                                uintptr
}

var loadFDK = sync.OnceValues(func() (*fdkAPI, error) {
	h, err := dl.Open(append(libNames("fdk-aac", "2"), libNames("fdk-aac", "1")...)...)
	if err != nil {
		return nil, err
	}
	a := &fdkAPI{}
	err = dl.BindAll(h, dl.Table{
		"aacEncOpen": &a.encOpen, "aacEncClose": &a.encClose, "aacEncoder_SetParam": &a.encSet,
		"aacEncEncode": &a.encEncode, "aacEncInfo": &a.encInfo,
		"aacDecoder_Open": &a.decOpen, "aacDecoder_Close": &a.decClose, "aacDecoder_ConfigRaw": &a.decConfig,
		"aacDecoder_SetParam": &a.decSet, "aacDecoder_Fill": &a.decFill, "aacDecoder_DecodeFrame": &a.decCode,
		"aacDecoder_GetStreamInfo": &a.decInfo,
	})
	return a, err
})

func fdkBackends() []backend {
	avail := func(*Library) error { _, err := loadFDK(); return err }
	dec := BackendInfo{"libfdk-aac", AAC, Decode, false}
	enc := BackendInfo{"libfdk-aac", AAC, Encode, false}
	return []backend{
		{BackendInfo: dec, avail: avail, adec: func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newFDKDecoder(cfg, dec) }},
		{BackendInfo: enc, avail: avail, aenc: func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newFDKEncoder(cfg, enc) }},
	}
}

// --- decoder ---

type fdkDecoder struct {
	audioDecBase
	c      dl.Caller
	api    *fdkAPI
	h      uintptr
	bufPtr uintptr // one-element arrays passed to aacDecoder_Fill
	bufLen uint32
	valid  uint32
	asc    []byte // C must only see heap memory
}

func newFDKDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadFDK()
	if err != nil {
		return nil, err
	}
	d := &fdkDecoder{api: a}
	d.initDec(cfg, info, aacFrameSamples)
	if len(cfg.Extra) > 0 {
		if err := d.open(cfg.Extra); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// open starts the decoder from an AudioSpecificConfig.
func (d *fdkDecoder) open(asc []byte) error {
	a := d.api
	h := d.c.C2(a.decOpen, 0, 1) // TT_MP4_RAW, one layer
	if h == 0 {
		return errors.New("fdk-aac: cannot open decoder")
	}
	d.asc = append(d.asc[:0], asc...)
	d.bufPtr, d.bufLen = uintptr(unsafe.Pointer(&d.asc[0])), uint32(len(d.asc))
	r := uint32(d.c.C3(a.decConfig, h, uintptr(unsafe.Pointer(&d.bufPtr)), uintptr(unsafe.Pointer(&d.bufLen))))
	if r != fdkDecOK {
		d.c.C1(a.decClose, h)
		return fmt.Errorf("%w: fdk-aac: bad AudioSpecificConfig (0x%x)", ErrInvalid, r)
	}
	// The limiter and the default concealment both add a frame of delay.
	d.c.C3(a.decSet, h, fdkDecParamLimit, 0)
	d.c.C3(a.decSet, h, fdkDecParamCncl, fdkConcealNoise)
	d.c.C3(a.decSet, h, fdkDecParamCMap, 1) // WAV channel order
	d.h = h
	return nil
}

func (d *fdkDecoder) decode(pts time.Duration, flags uintptr) (bool, error) {
	f := d.frames.get(fdkMaxFrame * fdkMaxChannels)
	r := uint32(d.c.C5(d.api.decCode, d.h, uintptr(unsafe.Pointer(&f.Samples[0])), uintptr(len(f.Samples)), flags, 0))
	if r == fdkDecNotEnough {
		f.Release()
		return false, nil
	}
	var err error
	if r != fdkDecOK {
		err = fmt.Errorf("%w: fdk-aac: decoder error 0x%x", ErrCorrupt, r)
		if r < fdkDecodeErrLow || r > fdkDecodeErrHigh { // no usable output
			f.Release()
			return false, err
		}
	}
	si := d.c.C1(d.api.decInfo, d.h)
	if si == 0 {
		f.Release()
		return false, errors.New("fdk-aac: no stream info")
	}
	info := cMem(si, 16)
	frames := int(rd32(info, fdkStreamFrame))
	d.rate, d.channels = int(rd32(info, fdkStreamRate)), int(rd32(info, fdkStreamChans))
	if frames <= 0 || d.channels <= 0 || d.channels > fdkMaxChannels {
		f.Release()
		return false, err
	}
	d.push(f, frames, pts)
	return true, err
}

func (d *fdkDecoder) unit(data []byte, pts time.Duration) error {
	d.bufPtr, d.bufLen = uintptr(unsafe.Pointer(&data[0])), uint32(len(data))
	d.valid = d.bufLen
	r := uint32(d.c.C5(d.api.decFill, d.h, uintptr(unsafe.Pointer(&d.bufPtr)), uintptr(unsafe.Pointer(&d.bufLen)),
		uintptr(unsafe.Pointer(&d.valid)), 0))
	runtime.KeepAlive(data)
	if r != fdkDecOK {
		return fmt.Errorf("%w: fdk-aac: fill failed (0x%x)", ErrCorrupt, r)
	}
	got, err := d.decode(pts, 0)
	if err == nil && !got {
		return fmt.Errorf("%w: fdk-aac: incomplete access unit", ErrCorrupt)
	}
	return err
}

func (d *fdkDecoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		if d.h == 0 {
			return nil
		}
		_, err := d.decode(p.PTS, fdkDecConceal)
		return err
	}
	if !adtsChain(p.Data) {
		if d.h == 0 {
			return fmt.Errorf("%w: aac: no AudioSpecificConfig and no ADTS header", ErrCorrupt)
		}
		return d.unit(p.Data, p.PTS)
	}
	data, pts := p.Data, p.PTS
	for len(data) > 0 {
		h, _ := parseADTS(data)
		if d.h == 0 {
			if err := d.open(h.config()); err != nil {
				return err
			}
		}
		if err := d.unit(data[h.headerLen:h.frameLen], pts); err != nil {
			return err
		}
		data = data[h.frameLen:]
		pts += audioDuration(d.lastFrames, d.rate)
	}
	return nil
}

func (d *fdkDecoder) Close() error {
	if !d.closed {
		d.closed = true
		if d.h != 0 {
			d.c.C1(d.api.decClose, d.h)
			d.h = 0
		}
		d.dropOutput()
	}
	return nil
}

// --- encoder ---

type fdkBufDesc struct {
	numBufs             int32
	_                   int32
	bufs, ids, sizes, e uintptr
}

// fdkCall is what one aacEncEncode call reads and writes. It lives on the heap
// inside the encoder, so C can be given pointers to it.
type fdkCall struct {
	in, out                 fdkBufDesc
	inBuf, outBuf           uintptr
	inID, outID             int32
	inSize, outSize         int32
	inEl, outEl             int32
	numIn, numAnc           int32
	outBytes, outIn, outAnc int32
	bitRes                  int32
}

type fdkEncoder struct {
	audioEncBase
	c       dl.Caller
	api     *fdkAPI
	h       uintptr
	call    fdkCall
	maxOut  int
	delay   time.Duration
	handle  uintptr // written by aacEncOpen
	infoBuf [fdkInfoSize]byte
}

func newFDKEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadFDK()
	if err != nil {
		return nil, err
	}
	mode := map[int]uintptr{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 11, 8: 12}[cfg.Channels]
	e := &fdkEncoder{api: a}
	e.cfg, e.info = cfg, info
	if r := uint32(e.c.C3(a.encOpen, uintptr(unsafe.Pointer(&e.handle)), 0, uintptr(cfg.Channels))); r != fdkEncOK {
		return nil, fmt.Errorf("fdk-aac: cannot open encoder (0x%x)", r)
	}
	e.h = e.handle
	set := func(param, v uintptr) error {
		if r := uint32(e.c.C3(a.encSet, e.h, param, v)); r != fdkEncOK {
			return fmt.Errorf("%w: fdk-aac: parameter 0x%x = %d rejected (0x%x)", ErrInvalid, param, v, r)
		}
		return nil
	}
	params := [][2]uintptr{
		{fdkParamAOT, 2}, {fdkParamSampleRate, uintptr(cfg.SampleRate)}, {fdkParamChanMode, mode},
		{fdkParamChanOrder, 1}, {fdkParamTransmux, 0}, {fdkParamAfterburn, 1},
	}
	if cfg.Bitrate > 0 {
		params = append(params, [2]uintptr{fdkParamBitrate, uintptr(cfg.Bitrate)})
	}
	for _, p := range params {
		if err := set(p[0], p[1]); err != nil {
			e.Close()
			return nil, err
		}
	}
	if r := uint32(e.c.C5(a.encEncode, e.h, 0, 0, 0, 0)); r != fdkEncOK {
		e.Close()
		return nil, fmt.Errorf("%w: fdk-aac: unsupported configuration (0x%x)", ErrInvalid, r)
	}
	info2 := e.infoBuf[:]
	if r := uint32(e.c.C2(a.encInfo, e.h, uintptr(unsafe.Pointer(&info2[0])))); r != fdkEncOK {
		e.Close()
		return nil, fmt.Errorf("fdk-aac: encoder info failed (0x%x)", r)
	}
	frames := int(rd32(info2, fdkInfoFrameLen))
	e.maxOut = max(int(rd32(info2, fdkInfoMaxOut)), 768*cfg.Channels)
	e.delay = audioDuration(int(rd32(info2, fdkInfoDelay)), cfg.SampleRate)
	e.config = append([]byte(nil), info2[fdkInfoConf:fdkInfoConf+int(rd32(info2, fdkInfoConfSize))]...)
	e.times.step = audioDuration(frames, cfg.SampleRate)
	e.framer.init(cfg, frames, e.encode)

	c := &e.call
	c.inID, c.outID, c.inEl, c.outEl = fdkBufInAudio, fdkBufOutBytes, 2, 1
	c.in = fdkBufDesc{numBufs: 1, bufs: uintptr(unsafe.Pointer(&c.inBuf)), ids: uintptr(unsafe.Pointer(&c.inID)),
		sizes: uintptr(unsafe.Pointer(&c.inSize)), e: uintptr(unsafe.Pointer(&c.inEl))}
	c.out = fdkBufDesc{numBufs: 1, bufs: uintptr(unsafe.Pointer(&c.outBuf)), ids: uintptr(unsafe.Pointer(&c.outID)),
		sizes: uintptr(unsafe.Pointer(&c.outSize)), e: uintptr(unsafe.Pointer(&c.outEl))}
	return e, nil
}

// run makes one aacEncEncode call. numIn of -1 drains the encoder.
func (e *fdkEncoder) run(pcm []int16, numIn int32) (uint32, error) {
	c := &e.call
	c.numIn = numIn
	c.inBuf, c.inSize = 0, 0
	if len(pcm) > 0 {
		c.inBuf, c.inSize = uintptr(unsafe.Pointer(&pcm[0])), int32(2*len(pcm))
	}
	p := e.packets.get(e.maxOut)
	c.outBuf, c.outSize = uintptr(unsafe.Pointer(&p.Data[0])), int32(len(p.Data))
	c.outBytes = 0
	r := uint32(e.c.C5(e.api.encEncode, e.h, uintptr(unsafe.Pointer(&c.in)), uintptr(unsafe.Pointer(&c.out)),
		uintptr(unsafe.Pointer(&c.numIn)), uintptr(unsafe.Pointer(&c.outBytes))))
	runtime.KeepAlive(pcm)
	if r != fdkEncOK && r != fdkEncEOF {
		p.Release()
		return r, fmt.Errorf("fdk-aac: encode failed (0x%x)", r)
	}
	if c.outBytes <= 0 {
		p.Release()
		return r, nil
	}
	p.Data = p.Data[:c.outBytes]
	p.PTS, p.Keyframe = e.times.pop()-e.delay, true
	e.out.push(p)
	return r, nil
}

func (e *fdkEncoder) encode(pcm []int16, pts time.Duration) error {
	e.times.push(pts)
	_, err := e.run(pcm, int32(len(pcm)))
	return err
}

func (e *fdkEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

// Flush pads the last frame with silence and drains the encoder.
func (e *fdkEncoder) Flush() error {
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
	for {
		r, err := e.run(nil, -1)
		if err != nil {
			return err
		}
		if r == fdkEncEOF || e.call.outBytes <= 0 {
			return nil
		}
	}
}

func (e *fdkEncoder) Close() error {
	if !e.closed {
		e.closed = true
		if e.h != 0 {
			e.handle = e.h
			e.c.C1(e.api.encClose, uintptr(unsafe.Pointer(&e.handle)))
			e.h = 0
		}
		e.dropOutput()
	}
	return nil
}
