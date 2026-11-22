//go:build windows && (amd64 || arm64)

package gaal

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"
)

// AAC, MP3, FLAC, ALAC and AC-3 through the Media Foundation transforms of
// Windows. Which of them exist depends on the Windows edition: N editions and
// Server installations can lack some, in which case the backend is unavailable.

var (
	catAudioEnc = mfGUID{0x91c64bd0, 0xf91e, 0x4d8c, [8]byte{0x92, 0x76, 0xdb, 0x24, 0x82, 0x79, 0xd9, 0x75}}

	fmtAAC  = mfWaveFormat(0x1610)
	fmtMP3  = mfWaveFormat(0x55)
	fmtFLAC = mfWaveFormat(0xF1AC)
	fmtALAC = mfWaveFormat(0x6C61)
	fmtAC3  = mfGUID{0xe06d802c, 0xdb46, 0x11cf, [8]byte{0xb4, 0xd1, 0x00, 0x80, 0x5f, 0x6c, 0xbb, 0xea}}

	attrAACPayload = mfGUID{0xbfbabe79, 0x7434, 0x4d1c, [8]byte{0x94, 0xf0, 0x72, 0xa3, 0xb9, 0xe1, 0x71, 0x88}}
	attrAACProfile = mfGUID{0x7632f0e6, 0x9538, 0x4d61, [8]byte{0xac, 0xda, 0xea, 0x29, 0xc8, 0xc1, 0x44, 0x56}}
)

const (
	mfAACProfileLC  = 0x29
	mfHEAACInfoSize = 12 // HEAACWAVEINFO after its WAVEFORMATEX
	mfLostRing      = 8
)

func mfAudioSubtype(c Codec) *mfGUID {
	switch c {
	case AAC:
		return &fmtAAC
	case MP3:
		return &fmtMP3
	case FLAC:
		return &fmtFLAC
	case ALAC:
		return &fmtALAC
	case AC3:
		return &fmtAC3
	}
	return nil
}

func mfAudioSpec(c Codec, d Direction) mfSpec {
	sub := mfAudioSubtype(c)
	if d == Decode {
		return mfSpec{category: &catAudioDec, in: mfTypeInfo{mediaAudio, *sub}, anyOut: true}
	}
	return mfSpec{category: &catAudioEnc, in: mfTypeInfo{mediaAudio, fmtPCM}, out: mfTypeInfo{mediaAudio, *sub}}
}

func mfAudioProbe(c Codec, d Direction) error {
	api, err := loadMF()
	if err != nil {
		return err
	}
	call := newMFCall()
	list, err := api.enumerate(call, mfAudioSpec(c, d))
	if err != nil {
		return err
	}
	found := false
	for _, a := range list {
		found = found || !mfIsHardware(call, a)
		call.release(a)
	}
	if !found {
		return errMFNoTransform
	}
	return nil
}

func osAudioBackends() []backend {
	var out []backend
	add := func(c Codec, d Direction) {
		info := BackendInfo{mfBackendName, c, d, false}
		probe := sync.OnceValue(func() error { return mfAudioProbe(c, d) })
		b := backend{BackendInfo: info, avail: func(*Library) error { return probe() }}
		if d == Decode {
			b.adec = func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newMFCodecDecoder(cfg, info) }
		} else {
			b.aenc = func(_ *Library, cfg AudioConfig) (AudioEncoder, error) { return newMFCodecEncoder(cfg, info) }
		}
		out = append(out, b)
	}
	for _, c := range []Codec{AAC, MP3, FLAC, ALAC, AC3} {
		add(c, Decode)
	}
	for _, c := range []Codec{AAC, MP3, FLAC, ALAC} {
		add(c, Encode)
	}
	return out
}

func mfSetBlob(c *mfCall, t uintptr, key *mfGUID, b []byte) {
	if len(b) > 0 {
		c.m3(t, slotSetBlob, gp(key), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	}
}

// --- decoder ---

type mfCodecDecoder struct {
	audioDecBase
	m        *mfTransform
	float    bool
	extra    []byte // user data candidates are derived from this
	head     []byte // an in-band FLAC header being collected
	flacGaps flacGapCloser
	last     []byte
	lost     [mfLostRing]time.Duration
	lostAt   int
}

func newMFCodecDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	d := &mfCodecDecoder{}
	frames := map[Codec]int{AAC: aacFrameSamples, MP3: 1152, FLAC: flacDefaultBlock, ALAC: 4096, AC3: ac3FrameSamples}[cfg.Codec]
	d.initDec(cfg, info, frames)
	for i := range d.lost {
		d.lost[i] = -1 << 62
	}
	switch cfg.Codec {
	case AAC:
		if len(cfg.Extra) > 0 {
			_, rate, channels, ok := aacConfigInfo(cfg.Extra)
			if !ok {
				return nil, fmt.Errorf("%w: aac: bad AudioSpecificConfig", ErrInvalid)
			}
			d.rate = rate
			if channels > 0 {
				d.channels = channels
			}
			return d, d.open(cfg.Extra)
		}
		return d, nil
	case FLAC:
		if len(cfg.Extra) > 0 {
			b := flacConfigBlocks(cfg.Extra)
			if b == nil {
				return nil, fmt.Errorf("%w: flac: unrecognised setup data", ErrInvalid)
			}
			if len(b) >= 4+4+34 {
				si := b[8:]
				d.rate = int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
				d.channels = int(si[12]>>1&7) + 1
			}
			return d, d.open(b)
		}
		return d, nil
	case ALAC:
		if len(cfg.Extra) == 0 {
			return nil, fmt.Errorf("%w: alac needs its magic cookie in Extra", ErrInvalid)
		}
		return d, d.open(cfg.Extra)
	}
	return d, nil
}

func (d *mfCodecDecoder) open(extra []byte) error {
	d.extra = append([]byte(nil), extra...)
	m, err := mfOpen(mfAudioSpec(d.cfg.Codec, Decode), false, d, d.setup)
	if err != nil {
		return err
	}
	d.m = m
	return nil
}

// userData lists the forms of setup data to try, best first.
func (d *mfCodecDecoder) userData() [][]byte {
	switch d.cfg.Codec {
	case AAC:
		head := make([]byte, mfHEAACInfoSize, mfHEAACInfoSize+len(d.extra))
		head[2] = mfAACProfileLC
		return [][]byte{append(head, d.extra...)}
	case FLAC:
		if len(d.extra) > 4 {
			return [][]byte{d.extra, d.extra[4:]}
		}
	case ALAC:
		return [][]byte{d.extra, alacConfigOnly(d.extra)}
	}
	return [][]byte{nil}
}

// alacConfigOnly strips an 'alac' atom header from a magic cookie.
func alacConfigOnly(b []byte) []byte {
	const size = 24
	for i := 0; i+8+size <= len(b); i++ {
		if string(b[i:i+4]) == "alac" {
			return b[i+8 : i+8+size]
		}
	}
	return b
}

func (d *mfCodecDecoder) setup(m *mfTransform) error {
	d.m = m
	m.orderedPTS = true
	m.oneToOne = d.cfg.Codec == FLAC || d.cfg.Codec == ALAC
	c := &m.c
	sub := mfAudioSubtype(d.cfg.Codec)
	var hr uint32
	for _, data := range d.userData() {
		t, err := m.api.newMediaType(c, &mediaAudio, sub)
		if err != nil {
			return err
		}
		c.setU32(t, &attrRate, uint32(d.rate))
		c.setU32(t, &attrChannels, uint32(d.channels))
		c.setU32(t, &attrBits, 16)
		if d.cfg.Codec == AAC {
			c.setU32(t, &attrAACPayload, 0) // raw access units
			c.setU32(t, &attrAACProfile, mfAACProfileLC)
		}
		mfSetBlob(c, t, &attrUserData, data)
		if hr = m.setType(slotSetInputType, t, 0); !mfFailed(hr) {
			break
		}
	}
	if mfFailed(hr) {
		return mfError("audio input type", hr)
	}
	if err := d.onFormat(); err != nil {
		return err
	}
	if err := m.start(); err != nil {
		return err
	}
	// The FLAC decoder reads the stream header from its input, whatever the
	// type's user data says. Without it, ProcessOutput never returns once a
	// second frame is in (measured on Windows Server 2025 and 11 on Arm).
	if d.cfg.Codec == FLAC && len(d.extra) > 4 {
		m.untimed = true
		if err := d.queue(d.extra, 0); err != nil {
			return err
		}
		d.last = d.last[:0] // not a frame to repeat for a lost one
	}
	return nil
}

// onFormat selects PCM, preferring 16 bits and the stream layout.
func (d *mfCodecDecoder) onFormat() error {
	m := d.m
	c := &m.c
	best, bestScore, bestFloat, bestRate, bestCh := uintptr(0), -1, false, 0, 0
	for i := 0; ; i++ {
		c.p = 0
		if mfFailed(c.m3(m.xf, slotOutAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&c.p)))) {
			break
		}
		t := c.p
		sub, _ := c.getGUID(t, &attrSubtype)
		bits, _ := c.getU32(t, &attrBits)
		rate, _ := c.getU32(t, &attrRate)
		ch, _ := c.getU32(t, &attrChannels)
		score := -1
		switch {
		case sub == fmtPCM && bits == 16:
			score = 2
		case sub == fmtFloat && bits == 32:
			score = 0
		}
		if score >= 0 && int(ch) == d.channels {
			score += 4
		}
		if score >= 0 && int(rate) == d.rate {
			score += 8
		}
		if score > bestScore {
			c.release(best)
			best, bestScore, bestFloat, bestRate, bestCh = t, score, sub == fmtFloat, int(rate), int(ch)
		} else {
			c.release(t)
		}
	}
	if best == 0 {
		return fmt.Errorf("mediafoundation: no PCM output for %s", d.cfg.Codec)
	}
	if hr := m.setType(slotSetOutputType, best, 0); mfFailed(hr) {
		return mfError("audio output type", hr)
	}
	d.float, d.rate, d.channels = bestFloat, bestRate, bestCh
	return nil
}

func (d *mfCodecDecoder) isLost(pts time.Duration) bool {
	for _, v := range d.lost {
		if v == pts {
			return true
		}
	}
	return false
}

func (d *mfCodecDecoder) onSample(sample uintptr) error {
	m := d.m
	pts := m.samplePTS(sample)
	addr, _, size, err := m.lockSample(sample)
	if err != nil {
		return err
	}
	defer m.unlockSample()
	n := size / 2
	if d.float {
		n = size / 4
	}
	n -= n % max(d.channels, 1)
	if n == 0 {
		return nil
	}
	f := d.frames.get(n)
	switch {
	case d.cfg.Codec != AAC && d.isLost(pts):
		clear(f.Samples) // a repeated packet stands in for a lost one
	case d.float:
		for i, v := range unsafe.Slice((*float32)(mfPtr(addr)), n) {
			f.Samples[i] = clamp16(v)
		}
	default:
		copy(f.Samples, unsafe.Slice((*int16)(mfPtr(addr)), n))
	}
	d.push(f, n/d.channels, pts)
	return nil
}

func (d *mfCodecDecoder) queue(data []byte, pts time.Duration) error {
	in, dst, err := d.m.begin(len(data))
	if err != nil {
		return err
	}
	copy(dst, data)
	if cap(d.last) < len(data) {
		d.last = make([]byte, len(data), 2*len(data))
	}
	d.last = d.last[:len(data)]
	copy(d.last, data)
	// The ALAC decoder rejects input without a duration.
	return d.m.commit(in, len(data), pts, d.frameDuration(data), true)
}

// frameDuration is how long a frame of compressed data plays, or 0.
func (d *mfCodecDecoder) frameDuration(data []byte) time.Duration {
	switch d.cfg.Codec {
	case FLAC:
		if h, ok := parseFLACHeader(data); ok && d.rate > 0 {
			return audioDuration(int(h.block), d.rate)
		}
	case ALAC:
		if c := alacConfigOnly(d.extra); len(c) == 24 && d.rate > 0 {
			return audioDuration(int(c[0])<<24|int(c[1])<<16|int(c[2])<<8|int(c[3]), d.rate)
		}
	}
	return 0
}

func (d *mfCodecDecoder) lose(pts time.Duration) error {
	if d.m == nil || len(d.last) == 0 {
		d.silence(pts)
		return nil
	}
	d.lost[d.lostAt] = pts
	d.lostAt = (d.lostAt + 1) % mfLostRing
	if d.cfg.Codec == FLAC {
		var si []byte
		if len(d.extra) >= 4+4+34 {
			si = d.extra[8:]
		}
		if f := flacSilentAfter(d.last, si); f != nil {
			return d.queue(d.flacGaps.fix(f), pts)
		}
	}
	return d.queue(d.last, pts)
}

func (d *mfCodecDecoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		return d.lose(p.PTS)
	}
	data, pts := p.Data, p.PTS
	switch d.cfg.Codec {
	case AAC:
		if !adtsChain(data) {
			if d.m == nil {
				return fmt.Errorf("%w: aac: no AudioSpecificConfig and no ADTS header", ErrCorrupt)
			}
			return d.queue(data, pts)
		}
		for len(data) > 0 {
			h, _ := parseADTS(data)
			if d.m == nil {
				asc := h.config()
				_, d.rate, d.channels, _ = aacConfigInfo(asc)
				if err := d.open(asc); err != nil {
					return err
				}
			}
			if err := d.queue(data[h.headerLen:h.frameLen], pts); err != nil {
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
			if d.m == nil {
				d.rate, d.channels, d.lastFrames = h.rate, h.channels, h.samples
				if err := d.open(nil); err != nil {
					return err
				}
			}
			if err := d.queue(frame, pts); err != nil {
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
			if d.m == nil {
				d.rate, d.channels = rate, channels
				if err := d.open(nil); err != nil {
					return err
				}
			}
			if err := d.queue(data[:size], pts); err != nil {
				return err
			}
			data = data[size:]
			pts += audioDuration(ac3FrameSamples, rate)
		}
		return nil
	case FLAC:
		// An in-band header can come as "fLaC" and then one packet per
		// metadata block. The transform opens once the last block is in: given
		// "fLaC" alone it never returns from ProcessOutput.
		isHead := len(data) >= 4 && string(data[:4]) == "fLaC"
		if d.m == nil && (isHead || (len(d.head) > 0 && data[0] != 0xFF)) {
			if isHead {
				d.head = d.head[:0]
			}
			if d.head = append(d.head, data...); !flacHeaderComplete(d.head) {
				return nil
			}
			si := d.head[8:]
			d.rate = int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
			d.channels = int(si[12]>>1&7) + 1
			return d.open(d.head)
		}
		if isHead {
			return nil
		}
		if d.m == nil {
			return fmt.Errorf("%w: flac: no STREAMINFO", ErrCorrupt)
		}
		if !flacFrameValid(data) {
			return fmt.Errorf("%w: flac: damaged frame", ErrCorrupt)
		}
		return d.queue(d.flacGaps.fix(data), pts)
	}
	return d.queue(data, pts)
}

func (d *mfCodecDecoder) Receive() (*AudioFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if d.flushed {
		return nil, io.EOF
	}
	if d.m != nil {
		if err := d.m.service(); err != nil {
			return nil, err
		}
		if f, ok := d.out.pop(); ok {
			return f, nil
		}
	}
	return nil, ErrAgain
}

func (d *mfCodecDecoder) Flush() error {
	if d.closed {
		return ErrClosed
	}
	if d.flushed {
		return nil
	}
	d.flushed = true
	if d.m == nil {
		return nil
	}
	return d.m.drain()
}

func (d *mfCodecDecoder) Close() error {
	if !d.closed {
		d.closed = true
		if d.m != nil {
			d.m.close()
		}
		d.dropOutput()
	}
	return nil
}

// --- encoder ---

type mfCodecEncoder struct {
	audioEncBase
	m       *mfTransform
	lossy   bool
	dur     time.Duration
	guessed bool // config is the default ALAC cookie, not the transform's

	// MP3 packets are timed by count from the first input: the transform
	// stamps each with the input that completed it, which depends on how
	// much it buffers, and that changes with the bitrate.
	first    time.Duration
	started  bool
	mp3Count int // samples per channel in the MP3 frames emitted so far

	flacHead bool // inside the in-band FLAC header the transform emits
}

// mfMP3Delay is the delay of the Windows MP3 encoder and decoder together,
// in samples: 1830 at 22.05, 44.1 and 48 kHz, measured with the tone test.
const mfMP3Delay = 1830

// alacDefaultCookie is the ALACSpecificConfig of Apple's encoder defaults,
// which the Windows ALAC encoder uses, for when the transform states none.
func alacDefaultCookie(rate, channels, frames int) []byte {
	b := make([]byte, 24)
	binary.BigEndian.PutUint32(b[0:], uint32(frames))
	b[5], b[6], b[7], b[8], b[9] = 16, 40, 10, 14, byte(channels) // bit depth, pb, mb, kb
	binary.BigEndian.PutUint16(b[10:], 255)                       // maxRun
	binary.BigEndian.PutUint32(b[20:], uint32(rate))
	return b
}

func newMFCodecEncoder(cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	e := &mfCodecEncoder{lossy: cfg.Codec == AAC || cfg.Codec == MP3}
	e.cfg, e.info = cfg, info
	frames := map[Codec]int{AAC: aacFrameSamples, MP3: 1152, FLAC: flacDefaultBlock, ALAC: 4096}[cfg.Codec]
	if cfg.Codec == MP3 && cfg.SampleRate < 32000 {
		frames = 576
	}
	e.dur = audioDuration(frames, cfg.SampleRate)
	e.times.step = e.dur
	e.framer.init(cfg, frames, e.encode)
	m, err := mfOpen(mfAudioSpec(cfg.Codec, Encode), false, e, e.setup)
	if err != nil {
		return nil, err
	}
	e.m = m
	return e, nil
}

func (e *mfCodecEncoder) pcmType(m *mfTransform) (uintptr, error) {
	c, cfg := &m.c, e.cfg
	t, err := m.api.newMediaType(c, &mediaAudio, &fmtPCM)
	if err != nil {
		return 0, err
	}
	c.setU32(t, &attrRate, uint32(cfg.SampleRate))
	c.setU32(t, &attrChannels, uint32(cfg.Channels))
	c.setU32(t, &attrBits, 16)
	c.setU32(t, &attrBlockAlign, uint32(2*cfg.Channels))
	c.setU32(t, &attrAvgBytes, uint32(2*cfg.Channels*cfg.SampleRate))
	return t, nil
}

// chooseOutput sets the offered output type closest to the requested bitrate.
func (e *mfCodecEncoder) chooseOutput(m *mfTransform) uint32 {
	c, cfg := &m.c, e.cfg
	want := cfg.Bitrate / 8
	best, bestDiff := uintptr(0), 1<<62
	for i := 0; ; i++ {
		c.p = 0
		if mfFailed(c.m3(m.xf, slotOutAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&c.p)))) {
			break
		}
		t := c.p
		sub, _ := c.getGUID(t, &attrSubtype)
		rate, _ := c.getU32(t, &attrRate)
		ch, _ := c.getU32(t, &attrChannels)
		avg, _ := c.getU32(t, &attrAvgBytes)
		diff := int(avg) - want
		if diff < 0 {
			diff = -diff
		}
		if sub == *mfAudioSubtype(cfg.Codec) && int(rate) == cfg.SampleRate && int(ch) == cfg.Channels && diff < bestDiff {
			c.release(best)
			best, bestDiff = t, diff
		} else {
			c.release(t)
		}
	}
	if best == 0 {
		return hrInvalidType
	}
	return m.setType(slotSetOutputType, best, 0)
}

func (e *mfCodecEncoder) setup(m *mfTransform) error {
	e.m = m
	m.orderedPTS = true
	// The lossless encoders give one frame per input block, stamped with the
	// time of the next one.
	m.oneToOne = e.cfg.Codec == FLAC || e.cfg.Codec == ALAC
	t, err := e.pcmType(m)
	if err != nil {
		return err
	}
	inputSet := !mfFailed(m.setType(slotSetInputType, t, 0))
	if hr := e.chooseOutput(m); mfFailed(hr) {
		return fmt.Errorf("%w: %v", ErrInvalid, mfError("encoder output type", hr))
	}
	if !inputSet {
		if t, err = e.pcmType(m); err != nil {
			return err
		}
		if hr := m.setType(slotSetInputType, t, 0); mfFailed(hr) {
			return fmt.Errorf("%w: %v", ErrInvalid, mfError("encoder input type", hr))
		}
	}
	e.readConfig(m)
	return m.start()
}

func (e *mfCodecEncoder) readConfig(m *mfTransform) {
	c := &m.c
	c.p = 0
	if mfFailed(c.m2(m.xf, slotOutCurrent, 0, uintptr(unsafe.Pointer(&c.p)))) {
		return
	}
	t := c.p
	blob := c.getBlob(t, &attrUserData)
	c.release(t)
	e.useConfig(blob)
}

// useConfig keeps the setup data the transform reports, if any.
func (e *mfCodecEncoder) useConfig(blob []byte) {
	switch e.cfg.Codec {
	case AAC:
		if len(blob) > mfHEAACInfoSize {
			e.config = blob[mfHEAACInfoSize:]
		} else {
			e.config = aacConfig(e.cfg.SampleRate, e.cfg.Channels)
		}
	case FLAC:
		if b := flacConfigBlocks(blob); b != nil {
			e.config = b
		}
	default:
		if len(blob) > 0 {
			e.config, e.guessed = blob, false
		} else if e.cfg.Codec == ALAC && len(e.config) == 0 {
			e.config, e.guessed = alacDefaultCookie(e.cfg.SampleRate, e.cfg.Channels, 4096), true
		}
	}
}

func (e *mfCodecEncoder) onFormat() error { return nil }

func (e *mfCodecEncoder) onSample(sample uintptr) error {
	m := e.m
	if (len(e.config) == 0 && e.cfg.Codec == FLAC) || e.guessed {
		// Some transforms state the setup data only once they produce output,
		// on the sample or on the output type.
		if blob := m.c.getBlob(sample, &attrUserData); len(blob) > 0 {
			e.useConfig(blob)
		} else {
			e.readConfig(m)
		}
		e.guessed = false // look once, before packets go out under the guess
	}
	addr, _, size, err := m.lockSample(sample)
	if err != nil {
		return err
	}
	defer m.unlockSample()
	if size == 0 {
		return nil
	}
	data := cMem(addr, size)
	if e.cfg.Codec == FLAC {
		// The header samples are not timed frames: they must not take the
		// timestamps of the inputs that the frames come from.
		if data = e.flacHeader(data); len(data) == 0 {
			return nil
		}
	}
	pts := m.samplePTS(sample)
	switch e.cfg.Codec {
	case MP3:
		for len(data) >= 4 {
			h, ok := parseMP3(data)
			if !ok || h.size > len(data) {
				break
			}
			e.emit(data[:h.size], e.first+audioDuration(e.mp3Count-mfMP3Delay, h.rate))
			e.mp3Count += h.samples
			data = data[h.size:]
		}
		return nil
	}
	e.emit(data, pts)
	return nil
}

// flacHeader takes the stream header the FLAC transform emits as packets,
// "fLaC" and then metadata blocks, as the setup data, and returns what
// follows it. Packets carry frames only, and the header goes to CodecConfig.
func (e *mfCodecEncoder) flacHeader(b []byte) []byte {
	if len(b) >= 4 && string(b[:4]) == "fLaC" {
		e.flacHead, b = true, b[4:]
	}
	for e.flacHead && len(b) >= 4 {
		n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		if 4+n > len(b) {
			break
		}
		if b[0]&0x7F == 0 && n == 34 { // STREAMINFO, as the only block
			e.config = append([]byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}, b[4:4+n]...)
		}
		e.flacHead = b[0]&0x80 == 0
		b = b[4+n:]
	}
	return b
}

func (e *mfCodecEncoder) emit(b []byte, pts time.Duration) {
	p := e.packets.get(len(b))
	copy(p.Data, b)
	p.PTS, p.Keyframe = pts, true
	e.out.push(p)
}

func (e *mfCodecEncoder) encode(pcm []int16, pts time.Duration) error {
	m := e.m
	if !e.started {
		e.first, e.started = pts, true
	}
	n := 2 * len(pcm)
	in, dst, err := m.begin(n)
	if err != nil {
		return err
	}
	copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(&pcm[0])), n))
	return m.commit(in, n, pts, audioDuration(len(pcm)/e.cfg.Channels, e.cfg.SampleRate), true)
}

func (e *mfCodecEncoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	return e.framer.write(f)
}

func (e *mfCodecEncoder) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	if !e.flushed {
		if err := e.m.service(); err != nil {
			return nil, err
		}
		if p, ok := e.out.pop(); ok {
			return p, nil
		}
	}
	return nil, endOfQueue(e.flushed)
}

// Flush pads the last lossy frame with silence and drains the transform.
func (e *mfCodecEncoder) Flush() error {
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
	return e.m.drain()
}

func (e *mfCodecEncoder) Close() error {
	if !e.closed {
		e.closed = true
		if e.m != nil {
			e.m.close()
		}
		e.dropOutput()
	}
	return nil
}
