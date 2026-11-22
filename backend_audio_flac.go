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

// FLAC through libFLAC, if it is installed. The stream API works through
// callbacks, which are the assembly thunks in audio_thunk_*.s: they run inside
// the libFLAC call, on its stack, and never enter the Go runtime, so a frame costs
// no allocation. They only touch a flacShared.

const (
	flacStateEOS       = 4
	flacStreamInfoSize = 4 + 4 + 34 // "fLaC", block header, STREAMINFO
	flacMaxBlock       = 65535
	flacMaxChannels    = 8
)

// flacShared is read and written by the thunks.
type flacShared struct {
	out, outCap, outLen uint64 // encoder output
	count, overflow     uint32
	ends                [flacMaxEnds]uint32 // end offset of each frame in out

	in, inLeft uint64 // decoder input

	pcm, pcmCap, pcmLen uint64 // decoder output, counted in samples
	blocks              uint32 // frames per channel written
	channels, rate      uint32
	decoded, errors     uint32
	full                uint32
}

const flacMaxEnds = 4

func audioThunks() (encWrite, decRead, decWrite, decError, a52Frame uintptr)

// The thunks are only called from C. These declarations satisfy go vet.
func flacEncWriteThunk()
func flacDecReadThunk()
func flacDecWriteThunk()
func flacDecErrorThunk()
func a52FrameThunk()

type flacAPI struct {
	decProcess, decFlush, decState, encProcess uintptr

	decNew      func() uintptr
	decDelete   func(uintptr)
	decInit     func(d, read, seek, tell, length, eof, write, meta, err, client uintptr) int32
	decFinish   func(uintptr) int32
	decMetadata func(uintptr) int32

	encNew      func() uintptr
	encDelete   func(uintptr)
	encChannels func(uintptr, uint32) int32
	encBits     func(uintptr, uint32) int32
	encRate     func(uintptr, uint32) int32
	encLevel    func(uintptr, uint32) int32
	encSubset   func(uintptr, int32) int32
	encBlock    func(uintptr) uint32
	encInit     func(e, write, seek, tell, meta, client uintptr) int32
	encFinish   func(uintptr) int32
}

var loadFLAC = sync.OnceValues(func() (*flacAPI, error) {
	names := libNames("FLAC", "14")
	for _, v := range []string{"12", "8"} {
		names = append(names, libNames("FLAC", v)...)
	}
	h, err := dl.Open(names...)
	if err != nil {
		return nil, err
	}
	a := &flacAPI{}
	err = dl.BindAll(h, dl.Table{
		"FLAC__stream_decoder_new": &a.decNew, "FLAC__stream_decoder_delete": &a.decDelete,
		"FLAC__stream_decoder_init_stream": &a.decInit, "FLAC__stream_decoder_finish": &a.decFinish,
		"FLAC__stream_decoder_process_until_end_of_metadata": &a.decMetadata,
		"FLAC__stream_decoder_process_single":                &a.decProcess, "FLAC__stream_decoder_flush": &a.decFlush,
		"FLAC__stream_decoder_get_state": &a.decState,
		"FLAC__stream_encoder_new":       &a.encNew, "FLAC__stream_encoder_delete": &a.encDelete,
		"FLAC__stream_encoder_set_channels": &a.encChannels, "FLAC__stream_encoder_set_bits_per_sample": &a.encBits,
		"FLAC__stream_encoder_set_sample_rate": &a.encRate, "FLAC__stream_encoder_set_compression_level": &a.encLevel,
		"FLAC__stream_encoder_set_streamable_subset": &a.encSubset, "FLAC__stream_encoder_get_blocksize": &a.encBlock,
		"FLAC__stream_encoder_init_stream": &a.encInit, "FLAC__stream_encoder_finish": &a.encFinish,
		"FLAC__stream_encoder_process_interleaved": &a.encProcess,
	})
	return a, err
})

func flacBackends() []backend {
	avail := func(*Library) error { _, err := loadFLAC(); return err }
	dec := BackendInfo{"libflac", FLAC, Decode, false}
	enc := BackendInfo{"libflac", FLAC, Encode, false}
	return []backend{
		{BackendInfo: dec, avail: avail, adec: func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newFLACDecoder(cfg, dec) }},
		{BackendInfo: enc, avail: avail, aenc: func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newFLACEncoder(cfg, enc) }},
	}
}

// --- decoder ---

type flacDecoder struct {
	audioDecBase
	c   dl.Caller
	api *flacAPI
	h   uintptr
	s   flacShared
	pcm []int16
}

func newFLACDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadFLAC()
	if err != nil {
		return nil, err
	}
	h := a.decNew()
	if h == 0 {
		return nil, errors.New("flac: cannot create decoder")
	}
	d := &flacDecoder{api: a, h: h}
	d.initDec(cfg, info, flacDefaultBlock)
	d.setBuffer(flacDefaultBlock * cfg.Channels)
	_, rd, wr, er, _ := audioThunks()
	if st := a.decInit(h, rd, 0, 0, 0, 0, wr, 0, er, uintptr(unsafe.Pointer(&d.s))); st != 0 {
		a.decDelete(h)
		return nil, fmt.Errorf("flac: decoder init failed (%d)", st)
	}
	if len(cfg.Extra) > 0 {
		if err := d.header(cfg.Extra); err != nil {
			d.Close()
			return nil, err
		}
	}
	d.c.C1(a.decFlush, h)
	return d, nil
}

func (d *flacDecoder) setBuffer(samples int) {
	d.pcm = make([]int16, samples)
	d.s.pcm, d.s.pcmCap = uint64(uintptr(unsafe.Pointer(&d.pcm[0]))), uint64(samples)
}

func (d *flacDecoder) feed(b []byte) {
	d.s.in, d.s.inLeft = uint64(uintptr(unsafe.Pointer(&b[0]))), uint64(len(b))
}

// header parses setup data in any of the accepted forms.
func (d *flacDecoder) header(extra []byte) error {
	b := flacConfigBlocks(extra)
	if b == nil {
		return fmt.Errorf("%w: flac: unrecognised setup data", ErrInvalid)
	}
	d.feed(b)
	ok := d.api.decMetadata(d.h)
	runtime.KeepAlive(b)
	d.s.inLeft = 0
	if ok == 0 {
		return fmt.Errorf("%w: flac: bad metadata", ErrInvalid)
	}
	d.s.errors = 0
	// The largest block sets the size of the output buffer.
	if len(b) >= flacStreamInfoSize {
		maxBlock := int(b[10])<<8 | int(b[11])
		if maxBlock > 0 && maxBlock*d.channels > len(d.pcm) {
			d.setBuffer(maxBlock * d.channels)
		}
	}
	return nil
}

func (d *flacDecoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		d.silence(p.PTS)
		return nil
	}
	if len(p.Data) >= 4 && string(p.Data[:4]) == "fLaC" {
		d.c.C1(d.api.decFlush, d.h)
		err := d.header(p.Data)
		d.c.C1(d.api.decFlush, d.h)
		return err
	}
	s := &d.s
	s.pcmLen, s.blocks, s.decoded, s.errors, s.full = 0, 0, 0, 0, 0
	d.feed(p.Data)
	for try := 0; try <= len(p.Data); try++ {
		if int32(d.c.C1(d.api.decProcess, d.h)) == 0 || int32(d.c.C1(d.api.decState, d.h)) == flacStateEOS {
			break
		}
	}
	runtime.KeepAlive(p.Data)
	s.inLeft = 0
	d.c.C1(d.api.decFlush, d.h) // leaves the end-of-stream state, ready for the next packet
	if s.full != 0 {
		// A block larger than announced. Make room for the next one.
		d.setBuffer(min(flacMaxBlock*flacMaxChannels, 2*len(d.pcm)+flacDefaultBlock*flacMaxChannels))
		return fmt.Errorf("%w: flac: block larger than expected", ErrCorrupt)
	}
	if s.errors != 0 || s.decoded == 0 {
		return fmt.Errorf("%w: flac: damaged frame", ErrCorrupt)
	}
	d.rate, d.channels = int(s.rate), int(s.channels)
	f := d.frames.get(int(s.pcmLen))
	copy(f.Samples, d.pcm[:s.pcmLen])
	d.push(f, int(s.blocks), p.PTS)
	return nil
}

func (d *flacDecoder) Close() error {
	if !d.closed {
		d.closed = true
		d.api.decFinish(d.h)
		d.api.decDelete(d.h)
		d.h = 0
		d.dropOutput()
	}
	return nil
}

// --- encoder ---

type flacEncoder struct {
	audioEncBase
	c    dl.Caller
	api  *flacAPI
	h    uintptr
	s    flacShared
	buf  []byte
	conv []int32
}

func newFLACEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadFLAC()
	if err != nil {
		return nil, err
	}
	h := a.encNew()
	if h == 0 {
		return nil, errors.New("flac: cannot create encoder")
	}
	e := &flacEncoder{api: a, h: h}
	e.cfg, e.info = cfg, info
	a.encChannels(h, uint32(cfg.Channels))
	a.encBits(h, 16)
	a.encRate(h, uint32(cfg.SampleRate))
	a.encLevel(h, 5)
	e.buf = make([]byte, 1<<16)
	e.s.out, e.s.outCap = uint64(uintptr(unsafe.Pointer(&e.buf[0]))), uint64(len(e.buf))
	write, _, _, _, _ := audioThunks()
	client := uintptr(unsafe.Pointer(&e.s))
	st := a.encInit(h, write, 0, 0, 0, client)
	if st != 0 { // rates outside the streamable subset
		e.s.outLen = 0
		a.encFinish(h)
		a.encSubset(h, 0)
		st = a.encInit(h, write, 0, 0, 0, client)
	}
	if st != 0 {
		e.Close()
		return nil, fmt.Errorf("%w: flac: unsupported configuration (%d)", ErrInvalid, st)
	}
	if e.s.outLen < flacStreamInfoSize {
		e.Close()
		return nil, errors.New("flac: encoder wrote no stream header")
	}
	e.config = append([]byte(nil), e.buf[:flacStreamInfoSize]...)
	e.config[4] |= 0x80 // STREAMINFO is the only block kept
	e.s.outLen, e.s.count = 0, 0
	block := int(a.encBlock(h))
	worst := block*cfg.Channels*2*2 + 1<<12
	if worst > len(e.buf) {
		e.buf = make([]byte, worst)
		e.s.out, e.s.outCap = uint64(uintptr(unsafe.Pointer(&e.buf[0]))), uint64(len(e.buf))
	}
	e.conv = make([]int32, block*cfg.Channels)
	e.times.step = audioDuration(block, cfg.SampleRate)
	e.framer.init(cfg, block, e.encode)
	return e, nil
}

// collect turns what the write thunk stored into packets.
func (e *flacEncoder) collect() error {
	s := &e.s
	if s.overflow != 0 {
		return errors.New("flac: frame larger than the output buffer")
	}
	start := uint32(0)
	frames := min(int(s.count), flacMaxEnds)
	for i := 0; i < frames; i++ {
		end := s.ends[i]
		if i == frames-1 {
			end = uint32(s.outLen) // any frames past the table stay together
		}
		p := e.packets.get(int(end - start))
		copy(p.Data, e.buf[start:end])
		p.PTS, p.Keyframe = e.times.pop(), true
		e.out.push(p)
		start = end
	}
	s.outLen, s.count = 0, 0
	return nil
}

func (e *flacEncoder) encode(pcm []int16, pts time.Duration) error {
	e.times.push(pts)
	conv := e.conv[:len(pcm)]
	for i, v := range pcm {
		conv[i] = int32(v)
	}
	r := e.c.C3(e.api.encProcess, e.h, uintptr(unsafe.Pointer(&conv[0])), uintptr(len(pcm)/e.cfg.Channels))
	if int32(r) == 0 {
		return errors.New("flac: encoder failed")
	}
	return e.collect()
}

func (e *flacEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

// Flush encodes the remaining samples. FLAC is lossless, so nothing is padded.
func (e *flacEncoder) Flush() error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return nil
	}
	e.flushed = true
	if err := e.framer.flush(false); err != nil {
		return err
	}
	e.api.encFinish(e.h)
	return e.collect()
}

func (e *flacEncoder) Close() error {
	if !e.closed {
		e.closed = true
		e.api.encDelete(e.h) // finishes the stream if needed
		e.h = 0
		e.dropOutput()
	}
	return nil
}
