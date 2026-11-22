//go:build android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// AAC, MP3, FLAC, Vorbis, ALAC and AC-3 through MediaCodec. The codecs belong to
// the device, and which of them exist depends on it. They are reported as
// software: audio codecs run on the CPU or on a DSP the application cannot tell
// apart from it, so the hardware flag would claim nothing useful.

const (
	mcAudioMaxInput = 1 << 16
	mcAudioPCM16    = 2
	mcAudioFloat    = 4
	mcLostRing      = 8
	flacHeaderLen   = 4 + 4 + 34
	mcMaxReopens    = 16
	// mcAACDelay is the encoder delay of the AAC-LC encoders found on Android,
	// counted in samples at the stream rate.
	mcAACDelay = 2048
)

func mcAudioMime(c Codec) string {
	switch c {
	case AAC:
		return "audio/mp4a-latm"
	case MP3:
		return "audio/mpeg"
	case FLAC:
		return "audio/flac"
	case Vorbis:
		return "audio/vorbis"
	case ALAC:
		return "audio/alac"
	case AC3:
		return "audio/ac3"
	}
	return ""
}

func osAudioBackends() []backend {
	type key struct {
		c Codec
		d Direction
	}
	var mu sync.Mutex
	known := map[key]error{}
	avail := func(k key) func(*Library) error {
		return func(*Library) error {
			mu.Lock()
			defer mu.Unlock()
			err, ok := known[k]
			if !ok {
				err = mcAudioProbe(k.c, k.d)
				known[k] = err
			}
			return err
		}
	}
	var out []backend
	for _, c := range []Codec{AAC, MP3, FLAC, Vorbis, ALAC, AC3} {
		info := BackendInfo{"mediacodec", c, Decode, false}
		out = append(out, backend{BackendInfo: info, avail: avail(key{c, Decode}),
			adec: func(l *Library, cfg AudioConfig) (AudioDecoder, error) { return newMCCodecDecoder(l, cfg, info) }})
	}
	for _, c := range []Codec{AAC, FLAC} {
		info := BackendInfo{"mediacodec", c, Encode, false}
		out = append(out, backend{BackendInfo: info, avail: avail(key{c, Encode}),
			aenc: func(l *Library, cfg AudioConfig) (AudioEncoder, error) { return newMCCodecEncoder(l, cfg, info) }})
	}
	return out
}

func mcAudioProbe(c Codec, d Direction) error {
	a, err := loadMedia()
	if err != nil {
		return err
	}
	mime := mcAudioMime(c)
	var h uintptr
	if d == Encode {
		h = a.createEncoder(mime)
	} else {
		h = a.createDecoder(mime)
	}
	if h == 0 {
		return fmt.Errorf("mediacodec: no %s %s", mime, d)
	}
	a.del(h)
	return nil
}

// --- decoder ---

type mcCodecDecoder struct {
	mcCodec
	lib      *Library
	cfg      AudioConfig
	static   BackendInfo
	frames   audioPool
	out      fifo[*AudioFrame]
	onData   func([]byte, *mcBufferInfo) error
	onFormat func() error
	drain    func() error

	rate, channels int
	float          bool
	pinned         bool // the channel count comes from the stream setup
	lastFrames     int
	last           []byte // latest packet, repeated for a lost one
	flacGaps       flacGapCloser
	lost           [mcLostRing]int64
	lostNext       int
	headers        [][]byte // Vorbis headers seen in the stream
	csd            [2][]byte
	opened         bool
	flushedInput   bool
	closed         bool
	produced       bool
	reopens        int
}

func newMCCodecDecoder(l *Library, cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	d := &mcCodecDecoder{lib: l, cfg: cfg, static: info, rate: cfg.SampleRate, channels: cfg.Channels}
	d.info = info
	d.onData, d.onFormat = d.samples, d.readFormat
	d.drain = func() error { return d.pump(0, d.onFormat, d.onData) }
	for i := range d.lost {
		d.lost[i] = -1 << 62
	}
	var err error
	switch cfg.Codec {
	case AAC:
		d.lastFrames = aacFrameSamples
		if len(cfg.Extra) > 0 {
			if _, rate, channels, ok := aacConfigInfo(cfg.Extra); ok {
				d.rate = rate
				if channels > 0 {
					d.channels = channels
					d.pinned = channels > 1
				}
			} else {
				return nil, fmt.Errorf("%w: aac: bad AudioSpecificConfig", ErrInvalid)
			}
			d.csd[0] = append([]byte(nil), cfg.Extra...)
			err = d.start()
		}
	case FLAC:
		d.lastFrames = flacDefaultBlock
		if len(cfg.Extra) > 0 {
			b := flacConfigBlocks(cfg.Extra)
			if b == nil {
				return nil, fmt.Errorf("%w: flac: unrecognised setup data", ErrInvalid)
			}
			d.csd[0] = append([]byte(nil), b...)
			if len(b) >= flacHeaderLen {
				si := b[8:]
				d.rate = int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
				d.channels, d.pinned = int(si[12]>>1&7)+1, true
			}
		}
		err = d.start()
	case Vorbis:
		d.lastFrames = 1024
		if len(cfg.Extra) > 0 {
			parts := xiphUnlace(cfg.Extra)
			if len(parts) != 3 {
				return nil, fmt.Errorf("%w: vorbis: setup data is not three laced headers", ErrInvalid)
			}
			d.setVorbisHeaders(parts[0], parts[2])
			err = d.start()
		}
	case ALAC:
		d.lastFrames = 4096
		if len(cfg.Extra) == 0 {
			return nil, fmt.Errorf("%w: alac needs its magic cookie in Extra", ErrInvalid)
		}
		d.csd[0] = append([]byte(nil), cfg.Extra...)
		err = d.start()
	case MP3:
		d.lastFrames = 1152
		err = d.start()
	case AC3:
		d.lastFrames = ac3FrameSamples
		err = d.start()
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// setVorbisHeaders takes the setup from the identification and setup headers.
func (d *mcCodecDecoder) setVorbisHeaders(id, setup []byte) {
	d.csd[0], d.csd[1] = append([]byte(nil), id...), append([]byte(nil), setup...)
	if len(id) >= 16 {
		d.channels, d.pinned = int(id[11]), true
		d.rate = int(id[12]) | int(id[13])<<8 | int(id[14])<<16 | int(id[15])<<24
	}
}

func (d *mcCodecDecoder) start() error {
	err := d.open(d.lib, d.static, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "sample-rate", int32(d.rate))
		a.setInt32(f, "channel-count", int32(d.channels))
		a.setInt32(f, "pcm-encoding", mcAudioPCM16)
		a.setInt32(f, "max-input-size", mcAudioMaxInput)
		for i, name := range []string{"csd-0", "csd-1"} {
			if b := d.csd[i]; len(b) > 0 {
				a.setBuffer(f, name, unsafe.Pointer(&b[0]), uintptr(len(b)))
			}
		}
	})
	if err != nil {
		return err
	}
	d.info.Hardware = false
	d.opened = true
	return nil
}

// recover replaces a codec that failed on damaged data.
func (d *mcCodecDecoder) recover(cause error) error {
	if !d.produced || d.reopens >= mcMaxReopens {
		return cause
	}
	d.reopens++
	d.mcCodec.Close()
	d.mcCodec = mcCodec{info: d.static}
	if err := d.start(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, cause)
}

func (d *mcCodecDecoder) readFormat() error {
	a := d.api
	f := a.outputFormat(d.h)
	if f == 0 {
		return nil
	}
	defer a.formatDelete(f)
	var v int32
	if a.getInt32(f, "sample-rate", &v) && v > 0 {
		d.rate = int(v)
	}
	if a.getInt32(f, "channel-count", &v) && v > 0 && !d.pinned {
		d.channels = int(v)
	}
	d.float = a.getInt32(f, "pcm-encoding", &v) && v == mcAudioFloat
	return nil
}

func (d *mcCodecDecoder) isLost(pts int64) bool {
	for _, v := range d.lost {
		if v == pts {
			return true
		}
	}
	return false
}

func (d *mcCodecDecoder) samples(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 || d.channels == 0 {
		return nil
	}
	n := len(b) / 2
	if d.float {
		n = len(b) / 4
	}
	n -= n % d.channels
	if n == 0 {
		return nil
	}
	f := d.frames.get(n)
	switch {
	case d.cfg.Codec != AAC && d.isLost(info.pts):
		clear(f.Samples) // a repeated packet stands in for a lost one
	case d.float:
		src := unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
		for i, v := range src {
			f.Samples[i] = clamp16(v)
		}
	default:
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&f.Samples[0])), 2*n), b)
	}
	f.SampleRate, f.Channels = d.rate, d.channels
	f.PTS = time.Duration(info.pts) * time.Microsecond
	d.lastFrames = n / d.channels
	d.produced, d.reopens = true, 0
	d.out.push(f)
	return nil
}

func (d *mcCodecDecoder) queueData(data []byte, pts time.Duration) error {
	idx, buf, err := d.acquireInput(d.drain)
	if err != nil {
		return d.recover(err)
	}
	size := len(data)
	if d.cfg.Codec == Vorbis {
		size += 4 // the codec reads a sample count after the packet, -1 is unknown
	}
	if size > len(buf) {
		d.queue(idx, 0, pts, 0)
		return fmt.Errorf("%w: packet exceeds codec buffer", ErrCorrupt)
	}
	copy(buf, data)
	if d.cfg.Codec == Vorbis {
		*(*int32)(unsafe.Pointer(&buf[len(data)])) = -1
	}
	if err := d.queue(idx, size, pts, 0); err != nil {
		return d.recover(err)
	}
	if cap(d.last) < len(data) {
		d.last = make([]byte, len(data), 2*len(data))
	}
	d.last = d.last[:len(data)]
	copy(d.last, data)
	if err := d.drain(); err != nil {
		return d.recover(err)
	}
	return nil
}

func (d *mcCodecDecoder) lose(pts time.Duration) error {
	if !d.opened || len(d.last) == 0 {
		// Nothing to repeat, and nothing in flight to stay ordered with.
		n := d.lastFrames * d.channels
		if n > 0 {
			f := d.frames.get(n)
			clear(f.Samples)
			f.SampleRate, f.Channels, f.PTS = d.rate, d.channels, pts
			d.out.push(f)
		}
		return nil
	}
	d.lost[d.lostNext] = int64(pts / time.Microsecond)
	d.lostNext = (d.lostNext + 1) % mcLostRing
	if d.cfg.Codec == FLAC {
		var si []byte
		if len(d.csd[0]) >= flacHeaderLen {
			si = d.csd[0][8:]
		}
		if f := flacSilentAfter(d.last, si); f != nil {
			return d.queueFLAC(f, pts)
		}
	}
	return d.queueData(d.last, pts)
}

func (d *mcCodecDecoder) Send(p Packet) error {
	switch {
	case d.closed:
		return ErrClosed
	case d.flushedInput:
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	if len(p.Data) == 0 {
		return d.lose(p.PTS)
	}
	data, pts := p.Data, p.PTS
	switch d.cfg.Codec {
	case AAC:
		if !adtsChain(data) {
			if !d.opened {
				return fmt.Errorf("%w: aac: no AudioSpecificConfig and no ADTS header", ErrCorrupt)
			}
			return d.queueData(data, pts)
		}
		for len(data) > 0 {
			h, _ := parseADTS(data)
			if !d.opened {
				d.csd[0] = h.config()
				_, d.rate, d.channels, _ = aacConfigInfo(d.csd[0])
				if err := d.start(); err != nil {
					return err
				}
			}
			if err := d.queueData(data[h.headerLen:h.frameLen], pts); err != nil {
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
			if err := d.queueData(frame, pts); err != nil {
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
			size, rate, _, _ := parseAC3(data)
			if err := d.queueData(data[:size], pts); err != nil {
				return err
			}
			data = data[size:]
			pts += audioDuration(ac3FrameSamples, rate)
		}
		return nil
	case Vorbis:
		if !d.opened {
			if !isVorbisHeaderPacket(data) {
				return fmt.Errorf("%w: vorbis: expected a header packet", ErrCorrupt)
			}
			d.headers = append(d.headers, append([]byte(nil), data...))
			if len(d.headers) == 3 {
				d.setVorbisHeaders(d.headers[0], d.headers[2])
				return d.start()
			}
			return nil
		}
		if data[0]&1 != 0 {
			if isVorbisHeaderPacket(data) {
				return nil
			}
			return fmt.Errorf("%w: vorbis: not an audio packet", ErrCorrupt)
		}
	case FLAC:
		if len(data) >= 4 && string(data[:4]) == "fLaC" {
			return nil
		}
		if !flacFrameValid(data) {
			return fmt.Errorf("%w: flac: damaged frame", ErrCorrupt)
		}
		return d.queueFLAC(data, pts)
	}
	return d.queueData(data, pts)
}

// queueFLAC passes a frame through the gap closer.
func (d *mcCodecDecoder) queueFLAC(frame []byte, pts time.Duration) error {
	return d.queueData(d.flacGaps.fix(frame), pts)
}

func isVorbisHeaderPacket(b []byte) bool {
	return len(b) >= 7 && b[0]&1 == 1 && string(b[1:7]) == "vorbis"
}

func (d *mcCodecDecoder) Receive() (*AudioFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	for {
		if f, ok := d.out.pop(); ok {
			return f, nil
		}
		if !d.opened {
			return nil, endOfQueue(d.flushedInput)
		}
		if d.finished {
			return nil, endOfQueue(true)
		}
		if err := d.pump(d.receiveWait(), d.onFormat, d.onData); err != nil {
			if rerr := d.recover(err); !errors.Is(rerr, ErrCorrupt) {
				return nil, rerr
			}
			d.finished = d.flushedInput
			continue
		}
		if d.out.head == len(d.out.items) && !d.mcCodec.flushed {
			return nil, ErrAgain
		}
	}
}

func (d *mcCodecDecoder) Flush() error {
	if d.closed {
		return ErrClosed
	}
	d.flushedInput = true
	if !d.opened {
		return nil
	}
	return d.sendEOS(d.drain)
}

// Wait implements Waiter.
func (d *mcCodecDecoder) Wait(t time.Duration) {
	if d.opened && d.out.head == len(d.out.items) && !d.finished {
		d.pump(mcMicros(t), d.onFormat, d.onData)
	}
}

func (d *mcCodecDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.mcCodec.Close()
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}

// --- encoder ---

type mcCodecEncoder struct {
	mcCodec
	cfg          AudioConfig
	packets      packetPool
	out          fifo[*EncodedPacket]
	framer       audioFramer
	config       []byte
	delay        time.Duration
	onData       func([]byte, *mcBufferInfo) error
	drain        func() error
	order        []int   // input channel order expected by the codec
	reordered    []int16 // scratch for it
	flushedInput bool
	closed       bool
}

// mcAACOrder maps the positions the AAC encoders expect (center first) to WAV
// positions. Their decoders output WAV order already.
var mcAACOrder = map[int][]int{
	3: {2, 0, 1}, 5: {2, 0, 1, 3, 4}, 6: {2, 0, 1, 4, 5, 3}, 8: {2, 0, 1, 6, 7, 4, 5, 3},
}

func newMCCodecEncoder(l *Library, cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	if cfg.Codec == FLAC && cfg.Channels > 2 {
		return nil, fmt.Errorf("%w: mediacodec: flac encoders take at most two channels", ErrInvalid)
	}
	e := &mcCodecEncoder{cfg: cfg}
	frames := 4096
	bitrate := cfg.Bitrate
	if cfg.Codec == AAC {
		frames = aacFrameSamples
		e.config = aacConfig(cfg.SampleRate, cfg.Channels)
		e.delay = audioDuration(mcAACDelay, cfg.SampleRate)
		e.order = mcAACOrder[cfg.Channels]
		if bitrate == 0 {
			bitrate = 64000 * cfg.Channels
		}
	} else if bitrate == 0 {
		bitrate = cfg.SampleRate * cfg.Channels * 16
	}
	if err := e.open(l, info, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "sample-rate", int32(cfg.SampleRate))
		a.setInt32(f, "channel-count", int32(cfg.Channels))
		a.setInt32(f, "bitrate", int32(bitrate))
		a.setInt32(f, "pcm-encoding", mcAudioPCM16)
		a.setInt32(f, "max-input-size", mcAudioMaxInput)
		if cfg.Codec == AAC {
			a.setInt32(f, "aac-profile", 2) // AAC-LC
		}
	}); err != nil {
		return nil, err
	}
	e.info.Hardware = false
	e.onData = e.packet
	e.drain = func() error { return e.pump(0, noFormat, e.onData) }
	e.framer.init(cfg, frames, e.feed)
	if e.order != nil {
		e.reordered = make([]int16, frames*cfg.Channels)
	}
	return e, nil
}

// CodecConfig implements CodecConfigurer. For FLAC it is available once the codec
// has produced its first output.
func (e *mcCodecEncoder) CodecConfig() []byte { return e.config }

func (e *mcCodecEncoder) packet(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 {
		e.config = append(e.config[:0], b...)
		return nil
	}
	p := e.packets.get(len(b))
	copy(p.Data, b)
	p.PTS, p.Keyframe = time.Duration(info.pts)*time.Microsecond-e.delay, true
	e.out.push(p)
	return nil
}

func (e *mcCodecEncoder) feed(pcm []int16, pts time.Duration) error {
	if e.order != nil {
		ch := e.cfg.Channels
		out := e.reordered[:len(pcm)]
		for i := 0; i+ch <= len(pcm); i += ch {
			for c, from := range e.order {
				out[i+c] = pcm[i+from]
			}
		}
		pcm = out
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(&pcm[0])), 2*len(pcm))
	perSecond := e.cfg.SampleRate * e.cfg.Channels * 2
	for len(b) > 0 {
		idx, buf, err := e.acquireInput(e.drain)
		if err != nil {
			return err
		}
		n := copy(buf, b)
		if err := e.queue(idx, n, pts, 0); err != nil {
			return err
		}
		b = b[n:]
		pts += time.Duration(n) * time.Second / time.Duration(perSecond)
	}
	return e.drain()
}

func (e *mcCodecEncoder) Send(f *AudioFrame) error {
	switch {
	case e.closed:
		return ErrClosed
	case e.flushedInput:
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	case f.SampleRate != e.cfg.SampleRate || f.Channels != e.cfg.Channels || len(f.Samples)%e.cfg.Channels != 0:
		return fmt.Errorf("%w: audio frame does not match encoder format", ErrInvalid)
	}
	return e.framer.write(f)
}

func (e *mcCodecEncoder) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	for {
		if p, ok := e.out.pop(); ok {
			return p, nil
		}
		if e.finished {
			return nil, endOfQueue(true)
		}
		if err := e.pump(e.receiveWait(), noFormat, e.onData); err != nil {
			return nil, err
		}
		if e.out.head == len(e.out.items) && !e.mcCodec.flushed {
			return nil, ErrAgain
		}
	}
}

// Flush pads the last AAC frame with silence, and drains the codec.
func (e *mcCodecEncoder) Flush() error {
	if e.closed {
		return ErrClosed
	}
	if e.flushedInput {
		return nil
	}
	e.flushedInput = true
	if err := e.framer.flush(e.cfg.Codec == AAC); err != nil {
		return err
	}
	return e.sendEOS(e.drain)
}

// Wait implements Waiter.
func (e *mcCodecEncoder) Wait(t time.Duration) {
	if e.out.head == len(e.out.items) && !e.finished {
		e.pump(mcMicros(t), noFormat, e.onData)
	}
}

func (e *mcCodecEncoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	e.mcCodec.Close()
	for {
		p, ok := e.out.pop()
		if !ok {
			return nil
		}
		p.Release()
	}
}
