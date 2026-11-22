package gaal

import (
	"fmt"
	"time"
)

// Audio codecs other than Opus. No codec is implemented here except G.711:
// everything else is reached through the operating system or through a library
// that is already installed. AAC, MP3 and AC-3 are never shipped by this module.
//
// # Packet formats
//
// One Packet carries, per codec:
//
//	AAC     one raw access unit, with AudioConfig.Extra holding the
//	        AudioSpecificConfig. Without Extra, one or more ADTS frames, and the
//	        configuration comes from the first header. Encoders output raw access
//	        units and return the AudioSpecificConfig from CodecConfig.
//	MP3     one or more complete MPEG audio frames, starting at a sync word. Xing,
//	        Info and VBRI tag frames are skipped.
//	FLAC    one complete frame (libFLAC also takes several). Extra is optional: "fLaC" plus metadata
//	        blocks, metadata blocks alone, or a bare 34-byte STREAMINFO. CodecConfig
//	        returns "fLaC" plus the STREAMINFO block. A packet starting with "fLaC"
//	        is taken as such a header.
//	Vorbis  one audio packet. Extra holds the three headers in Xiph lacing, which
//	        is what CodecConfig returns. Without Extra the first three packets must
//	        be the headers. The first audio packet decodes to no samples.
//	ALAC    one frame, with Extra holding the magic cookie: the 24-byte
//	        ALACSpecificConfig, optionally inside its 'alac' atom.
//	AC3     one or more complete syncframes.
//	PCMU,
//	PCMA    any whole number of G.711 samples, interleaved, one byte each.
//
// # Encoders
//
// Encoders accept any number of samples and buffer them into codec frames. Flush
// pads the last frame of a lossy codec with silence and drains the codec. Lossless
// codecs and G.711 keep the exact length.
//
// # Timestamps
//
// The PTS of an encoded packet is the presentation time of the first sample its
// decoder outputs, so the codec delay is already accounted for. Lossy encoders
// therefore start with negative timestamps: output before time zero is pre-roll
// that a player drops. Decoders stamp output with the PTS of its packet.
//
// # Lost packets
//
// An empty Packet reports a loss. Where the codec can conceal it, it does: AAC
// through libfdk-aac, and AAC on Android and Windows, whose decoders are given the
// previous access unit again. Everything else outputs silence as long as the last
// frame.

// audioBackends lists the backends of these codecs on this system, best first.
func audioBackends() []backend {
	out := osAudioBackends()
	out = append(out, libAudioBackends()...)
	return append(out, g711Backends()...)
}

const audioPTSRing = 128

// ptsRing remembers the timestamps of frames inside a codec. Past its contents
// it extrapolates by step.
type ptsRing struct {
	v       [audioPTSRing]time.Duration
	head, n int
	last    time.Duration
	step    time.Duration
	used    bool
}

func (r *ptsRing) push(t time.Duration) {
	if r.n == audioPTSRing {
		r.head = (r.head + 1) % audioPTSRing
		r.n--
	}
	r.v[(r.head+r.n)%audioPTSRing] = t
	r.n++
}

func (r *ptsRing) pop() time.Duration {
	if r.n == 0 {
		if r.used {
			r.last += r.step
		}
		r.used = true
		return r.last
	}
	r.last = r.v[r.head]
	r.head = (r.head + 1) % audioPTSRing
	r.n--
	r.used = true
	return r.last
}

func audioDuration(frames, rate int) time.Duration {
	return time.Duration(int64(frames) * int64(time.Second) / int64(rate))
}

// audioFramer cuts arbitrary input into codec frames.
type audioFramer struct {
	rate, channels int
	size           int // interleaved samples per codec frame
	pending        []int16
	origin         time.Duration
	count          int // frames per channel since origin
	emit           func(pcm []int16, pts time.Duration) error
}

func (a *audioFramer) init(cfg AudioConfig, frames int, emit func([]int16, time.Duration) error) {
	a.rate, a.channels, a.size, a.emit = cfg.SampleRate, cfg.Channels, frames*cfg.Channels, emit
	a.pending = make([]int16, 0, a.size)
}

func (a *audioFramer) pts() time.Duration { return a.origin + audioDuration(a.count, a.rate) }

func (a *audioFramer) put(pcm []int16) error {
	pts := a.pts()
	a.count += len(pcm) / a.channels
	return a.emit(pcm, pts)
}

func (a *audioFramer) write(f *AudioFrame) error {
	samples := f.Samples
	if len(samples) == 0 {
		return nil
	}
	if len(a.pending) == 0 {
		a.origin, a.count = f.PTS, 0
	} else {
		n := min(a.size-len(a.pending), len(samples))
		a.pending = append(a.pending, samples[:n]...)
		samples = samples[n:]
		if len(a.pending) < a.size {
			return nil
		}
		err := a.put(a.pending)
		a.pending = a.pending[:0]
		if err != nil {
			return err
		}
	}
	for len(samples) >= a.size {
		if err := a.put(samples[:a.size]); err != nil {
			return err
		}
		samples = samples[a.size:]
	}
	a.pending = append(a.pending, samples...)
	return nil
}

// flush emits what is left, padded with silence to a whole frame if pad is set.
func (a *audioFramer) flush(pad bool) error {
	if len(a.pending) == 0 {
		return nil
	}
	if pad {
		n := len(a.pending)
		a.pending = a.pending[:a.size]
		clear(a.pending[n:])
	}
	err := a.put(a.pending)
	a.pending = a.pending[:0]
	return err
}

// audioEncBase is the part shared by audio encoders.
type audioEncBase struct {
	cfg     AudioConfig
	info    BackendInfo
	packets packetPool
	out     fifo[*EncodedPacket]
	framer  audioFramer
	times   ptsRing
	config  []byte
	flushed bool
	closed  bool
}

func (e *audioEncBase) checkSend(f *AudioFrame) error {
	switch {
	case e.closed:
		return ErrClosed
	case e.flushed:
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	case f.SampleRate != e.cfg.SampleRate || f.Channels != e.cfg.Channels || len(f.Samples)%e.cfg.Channels != 0:
		return fmt.Errorf("%w: audio frame does not match encoder format", ErrInvalid)
	}
	return nil
}

func (e *audioEncBase) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *audioEncBase) Backend() BackendInfo { return e.info }

// CodecConfig implements CodecConfigurer.
func (e *audioEncBase) CodecConfig() []byte { return e.config }

func (e *audioEncBase) dropOutput() {
	for {
		p, ok := e.out.pop()
		if !ok {
			return
		}
		p.Release()
	}
}

// audioDecBase is the part shared by audio decoders.
type audioDecBase struct {
	cfg        AudioConfig
	info       BackendInfo
	frames     audioPool
	out        fifo[*AudioFrame]
	rate       int // of the stream, once known
	channels   int
	lastFrames int // samples per channel of the latest output
	flushed    bool
	closed     bool
}

func (d *audioDecBase) initDec(cfg AudioConfig, info BackendInfo, defaultFrames int) {
	d.cfg, d.info, d.rate, d.channels, d.lastFrames = cfg, info, cfg.SampleRate, cfg.Channels, defaultFrames
}

func (d *audioDecBase) checkSend() error {
	switch {
	case d.closed:
		return ErrClosed
	case d.flushed:
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	return nil
}

func (d *audioDecBase) Receive() (*AudioFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *audioDecBase) Flush() error         { d.flushed = true; return nil }
func (d *audioDecBase) Backend() BackendInfo { return d.info }

func (d *audioDecBase) dropOutput() {
	for {
		f, ok := d.out.pop()
		if !ok {
			return
		}
		f.Release()
	}
}

// push queues f, which holds frames samples per channel.
func (d *audioDecBase) push(f *AudioFrame, frames int, pts time.Duration) {
	f.Samples = f.Samples[:frames*d.channels]
	f.SampleRate, f.Channels, f.PTS = d.rate, d.channels, pts
	d.lastFrames = frames
	d.out.push(f)
}

// silence outputs one frame of silence for a lost packet.
func (d *audioDecBase) silence(pts time.Duration) {
	n := d.lastFrames * d.channels
	if n == 0 {
		return
	}
	f := d.frames.get(n)
	clear(f.Samples)
	d.push(f, d.lastFrames, pts)
}

func clamp16(v float32) int16 {
	v *= 32768
	switch {
	case v >= 32767:
		return 32767
	case v <= -32768:
		return -32768
	case v >= 0:
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}

// --- AAC configuration and ADTS ---

var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// aacChannelConfig maps a channel count to channelConfiguration. Seven channels
// have no configuration of their own in the common profiles.
func aacChannelConfig(channels int) int {
	switch {
	case channels >= 1 && channels <= 6:
		return channels
	case channels == 8:
		return 7
	}
	return 0
}

// aacConfig builds the AudioSpecificConfig of an AAC-LC stream.
func aacConfig(rate, channels int) []byte {
	const lc = 2
	for i, r := range aacRates {
		if r == rate {
			v := lc<<11 | i<<7 | aacChannelConfig(channels)<<3
			return []byte{byte(v >> 8), byte(v)}
		}
	}
	v := uint64(lc)<<35 | 15<<31 | uint64(rate)<<7 | uint64(aacChannelConfig(channels))<<3
	return []byte{byte(v >> 32), byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// aacConfigInfo reads the object type, rate and channels of an
// AudioSpecificConfig. Channels is 0 when a program config element sets them.
func aacConfigInfo(b []byte) (aot, rate, channels int, ok bool) {
	if len(b) < 2 {
		return 0, 0, 0, false
	}
	pos := 0
	bits := func(n int) int {
		v := 0
		for ; n > 0; n-- {
			if pos>>3 >= len(b) {
				ok = false
				return 0
			}
			v = v<<1 | int(b[pos>>3]>>(7-pos&7))&1
			pos++
		}
		return v
	}
	ok = true
	aot = bits(5)
	if aot == 31 {
		aot = 32 + bits(6)
	}
	if i := bits(4); i == 15 {
		rate = bits(24)
	} else if i < len(aacRates) {
		rate = aacRates[i]
	} else {
		return 0, 0, 0, false
	}
	channels = bits(4)
	if channels == 7 {
		channels = 8
	}
	return aot, rate, channels, ok && rate > 0
}

// adtsHeader describes one ADTS frame.
type adtsHeader struct {
	headerLen, frameLen int
	profile             int // object type minus one
	rateIndex           int
	channelConfig       int
}

// parseADTS reads the header at the start of b. It reports false unless b starts
// with a valid header and holds the whole frame.
func parseADTS(b []byte) (h adtsHeader, ok bool) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return h, false
	}
	h.headerLen = 7
	if b[1]&1 == 0 {
		h.headerLen = 9
	}
	h.profile = int(b[2] >> 6)
	h.rateIndex = int(b[2] >> 2 & 15)
	h.channelConfig = int(b[2]&1)<<2 | int(b[3]>>6)
	h.frameLen = int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5]>>5)
	if h.rateIndex >= len(aacRates) || h.frameLen <= h.headerLen || h.frameLen > len(b) {
		return h, false
	}
	return h, true
}

func (h adtsHeader) config() []byte {
	v := (h.profile+1)<<11 | h.rateIndex<<7 | h.channelConfig<<3
	return []byte{byte(v >> 8), byte(v)}
}

// adtsChain reports whether b is entirely made of ADTS frames.
func adtsChain(b []byte) bool {
	for len(b) > 0 {
		h, ok := parseADTS(b)
		if !ok {
			return false
		}
		b = b[h.frameLen:]
	}
	return true
}

// --- MPEG audio frame headers ---

var (
	mp3BitratesV1 = [16]uint16{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp3BitratesV2 = [16]uint16{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mp3Rates      = [4]uint16{44100, 48000, 32000, 0}
)

// mp3Frame describes one MPEG-1, 2 or 2.5 Layer III frame.
type mp3Frame struct {
	size, samples, rate, channels int
	sideInfo                      int // bytes between the header and the main data
}

// parseMP3 reads the frame header at the start of b.
func parseMP3(b []byte) (f mp3Frame, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return f, false
	}
	version := b[1] >> 3 & 3 // 0: 2.5, 2: 2, 3: 1
	layer := b[1] >> 1 & 3
	bitrate := b[2] >> 4
	rate := int(mp3Rates[b[2]>>2&3])
	if version == 1 || layer != 1 || bitrate == 0 || bitrate == 15 || rate == 0 {
		return f, false
	}
	pad := int(b[2] >> 1 & 1)
	f.channels = 2
	if b[3]>>6 == 3 {
		f.channels = 1
	}
	if version == 3 {
		f.rate, f.samples = rate, 1152
		f.size = 144000*int(mp3BitratesV1[bitrate])/rate + pad
		f.sideInfo = 32
		if f.channels == 1 {
			f.sideInfo = 17
		}
	} else {
		f.rate = rate / 2
		if version == 0 {
			f.rate = rate / 4
		}
		f.samples = 576
		f.size = 72000*int(mp3BitratesV2[bitrate])/f.rate + pad
		f.sideInfo = 17
		if f.channels == 1 {
			f.sideInfo = 9
		}
	}
	if b[1]&1 == 0 {
		f.sideInfo += 2 // CRC
	}
	return f, true
}

// isTag reports whether the frame b holds a Xing, Info or VBRI tag, not audio.
func (f mp3Frame) isTag(b []byte) bool {
	at := func(off int, s string) bool {
		return off+len(s) <= len(b) && string(b[off:off+len(s)]) == s
	}
	off := 4 + f.sideInfo
	return at(off, "Xing") || at(off, "Info") || at(4+32, "VBRI")
}

// --- AC-3 ---

const (
	ac3FrameSamples  = 1536
	aacFrameSamples  = 1024
	flacDefaultBlock = 4096
)

var ac3Bitrates = [19]uint16{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

// parseAC3 returns the length, rate and full-bandwidth plus LFE channel count of
// the syncframe at the start of b.
func parseAC3(b []byte) (size, rate, channels int, ok bool) {
	if len(b) < 7 || b[0] != 0x0B || b[1] != 0x77 {
		return 0, 0, 0, false
	}
	fscod, code := int(b[4]>>6), int(b[4]&63)
	if fscod == 3 || code>>1 >= len(ac3Bitrates) || b[5]>>3 > 10 {
		return 0, 0, 0, false
	}
	br := int(ac3Bitrates[code>>1])
	switch fscod {
	case 0:
		size, rate = 4*br, 48000
	case 1:
		size, rate = 2*(br*320/147+code&1), 44100
	case 2:
		size, rate = 6*br, 32000
	}
	acmod := int(b[6] >> 5)
	channels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}[acmod]
	// lfeon follows cmixlev, surmixlev and dsurmod, which depend on acmod.
	pos := 3
	if acmod&1 != 0 && acmod != 1 {
		pos += 2
	}
	if acmod&4 != 0 {
		pos += 2
	}
	if acmod == 2 {
		pos += 2
	}
	if b[6]>>(7-pos)&1 != 0 {
		channels++
	}
	return size, rate, channels, true
}

// ac3CRCValid checks the CRC that covers a whole syncframe.
func ac3CRCValid(frame []byte) bool {
	crc := uint16(0)
	for _, v := range frame[2:] {
		crc ^= uint16(v) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc == 0
}

// --- Xiph lacing, used for the Vorbis headers ---

func xiphLace(parts ...[]byte) []byte {
	out := []byte{byte(len(parts) - 1)}
	for _, p := range parts[:len(parts)-1] {
		n := len(p)
		for ; n >= 255; n -= 255 {
			out = append(out, 255)
		}
		out = append(out, byte(n))
	}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func xiphUnlace(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	count := int(b[0]) + 1
	b = b[1:]
	sizes := make([]int, count)
	total := 0
	for i := 0; i < count-1; i++ {
		for {
			if len(b) == 0 {
				return nil
			}
			v := int(b[0])
			b = b[1:]
			sizes[i] += v
			if v != 255 {
				break
			}
		}
		total += sizes[i]
	}
	if total > len(b) {
		return nil
	}
	sizes[count-1] = len(b) - total
	out := make([][]byte, count)
	for i, n := range sizes {
		out[i], b = b[:n], b[n:]
	}
	return out
}

// flacFrameValid checks the sync code and the CRC-16 ending a frame.
func flacFrameValid(b []byte) bool {
	if len(b) < 6 || b[0] != 0xFF || b[1]&0xFE != 0xF8 {
		return false
	}
	return flacCRC16(b) == 0
}

// flacHeader is what a FLAC frame header says, as far as gaal needs it.
type flacHeader struct {
	variable bool   // blocking strategy: num counts samples, not frames
	num      uint64 // frame or sample number
	numEnd   int    // end of the number in the frame
	headEnd  int    // end of the header before its CRC-8
	block    uint64 // samples per channel
	channels int
	bps      int // 0 if the header leaves it to STREAMINFO
}

func parseFLACHeader(b []byte) (h flacHeader, ok bool) {
	if !flacFrameValid(b) || len(b) < 5 {
		return h, false
	}
	h.variable = b[1]&1 != 0
	sizeCode, rateCode := b[2]>>4, b[2]&0x0F
	chanCode, bpsCode := b[3]>>4, b[3]>>1&7
	var n int
	if h.num, n = flacUTF8(b[4:]); n == 0 {
		return h, false
	}
	h.numEnd = 4 + n
	h.headEnd = h.numEnd + [16]int{6: 1, 7: 2}[sizeCode] + [16]int{12: 1, 13: 2, 14: 2}[rateCode]
	if len(b) < h.headEnd+3 {
		return h, false
	}
	pos := h.numEnd
	switch {
	case sizeCode == 1:
		h.block = 192
	case sizeCode >= 2 && sizeCode <= 5:
		h.block = 576 << (sizeCode - 2)
	case sizeCode == 6:
		h.block = uint64(b[pos]) + 1
	case sizeCode == 7:
		h.block = uint64(b[pos])<<8 | uint64(b[pos+1]) + 1
	case sizeCode >= 8:
		h.block = 256 << (sizeCode - 8)
	default:
		return h, false
	}
	switch {
	case chanCode <= 7:
		h.channels = int(chanCode) + 1
	case chanCode <= 10:
		h.channels = 2
	default:
		return h, false
	}
	h.bps = [8]int{0, 8, 12, 0, 16, 20, 24, 32}[bpsCode]
	return h, true
}

// next is the number of the frame that follows.
func (h flacHeader) next() uint64 {
	if h.variable {
		return h.num + h.block
	}
	return h.num + 1
}

// flacRenumber appends frame to dst with its frame or sample number replaced
// by num and both CRCs updated. The audio data is unchanged.
func flacRenumber(dst, frame []byte, h flacHeader, num uint64) []byte {
	at := len(dst)
	dst = append(dst, frame[:4]...)
	dst = flacPutUTF8(dst, num)
	dst = append(dst, frame[h.numEnd:h.headEnd]...)
	dst = append(dst, flacCRC8(dst[at:]))
	dst = append(dst, frame[h.headEnd+1:len(frame)-2]...)
	crc := flacCRC16(dst[at:])
	return append(dst, byte(crc>>8), byte(crc))
}

// flacGapCloser keeps the frame numbers a FLAC decoder sees free of forward
// jumps, which loss and damage cause. Android's decoder fails on one, and the
// Windows one makes up frames for the gap, which a caller cannot time. A
// repeated number or a restart is passed on unchanged.
type flacGapCloser struct {
	next    uint64 // the frame or sample number expected
	started bool
	buf     []byte // the last renumbered frame
}

// fix returns frame, or a copy renumbered to close a gap, valid until the
// next call.
func (g *flacGapCloser) fix(frame []byte) []byte {
	h, ok := parseFLACHeader(frame)
	if !ok {
		return frame
	}
	num := h.num
	if g.started && num > g.next {
		num = g.next
		g.buf = flacRenumber(g.buf[:0], frame, h, num)
		frame = g.buf
	}
	g.next, g.started = num+h.next()-h.num, true
	return frame
}

// flacSilentAfter builds a silent frame that follows the frame prev, for a
// decoder to take in place of a lost one. Repeating prev instead would give
// the decoder a frame number it has already seen. streamInfo is the 34-byte
// STREAMINFO block or nil; it is needed only when prev's header leaves the
// sample size to it. It returns nil if prev is not a frame it can follow.
func flacSilentAfter(prev, streamInfo []byte) []byte {
	h, ok := parseFLACHeader(prev)
	if !ok {
		return nil
	}
	bps := h.bps
	if bps == 0 && len(streamInfo) >= 14 {
		bps = int(streamInfo[12]&1)<<4 | int(streamInfo[13]>>4) + 1
	}
	if bps == 0 {
		return nil
	}
	// Independent channels, so every subframe has the stream's sample size.
	out := []byte{0xFF, prev[1], prev[2], byte(h.channels-1)<<4 | prev[3]&0x0F}
	out = flacPutUTF8(out, h.next())
	out = append(out, prev[h.numEnd:h.headEnd]...)
	out = append(out, flacCRC8(out))
	// Each subframe is a zero header byte (CONSTANT) and a zero sample.
	out = append(out, make([]byte, (h.channels*(8+bps)+7)/8)...)
	crc := flacCRC16(out)
	return append(out, byte(crc>>8), byte(crc))
}

// flacUTF8 reads the extended UTF-8 number of a frame header.
func flacUTF8(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	if b[0] < 0x80 {
		return uint64(b[0]), 1
	}
	n := 0
	for m := byte(0x80); b[0]&m != 0 && n < 8; m >>= 1 {
		n++
	}
	if n < 2 || n > 7 || len(b) < n {
		return 0, 0
	}
	v := uint64(b[0] & (0xFF >> (n + 1)))
	for _, c := range b[1:n] {
		if c&0xC0 != 0x80 {
			return 0, 0
		}
		v = v<<6 | uint64(c&0x3F)
	}
	return v, n
}

func flacPutUTF8(dst []byte, v uint64) []byte {
	if v < 0x80 {
		return append(dst, byte(v))
	}
	n := 2
	for n < 7 && v >= 1<<(5*n+1) {
		n++
	}
	dst = append(dst, byte(uint16(0xFF00)>>n)|byte(v>>(6*(n-1))))
	for i := n - 2; i >= 0; i-- {
		dst = append(dst, 0x80|byte(v>>(6*i))&0x3F)
	}
	return dst
}

func flacCRC8(b []byte) byte {
	crc := byte(0)
	for _, v := range b {
		crc ^= v
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func flacCRC16(b []byte) uint16 {
	crc := uint16(0)
	for _, v := range b {
		crc ^= uint16(v) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// flacHeaderComplete reports whether b, "fLaC" and metadata blocks, ends with
// the last block and holds STREAMINFO first.
func flacHeaderComplete(b []byte) bool {
	if len(b) < 4+4+34 || string(b[:4]) != "fLaC" || b[4]&0x7F != 0 {
		return false
	}
	for pos := 4; pos+4 <= len(b); {
		n := int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
		if pos+4+n > len(b) {
			return false
		}
		if b[pos]&0x80 != 0 {
			return pos+4+n == len(b)
		}
		pos += 4 + n
	}
	return false
}

// flacConfigBlocks normalises FLAC setup data to "fLaC" plus metadata blocks. It
// returns nil if b is not recognised.
func flacConfigBlocks(b []byte) []byte {
	const streamInfoLen = 34
	switch {
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return b
	case len(b) == streamInfoLen:
		return append([]byte{'f', 'L', 'a', 'C', 0x80, 0, 0, streamInfoLen}, b...)
	case len(b) >= 4+streamInfoLen && b[0]&0x7F == 0 && int(b[3]) == streamInfoLen:
		return append([]byte("fLaC"), b...)
	}
	return nil
}
