//go:build darwin && arm64

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

// AAC, MP3, FLAC, ALAC and AC-3 through the AudioConverter of AudioToolbox. All
// the codecs are part of the operating system. The input callback is the assembly
// thunk in apple_audio_thunk_darwin_arm64.s, so a frame costs no allocation and
// never enters Go from a foreign context.

const (
	atPropDecompCookie = 0x646d6763 // dmgc
	atPropCompCookie   = 0x636d6763 // cmgc
	atPropPrimeMethod  = 0x70726d6d // prmm
	atPropPrimeInfo    = 0x7072696d // prim
	atPropMaxOutPacket = 0x786f7073 // xops
	atPropOutFormat    = 0x61636f64 // acod
	atPropInLayout     = 0x69636c20 // "icl "
	atPropOutLayout    = 0x6f636c20 // "ocl "

	atPrimeNone = 2

	fourccAAC  = 0x61616320 // "aac "
	fourccMP3  = 0x2e6d7033 // ".mp3"
	fourccFLAC = 0x666c6163 // flac
	fourccALAC = 0x616c6163 // alac
	fourccAC3  = 0x61632d33 // ac-3

	atLossless16Bit = 1 // kAppleLosslessFormatFlag_16BitSourceData

	atDefaultMaxPacket = 1 << 17
	atMaxDecodeFrames  = 1 << 16
)

// atShared is read by atInputThunk.
type atShared struct {
	avail, packets, channels, bytes uint32
	data                            uintptr
	desc                            auPacketDesc
	eof                             uint32
}

func atThunk() uintptr

// atInputThunk is only called from C. The declaration satisfies go vet.
func atInputThunk()

type atAPI struct {
	fill uintptr

	create   func(in, out unsafe.Pointer, conv *uintptr) int32
	dispose  func(uintptr) int32
	reset    func(uintptr) int32
	setProp  func(conv uintptr, id, size uint32, p unsafe.Pointer) int32
	getProp  func(conv uintptr, id uint32, size *uint32, p unsafe.Pointer) int32
	propInfo func(conv uintptr, id uint32, size *uint32, writable *uint8) int32
}

var loadAT = sync.OnceValues(func() (*atAPI, error) {
	h, err := dl.Open("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox")
	if err != nil {
		return nil, err
	}
	a := &atAPI{}
	return a, dl.BindAll(h, dl.Table{
		"AudioConverterNew": &a.create, "AudioConverterDispose": &a.dispose, "AudioConverterReset": &a.reset,
		"AudioConverterSetProperty": &a.setProp, "AudioConverterGetProperty": &a.getProp,
		"AudioConverterGetPropertyInfo": &a.propInfo, "AudioConverterFillComplexBuffer": &a.fill,
	})
})

func atFormat(c Codec) (fourcc, flags, frames uint32) {
	switch c {
	case AAC:
		return fourccAAC, 0, aacFrameSamples
	case MP3:
		return fourccMP3, 0, 1152
	case FLAC:
		return fourccFLAC, atLossless16Bit, 4096
	case ALAC:
		return fourccALAC, atLossless16Bit, 4096
	case AC3:
		return fourccAC3, 0, 1536
	}
	return 0, 0, 0
}

func atCompressed(cfg AudioConfig) asbd {
	f, flags, frames := atFormat(cfg.Codec)
	return asbd{sampleRate: float64(cfg.SampleRate), format: f, flags: flags, framesPerPacket: frames, channels: uint32(cfg.Channels)}
}

// atProbe reports whether the system has this converter.
func atProbe(c Codec, d Direction) error {
	a, err := loadAT()
	if err != nil {
		return err
	}
	cfg := AudioConfig{Codec: c, SampleRate: 44100, Channels: 2}
	if c == AC3 {
		cfg.SampleRate = 48000
	}
	comp, pcm := atCompressed(cfg), pcmFormat(cfg)
	in, out := &comp, &pcm
	if d == Encode {
		in, out = &pcm, &comp
	}
	var conv uintptr
	if st := a.create(unsafe.Pointer(in), unsafe.Pointer(out), &conv); st != 0 {
		return fmt.Errorf("audiotoolbox: no %s %s (status %d)", c, d, st)
	}
	a.dispose(conv)
	return nil
}

func osAudioBackends() []backend {
	var out []backend
	add := func(c Codec, d Direction) {
		info := BackendInfo{"audiotoolbox", c, d, false}
		avail := sync.OnceValue(func() error { return atProbe(c, d) })
		b := backend{BackendInfo: info, avail: func(*Library) error { return avail() }}
		if d == Decode {
			b.adec = func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newATDecoder(cfg, info) }
		} else {
			b.aenc = func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newATEncoder(cfg, info) }
		}
		out = append(out, b)
	}
	for _, c := range []Codec{AAC, MP3, FLAC, ALAC, AC3} {
		add(c, Decode)
	}
	for _, c := range []Codec{AAC, FLAC, ALAC} {
		add(c, Encode)
	}
	return out
}

// atConverter wraps one AudioConverter.
type atConverter struct {
	c      dl.Caller
	api    *atAPI
	conv   uintptr
	thunk  uintptr
	input  atShared
	list   auBufferList
	count  uint32
	status int32
	chans  uint32
}

func (c *atConverter) open(a *atAPI, in, out *asbd, chans int) error {
	c.api, c.thunk, c.chans = a, atThunk(), uint32(chans)
	if st := a.create(unsafe.Pointer(in), unsafe.Pointer(out), &c.conv); st != 0 {
		return fmt.Errorf("audiotoolbox: cannot create converter (status %d)", st)
	}
	return nil
}

func (c *atConverter) dispose() {
	if c.conv != 0 {
		c.api.dispose(c.conv)
		c.conv = 0
	}
}

// fill runs the converter. It returns the packets produced; c.status says why it
// stopped.
func (c *atConverter) fill(dst unsafe.Pointer, size int, packets uint32, descs uintptr) (uint32, error) {
	c.list = auBufferList{count: 1, channels: c.chans, bytes: uint32(size), data: uintptr(dst)}
	c.count = packets
	r := c.c.C6(c.api.fill, c.conv, c.thunk, uintptr(unsafe.Pointer(&c.input)),
		uintptr(unsafe.Pointer(&c.count)), uintptr(unsafe.Pointer(&c.list)), descs)
	c.status = int32(r)
	if c.status != 0 && c.status != auNoData {
		return 0, fmt.Errorf("audiotoolbox: convert: status %d", c.status)
	}
	return c.count, nil
}

// setLayout tells the converter that the PCM side uses WAV channel order.
func (c *atConverter) setLayout(prop uint32, channels int) {
	tag := uint32(0)
	switch channels {
	case 3:
		tag = 113<<16 | 3 // MPEG_3_0_A: L R C
	case 5:
		tag = 117<<16 | 5 // MPEG_5_0_A: L R C Ls Rs
	case 6:
		tag = 121<<16 | 6 // MPEG_5_1_A: L R C LFE Ls Rs
	case 7:
		tag = 125<<16 | 7 // MPEG_6_1_A: L R C LFE Ls Rs Cs
	case 8:
		tag = 127<<16 | 8 // MPEG_7_1_C: L R C LFE Ls Rs Rls Rrs
	default:
		return
	}
	layout := [3]uint32{tag} // AudioChannelLayout with no descriptions
	c.api.setProp(c.conv, prop, 12, unsafe.Pointer(&layout))
}

// --- decoder ---

type atDecoder struct {
	audioDecBase
	atConverter
	opened    bool
	maxFrames int
	times     ptsRing
	cookie    []byte
}

func newATDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadAT()
	if err != nil {
		return nil, err
	}
	d := &atDecoder{}
	d.api = a
	_, _, frames := atFormat(cfg.Codec)
	d.initDec(cfg, info, int(frames))
	switch cfg.Codec {
	case AAC:
		if len(cfg.Extra) > 0 {
			if err := d.openAAC(cfg.Extra); err != nil {
				return nil, err
			}
		}
	case FLAC:
		var blocks []byte
		if len(cfg.Extra) > 0 {
			if blocks = flacConfigBlocks(cfg.Extra); blocks == nil {
				return nil, fmt.Errorf("%w: flac: unrecognised setup data", ErrInvalid)
			}
		}
		if len(blocks) >= flacHeaderSize {
			if err := d.openFLAC(blocks); err != nil {
				return nil, err
			}
		}
	case ALAC:
		cookie := alacCookie(cfg.Extra)
		if cookie == nil {
			return nil, fmt.Errorf("%w: alac needs its magic cookie in Extra", ErrInvalid)
		}
		frames := int(cookie[0])<<24 | int(cookie[1])<<16 | int(cookie[2])<<8 | int(cookie[3])
		d.channels = int(cookie[9])
		d.rate = int(cookie[20])<<24 | int(cookie[21])<<16 | int(cookie[22])<<8 | int(cookie[23])
		d.lastFrames = frames
		if err := d.start(uint32(frames), cookie); err != nil {
			return nil, err
		}
	}
	return d, nil
}

const flacHeaderSize = 4 + 4 + 34

// alacCookie finds the 24-byte ALACSpecificConfig in a magic cookie.
func alacCookie(b []byte) []byte {
	const size = 24
	for i := 0; i+8 <= len(b) && i < 64; i++ {
		if string(b[i:i+4]) == "alac" {
			if i+8+size <= len(b) {
				return b[i+8 : i+8+size]
			}
		}
	}
	if len(b) >= size {
		return b[:size]
	}
	return nil
}

func (d *atDecoder) openFLAC(blocks []byte) error {
	si := blocks[8:flacHeaderSize]
	rate := int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
	channels := int(si[12]>>1&7) + 1
	block := int(si[2])<<8 | int(si[3])
	if rate == 0 || block == 0 {
		return fmt.Errorf("%w: flac: bad STREAMINFO", ErrInvalid)
	}
	d.rate, d.channels, d.lastFrames = rate, channels, block
	// The cookie is a dfLa atom holding the STREAMINFO block.
	const atomSize = 12 + flacHeaderSize - 4
	cookie := make([]byte, atomSize)
	cookie[3] = atomSize
	copy(cookie[4:], "dfLa")
	copy(cookie[12:], blocks[4:flacHeaderSize])
	cookie[12] |= 0x80 // last block
	return d.start(uint32(block), cookie)
}

// esdsFromConfig wraps an AudioSpecificConfig the way the AAC decoder expects its
// magic cookie.
func esdsFromConfig(asc []byte) []byte {
	desc := func(tag byte, body ...[]byte) []byte {
		n := 0
		for _, b := range body {
			n += len(b)
		}
		out := []byte{tag, 0x80 | byte(n>>21), 0x80 | byte(n>>14), 0x80 | byte(n>>7), byte(n & 0x7F)}
		for _, b := range body {
			out = append(out, b...)
		}
		return out
	}
	config := desc(4, []byte{0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, desc(5, asc))
	return desc(3, []byte{0, 0, 0}, config, desc(6, []byte{2}))
}

// configFromEsds finds the AudioSpecificConfig inside an esds magic cookie.
func configFromEsds(b []byte) []byte {
	for len(b) >= 2 {
		tag := b[0]
		size, i := 0, 1
		for i < len(b) && i < 5 {
			v := b[i]
			i++
			size = size<<7 | int(v&0x7F)
			if v&0x80 == 0 {
				break
			}
		}
		if size > len(b)-i {
			return nil
		}
		body := b[i : i+size]
		switch tag {
		case 5:
			return body
		case 3:
			if len(body) < 3 {
				return nil
			}
			return configFromEsds(body[3:])
		case 4:
			if len(body) < 13 {
				return nil
			}
			return configFromEsds(body[13:])
		}
		b = b[i+size:]
	}
	return nil
}

func (d *atDecoder) openAAC(asc []byte) error {
	aot, rate, channels, ok := aacConfigInfo(asc)
	if !ok {
		return fmt.Errorf("%w: aac: bad AudioSpecificConfig", ErrInvalid)
	}
	if channels == 0 {
		channels = d.cfg.Channels
	}
	d.rate, d.channels = rate, channels
	frames := aacFrameSamples
	if aot == 5 || aot == 29 {
		frames = 2 * aacFrameSamples
		d.rate *= 2
	}
	d.lastFrames = frames
	return d.start(uint32(frames), esdsFromConfig(asc))
}

// start creates the converter for the stream format now in d.
func (d *atDecoder) start(framesPerPacket uint32, cookie []byte) error {
	f, flags, _ := atFormat(d.cfg.Codec)
	in := asbd{sampleRate: float64(d.rate), format: f, flags: flags, framesPerPacket: framesPerPacket, channels: uint32(d.channels)}
	out := pcmFormat(AudioConfig{SampleRate: d.rate, Channels: d.channels})
	if err := d.open(d.api, &in, &out, d.channels); err != nil {
		return err
	}
	if len(cookie) > 0 {
		if st := d.api.setProp(d.conv, atPropDecompCookie, uint32(len(cookie)), unsafe.Pointer(&cookie[0])); st != 0 {
			d.dispose()
			return fmt.Errorf("%w: audiotoolbox: magic cookie rejected (status %d)", ErrInvalid, st)
		}
		d.cookie = append([]byte(nil), cookie...)
	}
	prime := uint32(atPrimeNone)
	d.api.setProp(d.conv, atPropPrimeMethod, 4, unsafe.Pointer(&prime))
	d.setLayout(atPropOutLayout, d.channels)
	d.input.channels = uint32(d.channels)
	d.maxFrames = int(framesPerPacket) * 2
	d.opened = true
	return nil
}

// packet decodes one compressed packet.
func (d *atDecoder) packet(data []byte, pts time.Duration) error {
	s := &d.input
	s.avail, s.packets, s.bytes = 1, 1, uint32(len(data))
	s.data = uintptr(unsafe.Pointer(&data[0]))
	s.desc = auPacketDesc{dataByteSize: uint32(len(data))}
	d.times.push(pts)
	defer runtime.KeepAlive(data)
	for {
		f := d.frames.get(d.maxFrames * d.channels)
		n, err := d.fill(unsafe.Pointer(&f.Samples[0]), 2*len(f.Samples), uint32(d.maxFrames), 0)
		if err != nil {
			f.Release()
			s.avail = 0
			d.api.reset(d.conv)
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if n == 0 {
			f.Release()
			return nil
		}
		d.push(f, int(n), d.times.pop())
		if d.status == auNoData {
			return nil
		}
	}
}

func (d *atDecoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		d.silence(p.PTS)
		return nil
	}
	data, pts := p.Data, p.PTS
	switch d.cfg.Codec {
	case AAC:
		if !adtsChain(data) {
			if !d.opened {
				return fmt.Errorf("%w: aac: no AudioSpecificConfig and no ADTS header", ErrCorrupt)
			}
			return d.packet(data, pts)
		}
		for len(data) > 0 {
			h, _ := parseADTS(data)
			if !d.opened {
				if err := d.openAAC(h.config()); err != nil {
					return err
				}
			}
			if err := d.packet(data[h.headerLen:h.frameLen], pts); err != nil {
				return err
			}
			data = data[h.frameLen:]
			pts += audioDuration(aacFrameSamples, d.rate)
		}
		return nil
	case MP3:
		for rest := data; len(rest) > 0; {
			h, ok := parseMP3(rest)
			if !ok || h.size > len(rest) {
				return fmt.Errorf("%w: mp3: not a complete frame", ErrCorrupt)
			}
			rest = rest[h.size:]
		}
		for len(data) > 0 {
			h, _ := parseMP3(data)
			frame := data[:h.size]
			data = data[h.size:]
			if h.isTag(frame) {
				continue
			}
			if !d.opened {
				d.rate, d.channels, d.lastFrames = h.rate, h.channels, h.samples
				if err := d.start(uint32(h.samples), nil); err != nil {
					return err
				}
			}
			if err := d.packet(frame, pts); err != nil {
				return err
			}
			pts += audioDuration(h.samples, h.rate)
		}
		return nil
	case AC3:
		for rest := data; len(rest) > 0; {
			size, _, _, ok := parseAC3(rest)
			if !ok || size > len(rest) || !ac3CRCValid(rest[:size]) {
				return fmt.Errorf("%w: ac3: damaged syncframe", ErrCorrupt)
			}
			rest = rest[size:]
		}
		for len(data) > 0 {
			size, rate, channels, _ := parseAC3(data)
			if !d.opened {
				d.rate, d.channels = rate, channels
				if err := d.start(ac3FrameSamples, nil); err != nil {
					return err
				}
			}
			if err := d.packet(data[:size], pts); err != nil {
				return err
			}
			data = data[size:]
			pts += audioDuration(ac3FrameSamples, rate)
		}
		return nil
	case FLAC:
		if len(data) >= 4 && string(data[:4]) == "fLaC" {
			if d.opened || len(data) < flacHeaderSize {
				return nil
			}
			return d.openFLAC(data)
		}
		if !d.opened {
			return fmt.Errorf("%w: flac: no STREAMINFO", ErrCorrupt)
		}
		// Rejected here, because a converter reset loses the decoder state.
		if !flacFrameValid(data) {
			return fmt.Errorf("%w: flac: damaged frame", ErrCorrupt)
		}
	}
	return d.packet(data, pts)
}

func (d *atDecoder) Close() error {
	if !d.closed {
		d.closed = true
		d.dispose()
		d.dropOutput()
	}
	return nil
}

// --- encoder ---

type atEncoder struct {
	audioEncBase
	atConverter
	desc   auPacketDesc
	maxOut int
	delay  time.Duration
	lossy  bool
}

func newATEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadAT()
	if err != nil {
		return nil, err
	}
	e := &atEncoder{lossy: cfg.Codec == AAC}
	e.cfg, e.info = cfg, info
	in, out := pcmFormat(cfg), atCompressed(cfg)
	if err := e.open(a, &in, &out, cfg.Channels); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if cfg.Bitrate > 0 && e.lossy {
		v := uint32(cfg.Bitrate)
		if st := a.setProp(e.conv, auBitrateProp, 4, unsafe.Pointer(&v)); st != 0 {
			e.Close()
			return nil, fmt.Errorf("%w: audiotoolbox: bitrate %d rejected (status %d)", ErrInvalid, cfg.Bitrate, st)
		}
	}
	e.setLayout(atPropInLayout, cfg.Channels)
	var actual asbd
	size := uint32(unsafe.Sizeof(actual))
	if st := a.getProp(e.conv, atPropOutFormat, &size, unsafe.Pointer(&actual)); st != 0 || actual.framesPerPacket == 0 {
		e.Close()
		return nil, fmt.Errorf("audiotoolbox: no output format (status %d)", st)
	}
	frames := int(actual.framesPerPacket)
	var maxPacket uint32
	size = 4
	if a.getProp(e.conv, atPropMaxOutPacket, &size, unsafe.Pointer(&maxPacket)) != 0 || maxPacket == 0 {
		maxPacket = atDefaultMaxPacket
	}
	e.maxOut = int(maxPacket)
	if err := e.readConfig(); err != nil {
		e.Close()
		return nil, err
	}
	if e.lossy {
		var prime [2]uint32 // leadingFrames, trailingFrames
		size = 8
		if a.getProp(e.conv, atPropPrimeInfo, &size, unsafe.Pointer(&prime)) == 0 {
			e.delay = audioDuration(int(prime[0]), cfg.SampleRate)
		}
	}
	e.input.channels = uint32(cfg.Channels)
	e.times.step = audioDuration(frames, cfg.SampleRate)
	e.framer.init(cfg, frames, e.encode)
	return e, nil
}

func (e *atEncoder) readConfig() error {
	a := e.api
	var size uint32
	var writable uint8
	if a.propInfo(e.conv, atPropCompCookie, &size, &writable) != 0 || size == 0 {
		if e.cfg.Codec == AAC {
			e.config = aacConfig(e.cfg.SampleRate, e.cfg.Channels)
		}
		return nil
	}
	b := make([]byte, size)
	if st := a.getProp(e.conv, atPropCompCookie, &size, unsafe.Pointer(&b[0])); st != 0 {
		return fmt.Errorf("audiotoolbox: no magic cookie (status %d)", st)
	}
	b = b[:size]
	switch e.cfg.Codec {
	case AAC:
		e.config = append([]byte(nil), configFromEsds(b)...)
		if len(e.config) == 0 {
			e.config = aacConfig(e.cfg.SampleRate, e.cfg.Channels)
		}
	case ALAC:
		e.config = append([]byte(nil), alacCookie(b)...)
	case FLAC:
		if len(b) < 12 {
			return errors.New("audiotoolbox: short FLAC magic cookie")
		}
		e.config = append([]byte("fLaC"), b[12:]...) // skip the dfLa atom header
	default:
		e.config = b
	}
	return nil
}

// pull asks for one packet.
func (e *atEncoder) pull() (bool, error) {
	p := e.packets.get(e.maxOut)
	n, err := e.fill(unsafe.Pointer(&p.Data[0]), len(p.Data), 1, uintptr(unsafe.Pointer(&e.desc)))
	if err != nil {
		p.Release()
		return false, err
	}
	if n == 0 || e.desc.dataByteSize == 0 {
		p.Release()
		return false, nil
	}
	p.Data = p.Data[:e.desc.dataByteSize]
	p.PTS, p.Keyframe = e.times.pop()-e.delay, true
	e.out.push(p)
	return true, nil
}

func (e *atEncoder) encode(pcm []int16, pts time.Duration) error {
	e.times.push(pts)
	s := &e.input
	s.avail, s.packets, s.bytes = 1, uint32(len(pcm)/e.cfg.Channels), uint32(2*len(pcm))
	s.data = uintptr(unsafe.Pointer(&pcm[0]))
	s.desc = auPacketDesc{}
	defer runtime.KeepAlive(pcm)
	for {
		got, err := e.pull()
		if err != nil {
			return err
		}
		if !got || e.status == auNoData {
			return nil
		}
	}
}

func (e *atEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

// Flush pads the last AAC frame with silence, keeps the exact length of lossless
// streams, and drains the converter.
func (e *atEncoder) Flush() error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return nil
	}
	e.flushed = true
	if err := e.framer.flush(e.lossy); err != nil {
		return err
	}
	e.input.eof = 1
	for i := 0; i < 1<<16; i++ {
		got, err := e.pull()
		if err != nil {
			return err
		}
		if !got {
			return nil
		}
	}
	return errors.New("audiotoolbox: encoder does not drain")
}

func (e *atEncoder) Close() error {
	if !e.closed {
		e.closed = true
		e.dispose()
		e.dropOutput()
	}
	return nil
}
