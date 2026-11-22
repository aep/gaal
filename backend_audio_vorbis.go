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

// Vorbis through libvorbis and libvorbisenc, if they are installed. Only the
// packet API is used: no Ogg framing is involved. The C structs live inside the
// Go objects, which never move, with sizes for LP64 systems.

const (
	vorbisDefaultQuality = 0.4
	vorbisInputChunk     = 1024
	vorbisDefaultFrames  = 1024
)

// oggPacket is ogg_packet.
type oggPacket struct {
	packet         uintptr
	bytes          int64
	bos, eos       int64
	granule, numbr int64
}

// vorbisOrder maps WAV channel positions to Vorbis channel positions.
var vorbisOrder = [9][]int{
	1: {0}, 2: {0, 1}, 3: {0, 2, 1}, 4: {0, 1, 2, 3}, 5: {0, 2, 1, 3, 4},
	6: {0, 2, 1, 5, 3, 4}, 7: {0, 2, 1, 6, 5, 3, 4}, 8: {0, 2, 1, 7, 5, 6, 3, 4},
}

type vorbisAPI struct {
	buffer, wrote, blockout, analysis, addBlock, flushPacket uintptr
	synthesis, blockin, pcmout, read, blocksize              uintptr

	infoInit     func(unsafe.Pointer)
	infoClear    func(unsafe.Pointer)
	commentInit  func(unsafe.Pointer)
	commentClear func(unsafe.Pointer)
	dspClear     func(unsafe.Pointer)
	blockInit    func(vd, vb unsafe.Pointer) int32
	blockClear   func(unsafe.Pointer) int32
	analysisInit func(vd, vi unsafe.Pointer) int32
	headerout    func(vd, vc, a, b, c unsafe.Pointer) int32
	synthInit    func(vd, vi unsafe.Pointer) int32
	headerin     func(vi, vc, op unsafe.Pointer) int32
	encInit      func(vi unsafe.Pointer, channels, rate, maxBR, nomBR, minBR int64) int32
	encInitVBR   func(vi unsafe.Pointer, channels, rate int64, quality float32) int32
}

var loadVorbis = sync.OnceValues(func() (*vorbisAPI, error) {
	h, err := dl.Open(libNames("vorbis", "0")...)
	if err != nil {
		return nil, err
	}
	he, err := dl.Open(libNames("vorbisenc", "2")...)
	if err != nil {
		return nil, err
	}
	a := &vorbisAPI{}
	err = dl.BindAll(h, dl.Table{
		"vorbis_analysis_buffer": &a.buffer, "vorbis_analysis_wrote": &a.wrote, "vorbis_analysis_blockout": &a.blockout,
		"vorbis_analysis": &a.analysis, "vorbis_bitrate_addblock": &a.addBlock, "vorbis_bitrate_flushpacket": &a.flushPacket,
		"vorbis_synthesis": &a.synthesis, "vorbis_synthesis_blockin": &a.blockin, "vorbis_synthesis_pcmout": &a.pcmout,
		"vorbis_synthesis_read": &a.read, "vorbis_packet_blocksize": &a.blocksize,
		"vorbis_info_init": &a.infoInit, "vorbis_info_clear": &a.infoClear, "vorbis_comment_init": &a.commentInit,
		"vorbis_comment_clear": &a.commentClear, "vorbis_dsp_clear": &a.dspClear, "vorbis_block_init": &a.blockInit,
		"vorbis_block_clear": &a.blockClear, "vorbis_analysis_init": &a.analysisInit,
		"vorbis_analysis_headerout": &a.headerout, "vorbis_synthesis_init": &a.synthInit,
		"vorbis_synthesis_headerin": &a.headerin,
	})
	if err != nil {
		return nil, err
	}
	return a, dl.BindAll(he, dl.Table{"vorbis_encode_init": &a.encInit, "vorbis_encode_init_vbr": &a.encInitVBR})
})

func vorbisBackends() []backend {
	avail := func(*Library) error { _, err := loadVorbis(); return err }
	dec := BackendInfo{"libvorbis", Vorbis, Decode, false}
	enc := BackendInfo{"libvorbis", Vorbis, Encode, false}
	return []backend{
		{BackendInfo: dec, avail: avail, adec: func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newVorbisDecoder(cfg, dec) }},
		{BackendInfo: enc, avail: avail, aenc: func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newVorbisEncoder(cfg, enc) }},
	}
}

// vorbisState holds the C structs used by both directions.
type vorbisState struct {
	c       dl.Caller
	api     *vorbisAPI
	vi      [7]uint64  // vorbis_info
	vc      [4]uint64  // vorbis_comment
	vd      [18]uint64 // vorbis_dsp_state
	vb      [24]uint64 // vorbis_block
	op      oggPacket
	pcm     uintptr // float** out-parameter
	dspOpen bool
}

func (s *vorbisState) ptrs() (vi, vc, vd, vb unsafe.Pointer) {
	return unsafe.Pointer(&s.vi), unsafe.Pointer(&s.vc), unsafe.Pointer(&s.vd), unsafe.Pointer(&s.vb)
}

func (s *vorbisState) initInfo() {
	vi, vc, _, _ := s.ptrs()
	s.api.infoInit(vi)
	s.api.commentInit(vc)
}

func (s *vorbisState) clear() {
	vi, vc, vd, vb := s.ptrs()
	if s.dspOpen {
		s.api.blockClear(vb)
		s.api.dspClear(vd)
		s.dspOpen = false
	}
	s.api.commentClear(vc)
	s.api.infoClear(vi)
}

// --- decoder ---

type vorbisDecoder struct {
	audioDecBase
	vorbisState
	headers int
}

func newVorbisDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadVorbis()
	if err != nil {
		return nil, err
	}
	d := &vorbisDecoder{}
	d.api = a
	d.initDec(cfg, info, vorbisDefaultFrames)
	d.initInfo()
	if len(cfg.Extra) > 0 {
		parts := xiphUnlace(cfg.Extra)
		if len(parts) != 3 {
			d.clear()
			return nil, fmt.Errorf("%w: vorbis: setup data is not three laced headers", ErrInvalid)
		}
		for _, h := range parts {
			if err := d.header(h); err != nil {
				d.clear()
				return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
			}
		}
	}
	return d, nil
}

func isVorbisHeader(b []byte) bool {
	return len(b) >= 7 && b[0]&1 == 1 && string(b[1:7]) == "vorbis"
}

// header takes the next of the three headers and starts the decoder after the last.
func (d *vorbisDecoder) header(b []byte) error {
	if !isVorbisHeader(b) || d.headers >= 3 {
		return errors.New("vorbis: bad header packet")
	}
	vi, vc, vd, vb := d.ptrs()
	d.op = oggPacket{packet: uintptr(unsafe.Pointer(&b[0])), bytes: int64(len(b))}
	if d.headers == 0 {
		d.op.bos = 1
	}
	r := d.api.headerin(vi, vc, unsafe.Pointer(&d.op))
	runtime.KeepAlive(b)
	if r != 0 {
		return fmt.Errorf("vorbis: bad header packet (%d)", r)
	}
	d.headers++
	if d.headers == 3 {
		if d.api.synthInit(vd, vi) != 0 {
			return errors.New("vorbis: cannot start decoder")
		}
		d.api.blockInit(vd, vb)
		d.dspOpen = true
		d.channels = int(*(*int32)(unsafe.Add(vi, 4)))
		d.rate = int(*(*int64)(unsafe.Add(vi, 8)))
	}
	return nil
}

func (d *vorbisDecoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		if d.dspOpen {
			d.silence(p.PTS)
		}
		return nil
	}
	if !d.dspOpen {
		if err := d.header(p.Data); err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil
	}
	if p.Data[0]&1 == 1 { // a repeated header, or not an audio packet
		if isVorbisHeader(p.Data) {
			return nil
		}
		return fmt.Errorf("%w: vorbis: not an audio packet", ErrCorrupt)
	}
	_, _, vd, vb := d.ptrs()
	d.op = oggPacket{packet: uintptr(unsafe.Pointer(&p.Data[0])), bytes: int64(len(p.Data)), granule: -1}
	r := int32(d.c.C2(d.api.synthesis, uintptr(vb), uintptr(unsafe.Pointer(&d.op))))
	runtime.KeepAlive(p.Data)
	if r != 0 {
		return fmt.Errorf("%w: vorbis: bad packet (%d)", ErrCorrupt, r)
	}
	if r := int32(d.c.C2(d.api.blockin, uintptr(vd), uintptr(vb))); r != 0 {
		return fmt.Errorf("%w: vorbis: bad block (%d)", ErrCorrupt, r)
	}
	n := int(int32(d.c.C2(d.api.pcmout, uintptr(vd), uintptr(unsafe.Pointer(&d.pcm)))))
	if n <= 0 {
		return nil
	}
	ch := d.channels
	f := d.frames.get(n * ch)
	order := vorbisOrder[ch]
	chans := unsafe.Slice((*uintptr)(cPtr(d.pcm)), ch)
	for w := 0; w < ch; w++ {
		src := unsafe.Slice((*float32)(cPtr(chans[order[w]])), n)
		dst := f.Samples[w:]
		for i, v := range src {
			dst[i*ch] = clamp16(v)
		}
	}
	d.c.C2(d.api.read, uintptr(vd), uintptr(n))
	d.push(f, n, p.PTS)
	return nil
}

func (d *vorbisDecoder) Close() error {
	if !d.closed {
		d.closed = true
		d.clear()
		d.dropOutput()
	}
	return nil
}

// cPtr turns an address in C memory into a pointer.
func cPtr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

// --- encoder ---

type vorbisEncoder struct {
	audioEncBase
	vorbisState
	origin   time.Duration
	started  bool
	lastSize int // blocksize of the previous packet
}

func newVorbisEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadVorbis()
	if err != nil {
		return nil, err
	}
	e := &vorbisEncoder{}
	e.api, e.cfg, e.info = a, cfg, info
	e.initInfo()
	vi, vc, vd, vb := e.ptrs()
	var r int32
	if cfg.Bitrate > 0 {
		r = a.encInit(vi, int64(cfg.Channels), int64(cfg.SampleRate), -1, int64(cfg.Bitrate), -1)
	} else {
		r = a.encInitVBR(vi, int64(cfg.Channels), int64(cfg.SampleRate), vorbisDefaultQuality)
	}
	if r != 0 {
		e.clear()
		return nil, fmt.Errorf("%w: vorbis: unsupported configuration (%d)", ErrInvalid, r)
	}
	if a.analysisInit(vd, vi) != 0 {
		e.clear()
		return nil, errors.New("vorbis: cannot start encoder")
	}
	a.blockInit(vd, vb)
	e.dspOpen = true
	var h [3]oggPacket
	if r := a.headerout(vd, vc, unsafe.Pointer(&h[0]), unsafe.Pointer(&h[1]), unsafe.Pointer(&h[2])); r != 0 {
		e.clear()
		return nil, fmt.Errorf("vorbis: cannot write headers (%d)", r)
	}
	var parts [3][]byte
	for i := range h {
		parts[i] = append([]byte(nil), cMem(h[i].packet, int(h[i].bytes))...)
	}
	e.config = xiphLace(parts[:]...)
	e.framer.init(cfg, vorbisInputChunk, e.encode)
	return e, nil
}

// drain runs the analysis and queues every finished packet.
func (e *vorbisEncoder) drain() error {
	_, _, vd, vb := e.ptrs()
	for int32(e.c.C2(e.api.blockout, uintptr(vd), uintptr(vb))) == 1 {
		if r := int32(e.c.C2(e.api.analysis, uintptr(vb), 0)); r != 0 {
			return fmt.Errorf("vorbis: analysis failed (%d)", r)
		}
		if r := int32(e.c.C2(e.api.addBlock, uintptr(vb), 0)); r != 0 {
			return fmt.Errorf("vorbis: bitrate management failed (%d)", r)
		}
		for int32(e.c.C2(e.api.flushPacket, uintptr(vd), uintptr(unsafe.Pointer(&e.op)))) == 1 {
			e.packet()
		}
	}
	return nil
}

// packet stores e.op. Its PTS is the time of the first sample decoding it yields:
// the granule position counts the samples complete after this packet.
func (e *vorbisEncoder) packet() {
	vi, _, _, _ := e.ptrs()
	size := int(int32(e.c.C2(e.api.blocksize, uintptr(vi), uintptr(unsafe.Pointer(&e.op)))))
	frames := 0
	if e.lastSize > 0 && size > 0 {
		frames = (e.lastSize + size) / 4
	}
	e.lastSize = size
	p := e.packets.get(int(e.op.bytes))
	copy(p.Data, cMem(e.op.packet, int(e.op.bytes)))
	p.PTS = e.origin + audioDuration(int(e.op.granule)-frames, e.cfg.SampleRate)
	p.Keyframe = true
	e.out.push(p)
}

func (e *vorbisEncoder) encode(pcm []int16, pts time.Duration) error {
	if !e.started {
		e.started, e.origin = true, pts
	}
	_, _, vd, _ := e.ptrs()
	ch := e.cfg.Channels
	n := len(pcm) / ch
	b := e.c.C2(e.api.buffer, uintptr(vd), uintptr(n))
	if b == 0 {
		return errors.New("vorbis: no analysis buffer")
	}
	chans := unsafe.Slice((*uintptr)(cPtr(b)), ch)
	order := vorbisOrder[ch]
	for w := 0; w < ch; w++ {
		dst := unsafe.Slice((*float32)(cPtr(chans[order[w]])), n)
		src := pcm[w:]
		for i := range dst {
			dst[i] = float32(src[i*ch]) / 32768
		}
	}
	if r := int32(e.c.C2(e.api.wrote, uintptr(vd), uintptr(n))); r != 0 {
		return fmt.Errorf("vorbis: write failed (%d)", r)
	}
	return e.drain()
}

func (e *vorbisEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

// Flush encodes the remaining samples and ends the stream.
func (e *vorbisEncoder) Flush() error {
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
	_, _, vd, _ := e.ptrs()
	if r := int32(e.c.C2(e.api.wrote, uintptr(vd), 0)); r != 0 {
		return fmt.Errorf("vorbis: end of stream failed (%d)", r)
	}
	return e.drain()
}

func (e *vorbisEncoder) Close() error {
	if !e.closed {
		e.closed = true
		e.clear()
		e.dropOutput()
	}
	return nil
}
