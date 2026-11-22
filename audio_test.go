package gaal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// audioFixtures holds streams made by other encoders, for decoders that have no
// encoder here. A build can supply them through -overlay. On Linux they are made
// with ffmpeg when it is installed.
var audioFixtures map[string][]byte

type audioCase struct {
	codec    Codec
	rate, ch int
	bitrate  int
	lossless bool
}

func (c audioCase) String() string { return fmt.Sprintf("%s_%d_%d", c.codec, c.rate, c.ch) }

func (c audioCase) config() AudioConfig {
	return AudioConfig{Codec: c.codec, SampleRate: c.rate, Channels: c.ch, Bitrate: c.bitrate}
}

var audioCases = []audioCase{
	{codec: PCMU, rate: 8000, ch: 1},
	{codec: PCMA, rate: 8000, ch: 1},
	{codec: PCMU, rate: 16000, ch: 2},
	{codec: PCMA, rate: 48000, ch: 2},
	{codec: AAC, rate: 48000, ch: 2, bitrate: 128000},
	{codec: AAC, rate: 44100, ch: 2, bitrate: 128000},
	{codec: AAC, rate: 44100, ch: 1, bitrate: 64000},
	{codec: AAC, rate: 48000, ch: 6, bitrate: 384000},
	{codec: MP3, rate: 44100, ch: 2, bitrate: 128000},
	{codec: MP3, rate: 48000, ch: 2, bitrate: 192000},
	{codec: MP3, rate: 22050, ch: 1, bitrate: 48000},
	{codec: FLAC, rate: 48000, ch: 2, lossless: true},
	{codec: FLAC, rate: 44100, ch: 1, lossless: true},
	{codec: FLAC, rate: 48000, ch: 6, lossless: true},
	{codec: ALAC, rate: 44100, ch: 2, lossless: true},
	{codec: ALAC, rate: 48000, ch: 1, lossless: true},
	{codec: Vorbis, rate: 48000, ch: 2},
	{codec: Vorbis, rate: 44100, ch: 1},
	{codec: Vorbis, rate: 44100, ch: 6},
}

var toneFreqs = [8]float64{1000, 1500, 700, 60, 2100, 2600, 400, 3000}

const (
	audioTestSeconds = 3
	toneStart        = 250 * time.Millisecond
	toneAmp          = 0.4
)

func toneFreq(ch, rate int) float64 { return min(toneFreqs[ch], float64(rate)/4) }

// tonePCM is silence up to toneStart, then one sine per channel.
func tonePCM(rate, channels int, seconds float64) []int16 {
	n := int(float64(rate) * seconds)
	start := int(toneStart.Seconds() * float64(rate))
	pcm := make([]int16, n*channels)
	for i := start; i < n; i++ {
		for c := 0; c < channels; c++ {
			w := 2 * math.Pi * toneFreq(c, rate) * float64(i-start) / float64(rate)
			pcm[i*channels+c] = int16(toneAmp * 32767 * math.Sin(w))
		}
	}
	return pcm
}

// toneScore returns the share of the energy of one channel at its tone, and the
// RMS relative to the input tone.
func toneScore(pcm []int16, channels, ch, rate int) (share, rms float64) {
	f := toneFreq(ch, rate)
	var total, re, im float64
	n := len(pcm) / channels
	for i := 0; i < n; i++ {
		v := float64(pcm[i*channels+ch]) / 32767
		w := 2 * math.Pi * f * float64(i) / float64(rate)
		total += v * v
		re += v * math.Cos(w)
		im += v * math.Sin(w)
	}
	if total == 0 || n == 0 {
		return 0, 0
	}
	power := 2 * (re*re + im*im) / float64(n)
	return power / total, math.Sqrt(total/float64(n)) / (toneAmp / math.Sqrt2)
}

type decoded struct {
	pcm      []int16
	rate, ch int
	first    time.Duration
	frames   int
	sizes    []int
}

// sampleAt converts a time on the PTS timeline to an index into pcm.
func (d *decoded) sampleAt(t time.Duration) int {
	return int((t-d.first).Seconds()*float64(d.rate)+0.5) * d.ch
}

type frameSink struct {
	t       testing.TB
	out     decoded
	last    time.Duration
	started bool
}

func (s *frameSink) take(f *AudioFrame) {
	t := s.t
	t.Helper()
	if !s.started {
		s.started, s.out.first, s.out.rate, s.out.ch = true, f.PTS, f.SampleRate, f.Channels
	} else {
		if f.PTS < s.last {
			t.Errorf("frame PTS went backwards: %v after %v", f.PTS, s.last)
		}
		if f.SampleRate != s.out.rate || f.Channels != s.out.ch {
			t.Errorf("format changed to %d Hz, %d channels", f.SampleRate, f.Channels)
		}
	}
	s.last = f.PTS
	s.out.pcm = append(s.out.pcm, f.Samples...)
	s.out.frames++
	if f.Channels > 0 {
		s.out.sizes = append(s.out.sizes, len(f.Samples)/f.Channels)
	}
	f.Release()
}

func drainNow[In, Out any](t testing.TB, s Stream[In, Out], fn func(Out)) {
	t.Helper()
	for {
		v, err := s.Receive()
		switch {
		case err == nil:
			fn(v)
		case errors.Is(err, ErrAgain), errors.Is(err, io.EOF):
			return
		case errors.Is(err, ErrCorrupt):
		default:
			t.Fatalf("receive: %v", err)
		}
	}
}

func drainEnd[In, Out any](t testing.TB, s Stream[In, Out], fn func(Out)) {
	t.Helper()
	for {
		v, err := ReceiveWait(s, 10*time.Second)
		switch {
		case err == nil:
			fn(v)
		case errors.Is(err, io.EOF):
			return
		case errors.Is(err, ErrCorrupt):
			// A stream that never produced output keeps reporting its input error.
			return
		default:
			t.Fatalf("receive after flush: %v", err)
		}
	}
}

type encoded struct {
	packets []Packet
	config  []byte
	bytes   int
	took    time.Duration
}

func encodeAudio(t testing.TB, l *Library, cfg AudioConfig, pcm []int16) *encoded {
	t.Helper()
	enc, err := l.NewAudioEncoder(cfg)
	if err != nil {
		if errors.Is(err, ErrNoBackend) || errors.Is(err, ErrInvalid) {
			t.Skipf("encoder does not take this format: %v", err)
		}
		t.Fatal(err)
	}
	defer enc.Close()
	r := &encoded{}
	var last time.Duration
	take := func(p *EncodedPacket) {
		if len(p.Data) == 0 {
			t.Error("empty encoded packet")
		}
		if len(r.packets) > 0 && p.PTS < last {
			t.Errorf("packet PTS went backwards: %v after %v", p.PTS, last)
		}
		last = p.PTS
		r.bytes += len(p.Data)
		r.packets = append(r.packets, clonePacket(p.Packet))
		p.Release()
	}
	start := time.Now()
	// Odd chunk sizes exercise the buffering of the encoder.
	chunks := []int{cfg.SampleRate / 100, 1, 777, cfg.SampleRate / 50, 4096, 13}
	f := &AudioFrame{SampleRate: cfg.SampleRate, Channels: cfg.Channels}
	frames := len(pcm) / cfg.Channels
	for pos, i := 0, 0; pos < frames; i++ {
		n := min(chunks[i%len(chunks)], frames-pos)
		f.Samples, f.PTS = pcm[pos*cfg.Channels:(pos+n)*cfg.Channels], audioDuration(pos, cfg.SampleRate)
		if err := enc.Send(f); err != nil {
			t.Fatalf("encode: %v", err)
		}
		pos += n
		drainNow(t, enc, take)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	drainEnd(t, enc, take)
	r.took = time.Since(start)
	if c, ok := enc.(CodecConfigurer); ok {
		r.config = append([]byte(nil), c.CodecConfig()...)
	}
	return r
}

// decodeAudio runs packets through a decoder. edit may replace a packet.
func decodeAudio(t testing.TB, l *Library, cfg AudioConfig, packets []Packet, edit func(i int, p Packet) Packet) (*decoded, int, time.Duration) {
	t.Helper()
	dec, err := l.NewAudioDecoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	sink := &frameSink{t: t}
	corrupt := 0
	start := time.Now()
	for i, p := range packets {
		if edit != nil {
			p = edit(i, p)
		}
		if err := dec.Send(p); err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("decode packet %d: %v", i, err)
			}
			corrupt++
		}
		drainNow(t, dec, sink.take)
	}
	if err := dec.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	drainEnd(t, dec, sink.take)
	return &sink.out, corrupt, time.Since(start)
}

// checkTone verifies the tone in every channel, skipping the attack and the end.
func checkTone(t testing.TB, d *decoded, rate, channels int, minShare float64) {
	t.Helper()
	if d.rate != rate || d.ch != channels {
		t.Fatalf("decoded %d Hz %d channels, want %d Hz %d", d.rate, d.ch, rate, channels)
	}
	from := d.sampleAt(toneStart + 500*time.Millisecond)
	to := min(len(d.pcm), from+d.ch*rate)
	if from < 0 || to <= from {
		t.Fatalf("decoded only %d samples from %v", len(d.pcm)/d.ch, d.first)
	}
	seg := d.pcm[from:to]
	for c := 0; c < channels; c++ {
		if channels == 6 && c == 3 {
			continue // LFE
		}
		share, rms := toneScore(seg, channels, c, rate)
		if share < minShare || rms < 0.7 || rms > 1.3 {
			t.Errorf("channel %d: %.3f of energy at %.0f Hz, RMS ratio %.3f", c, share, toneFreq(c, rate), rms)
		}
	}
}

// onset finds when channel 0 starts, on the PTS timeline.
func onset(d *decoded) time.Duration {
	limit := int16(6553)
	for i := 0; i < len(d.pcm); i += d.ch {
		if v := d.pcm[i]; v > limit || v < -limit {
			return d.first + audioDuration(i/d.ch, d.rate)
		}
	}
	return -1
}

type audioResult struct {
	codec, pair string
	enc, dec    float64 // times real time
	kbps        float64
	note        string
}

var (
	resultsMu    sync.Mutex
	audioResults []audioResult
)

func report(r audioResult) {
	resultsMu.Lock()
	audioResults = append(audioResults, r)
	resultsMu.Unlock()
}

// asyncBackend reports a backend whose output can lag its input.
func asyncBackend(name string) bool { return strings.HasPrefix(name, "mediacodec") }

func TestAudioRoundTrip(t *testing.T) {
	base := New(Options{})
	for _, c := range audioCases {
		encs, decs := base.Backends(c.codec, Encode), base.Backends(c.codec, Decode)
		for _, eb := range encs {
			for _, db := range decs {
				t.Run(fmt.Sprintf("%s/%s+%s", c, eb.Name, db.Name), func(t *testing.T) {
					roundTrip(t, c, eb, db)
				})
			}
		}
	}
}

func roundTrip(t *testing.T, c audioCase, eb, db BackendInfo) {
	cfg := c.config()
	pcm := tonePCM(c.rate, c.ch, audioTestSeconds)
	el, dl := New(Options{Backend: eb.Name}), New(Options{Backend: db.Name})
	e := encodeAudio(t, el, cfg, pcm)
	if len(e.packets) == 0 {
		t.Fatal("no packets")
	}
	dcfg := cfg
	dcfg.Bitrate, dcfg.Extra = 0, e.config
	out, corrupt, took := decodeAudio(t, dl, dcfg, e.packets, nil)
	if corrupt != 0 {
		t.Errorf("%d packets reported as corrupt", corrupt)
	}
	frames := len(pcm) / c.ch
	got := len(out.pcm) / max(out.ch, 1)
	slack := c.rate / 2
	if got < frames-c.rate/20 || got > frames+slack {
		t.Errorf("decoded %d samples per channel from %d", got, frames)
	}
	checkTone(t, out, c.rate, c.ch, 0.9)
	at := onset(out)
	skew := at - toneStart
	if !asyncBackend(eb.Name) && !asyncBackend(db.Name) && (skew < -30*time.Millisecond || skew > 30*time.Millisecond) {
		t.Errorf("tone starts at %v on the PTS timeline, want %v", at, toneStart)
	}
	t.Logf("%d packets, %.1f kbit/s, PTS skew %v", len(e.packets), float64(e.bytes)*8/audioTestSeconds/1000, skew)
	if c.lossless {
		if got < frames {
			t.Fatalf("lossless stream lost samples: %d of %d", got, frames)
		}
		for i := range pcm {
			if out.pcm[i] != pcm[i] {
				t.Fatalf("lossless output differs at sample %d", i)
			}
		}
	}
	report(audioResult{codec: c.String(), pair: eb.Name + "+" + db.Name,
		enc: audioTestSeconds / e.took.Seconds(), dec: audioTestSeconds / took.Seconds(),
		kbps: float64(e.bytes) * 8 / audioTestSeconds / 1000, note: fmt.Sprintf("skew %v", skew.Round(100*time.Microsecond))})

	t.Run("corrupt", func(t *testing.T) { corruptSurvival(t, dl, dcfg, e.packets, c) })
	t.Run("lost", func(t *testing.T) { lostPacket(t, dl, dcfg, e.packets, c, out) })
	t.Run("allocs", func(t *testing.T) { audioAllocs(t, el, dl, cfg, dcfg, e.packets, pcm) })
}

func corruptSurvival(t *testing.T, l *Library, cfg AudioConfig, packets []Packet, c audioCase) {
	rng := rand.New(rand.NewSource(1))
	first := len(packets) / 3
	edit := func(i int, p Packet) Packet {
		if i < first || i >= first+3 {
			return p
		}
		q := clonePacket(p)
		switch i - first {
		case 0: // random bytes
			rng.Read(q.Data)
		case 1: // damaged in the middle
			for k := len(q.Data) / 2; k < len(q.Data); k++ {
				q.Data[k] ^= byte(rng.Intn(256))
			}
		case 2: // truncated
			q.Data = q.Data[:len(q.Data)/2+1]
		}
		return q
	}
	out, _, _ := decodeAudio(t, l, cfg, packets, edit)
	if out.ch == 0 {
		t.Fatal("no output")
	}
	// The stream must be good again after the damage.
	tail := out.pcm[max(0, len(out.pcm)-out.ch*c.rate):]
	tail = tail[:max(0, len(tail)-out.ch*c.rate/5)]
	for ch := 0; ch < c.ch; ch++ {
		if c.ch == 6 && ch == 3 {
			continue
		}
		share, _ := toneScore(tail, out.ch, ch, c.rate)
		if share < 0.8 {
			t.Errorf("channel %d after damage: %.3f of energy at its tone (%d samples, %d channels, %d frames)", ch, share, len(out.pcm), out.ch, out.frames)
		}
	}
}

func lostPacket(t *testing.T, l *Library, cfg AudioConfig, packets []Packet, c audioCase, clean *decoded) {
	at := len(packets) / 2
	edit := func(i int, p Packet) Packet {
		if i == at {
			return Packet{PTS: p.PTS}
		}
		return p
	}
	out, corrupt, _ := decodeAudio(t, l, cfg, packets, edit)
	if corrupt != 0 {
		t.Errorf("%d packets reported as corrupt", corrupt)
	}
	diff := len(clean.pcm) - len(out.pcm)
	if diff < 0 {
		diff = -diff
	}
	// The gap is filled, so the stream keeps its length within a frame or two.
	if limit := 2 * maxInt(clean.sizes) * c.ch; diff > limit {
		t.Errorf("output length changed by %d samples (limit %d)", diff, limit)
	}
}

func maxInt(v []int) int {
	m := 0
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func audioAllocs(t *testing.T, el, dl *Library, cfg, dcfg AudioConfig, packets []Packet, pcm []int16) {
	enc, err := el.NewAudioEncoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	defer enc.Close()
	dec, err := dl.NewAudioDecoder(dcfg)
	if err != nil {
		t.Skip(err)
	}
	defer dec.Close()
	n := cfg.SampleRate / 50 * cfg.Channels
	frame := &AudioFrame{SampleRate: cfg.SampleRate, Channels: cfg.Channels}
	ePos := 0
	ptsE := time.Duration(0)
	releaseP := func(p *EncodedPacket) { p.Release() }
	encodeOnce := func() {
		if ePos+n > len(pcm) {
			ePos = 0
		}
		frame.Samples, frame.PTS = pcm[ePos:ePos+n], ptsE
		ePos += n
		ptsE += 20 * time.Millisecond
		if err := enc.Send(frame); err != nil {
			t.Fatal(err)
		}
		drainNow[*AudioFrame, *EncodedPacket](t, enc, releaseP)
	}
	dPos := 0
	releaseF := func(f *AudioFrame) { f.Release() }
	decodeOnce := func() {
		p := packets[dPos%len(packets)]
		dPos++
		if err := dec.Send(p); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
		drainNow[Packet, *AudioFrame](t, dec, releaseF)
	}
	for i := 0; i < 300; i++ {
		encodeOnce()
		decodeOnce()
	}
	if a := testing.AllocsPerRun(300, encodeOnce); a > 0 {
		t.Errorf("encoder: %.2f allocations per frame", a)
	}
	if a := testing.AllocsPerRun(300, decodeOnce); a > 0 {
		t.Errorf("decoder: %.2f allocations per packet", a)
	}
}

// decoderAllocs measures a decoder alone, cycling through packets.
func decoderAllocs(t *testing.T, l *Library, cfg AudioConfig, packets []Packet) {
	dec, err := l.NewAudioDecoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	defer dec.Close()
	pos := 0
	release := func(f *AudioFrame) { f.Release() }
	once := func() {
		if err := dec.Send(packets[pos%len(packets)]); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
		pos++
		drainNow[Packet, *AudioFrame](t, dec, release)
	}
	for i := 0; i < 300; i++ {
		once()
	}
	if a := testing.AllocsPerRun(300, once); a > 0 {
		t.Errorf("decoder: %.2f allocations per packet", a)
	}
}

// underWine reports whether this is a Windows test binary running on Wine.
func underWine() bool {
	_, err := os.Stat(`C:\windows\system32\winecfg.exe`)
	return err == nil
}

func TestAudioReport(t *testing.T) {
	if len(audioResults) == 0 {
		t.Skip("no results")
	}
	sort.SliceStable(audioResults, func(i, j int) bool { return audioResults[i].codec < audioResults[j].codec })
	for _, r := range audioResults {
		t.Logf("%-16s %-44s encode %7.0fx  decode %7.0fx  %6.1f kbit/s  %s", r.codec, r.pair, r.enc, r.dec, r.kbps, r.note)
	}
}

func TestAudioBackendList(t *testing.T) {
	l := New(Options{})
	for _, c := range []Codec{AAC, MP3, FLAC, Vorbis, ALAC, AC3, PCMU, PCMA, Opus} {
		for _, d := range []Direction{Decode, Encode} {
			var names []string
			for _, b := range l.Backends(c, d) {
				s := b.Name
				if b.Hardware {
					s += "(hw)"
				}
				names = append(names, s)
			}
			t.Logf("%-7s %-7s %s", c, d, strings.Join(names, ", "))
		}
	}
}

// --- G.711 ---

func TestG711Tables(t *testing.T) {
	tb := loadG711()
	for i := 0; i < 256; i++ {
		if got := tb.ulawEnc[uint16(tb.ulawDec[i])>>2]; tb.ulawDec[got] != tb.ulawDec[i] {
			t.Errorf("mu-law %#x does not survive a round trip", i)
		}
		if got := tb.alawEnc[uint16(tb.alawDec[i])>>3]; got != uint8(i) {
			t.Errorf("A-law %#x comes back as %#x", i, got)
		}
	}
	// Reference points from ITU-T G.711.
	if tb.ulawEnc[0] != 0xFF || tb.alawEnc[0] != 0xD5 {
		t.Errorf("silence encodes to %#x and %#x", tb.ulawEnc[0], tb.alawEnc[0])
	}
}

// --- fixtures: streams from other encoders ---

type fixtureSpec struct {
	name     string
	codec    Codec
	rate, ch int
	args     []string
	split    func([]byte) ([]Packet, []byte)
}

func splitADTS(b []byte) ([]Packet, []byte) {
	var out []Packet
	for n := 0; len(b) > 0; n++ {
		h, ok := parseADTS(b)
		if !ok {
			break
		}
		out = append(out, Packet{Data: b[:h.frameLen], PTS: audioDuration(n*1024, 48000), Keyframe: true})
		b = b[h.frameLen:]
	}
	return out, nil
}

func splitMP3(b []byte) ([]Packet, []byte) {
	var out []Packet
	pts := time.Duration(0)
	for len(b) >= 4 {
		h, ok := parseMP3(b)
		if !ok || h.size > len(b) {
			break
		}
		out = append(out, Packet{Data: b[:h.size], PTS: pts, Keyframe: true})
		pts += audioDuration(h.samples, h.rate)
		b = b[h.size:]
	}
	return out, nil
}

func splitAC3(b []byte) ([]Packet, []byte) {
	var out []Packet
	pts := time.Duration(0)
	for {
		size, rate, _, ok := parseAC3(b)
		if !ok || size > len(b) {
			break
		}
		out = append(out, Packet{Data: b[:size], PTS: pts, Keyframe: true})
		pts += audioDuration(ac3FrameSamples, rate)
		b = b[size:]
	}
	return out, nil
}

// splitFLAC cuts a .flac file at frame headers verified by their CRC-8.
func splitFLAC(b []byte) ([]Packet, []byte) {
	if len(b) < 8 || string(b[:4]) != "fLaC" {
		return nil, nil
	}
	pos := 4
	for {
		last := b[pos]&0x80 != 0
		pos += 4 + int(b[pos+1])<<16 + int(b[pos+2])<<8 + int(b[pos+3])
		if last {
			break
		}
	}
	config := b[:pos]
	var starts []int
	for i := pos; i+6 < len(b); i++ {
		if b[i] == 0xFF && b[i+1]&0xFE == 0xF8 && flacHeaderValid(b[i:]) {
			starts = append(starts, i)
		}
	}
	var out []Packet
	for i, s := range starts {
		e := len(b)
		if i+1 < len(starts) {
			e = starts[i+1]
		}
		out = append(out, Packet{Data: b[s:e], PTS: time.Duration(i) * 80 * time.Millisecond, Keyframe: true})
	}
	return out, config
}

func flacHeaderValid(b []byte) bool {
	pos := 4
	if pos >= len(b) {
		return false
	}
	for v := b[4]; v&0x80 != 0; v <<= 1 { // UTF-8 style frame number
		pos++
	}
	if b[4]&0xC0 == 0x80 {
		return false
	}
	pos++
	switch b[2] >> 4 {
	case 6:
		pos++
	case 7:
		pos += 2
	}
	switch b[2] & 15 {
	case 12:
		pos++
	case 13, 14:
		pos += 2
	}
	if pos >= len(b) {
		return false
	}
	crc := byte(0)
	for _, v := range b[:pos] {
		crc ^= v
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 7
			} else {
				crc <<= 1
			}
		}
	}
	return crc == b[pos]
}

// splitOgg returns the packets of the first logical stream of an Ogg file.
func splitOgg(b []byte) ([]Packet, []byte) {
	var packets [][]byte
	var cur []byte
	for len(b) >= 27 && string(b[:4]) == "OggS" {
		segs := int(b[26])
		table := b[27 : 27+segs]
		body := b[27+segs:]
		for _, l := range table {
			cur = append(cur, body[:l]...)
			body = body[l:]
			if l < 255 {
				packets = append(packets, cur)
				cur = nil
			}
		}
		b = body
	}
	if len(packets) < 3 {
		return nil, nil
	}
	config := xiphLace(packets[0], packets[1], packets[2])
	var out []Packet
	for i, p := range packets[3:] {
		out = append(out, Packet{Data: p, PTS: time.Duration(i) * 20 * time.Millisecond, Keyframe: true})
	}
	return out, config
}

var fixtureSpecs = []fixtureSpec{
	{"aac_48000_2.adts", AAC, 48000, 2, []string{"-c:a", "aac", "-b:a", "128k", "-f", "adts"}, splitADTS},
	{"aac_48000_6.adts", AAC, 48000, 6, []string{"-c:a", "aac", "-b:a", "384k", "-f", "adts"}, splitADTS},
	{"aac_44100_1.adts", AAC, 44100, 1, []string{"-c:a", "aac", "-b:a", "64k", "-f", "adts"}, splitADTS},
	{"mp3_44100_2.mp3", MP3, 44100, 2, []string{"-c:a", "libmp3lame", "-b:a", "128k", "-write_xing", "0", "-id3v2_version", "0", "-f", "mp3"}, splitMP3},
	{"mp3_22050_1.mp3", MP3, 22050, 1, []string{"-c:a", "libmp3lame", "-b:a", "48k", "-write_xing", "0", "-id3v2_version", "0", "-f", "mp3"}, splitMP3},
	{"flac_48000_2.flac", FLAC, 48000, 2, []string{"-c:a", "flac", "-f", "flac"}, splitFLAC},
	{"vorbis_44100_2.ogg", Vorbis, 44100, 2, []string{"-c:a", "libvorbis", "-q:a", "4", "-f", "ogg"}, splitOgg},
	{"ac3_48000_2.ac3", AC3, 48000, 2, []string{"-c:a", "ac3", "-b:a", "192k", "-f", "ac3"}, splitAC3},
	{"ac3_48000_6.ac3", AC3, 48000, 6, []string{"-c:a", "ac3", "-b:a", "448k", "-f", "ac3"}, splitAC3},
	{"ac3_44100_1.ac3", AC3, 44100, 1, []string{"-c:a", "ac3", "-b:a", "96k", "-f", "ac3"}, splitAC3},
}

func haveFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func pcmBytes(pcm []int16) []byte {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func ffmpegRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

func ffmpegEncode(t *testing.T, s fixtureSpec) []byte {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.raw"), filepath.Join(dir, s.name)
	if err := os.WriteFile(in, pcmBytes(tonePCM(s.rate, s.ch, audioTestSeconds)), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", "s16le", "-ar", fmt.Sprint(s.rate), "-ac", fmt.Sprint(s.ch), "-i", in}
	ffmpegRun(t, append(append(args, s.args...), out)...)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAudioForeignStreams decodes streams from other encoders with every decoder.
func TestAudioForeignStreams(t *testing.T) {
	l := New(Options{})
	for _, s := range fixtureSpecs {
		for _, b := range l.Backends(s.codec, Decode) {
			t.Run(s.name+"/"+b.Name, func(t *testing.T) {
				data := audioFixtures[s.name]
				if data == nil {
					if !haveFFmpeg() {
						t.Skip("no fixture and no ffmpeg")
					}
					data = ffmpegEncode(t, s)
				}
				packets, extra := s.split(data)
				if len(packets) == 0 {
					t.Fatal("fixture has no packets")
				}
				if s.codec == MP3 && s.rate < 32000 && underWine() {
					t.Skip("the MP3 transform of Wine stops after the first MPEG-2 frame")
				}
				cfg := AudioConfig{Codec: s.codec, SampleRate: s.rate, Channels: s.ch, Extra: extra}
				dl := New(Options{Backend: b.Name})
				out, corrupt, took := decodeAudio(t, dl, cfg, packets, nil)
				if corrupt != 0 {
					t.Errorf("%d packets reported as corrupt", corrupt)
				}
				if s.codec == AC3 && asyncBackend(b.Name) {
					// Device decoders choose their own channel layout and level.
					t.Logf("device output: %d Hz, %d channels", out.rate, out.ch)
					from := out.sampleAt(toneStart + 500*time.Millisecond)
					seg := out.pcm[from:min(len(out.pcm), from+out.ch*out.rate)]
					if share, _ := toneScore(seg, out.ch, 0, s.rate); out.ch == s.ch && share < 0.85 {
						t.Errorf("channel 0: %.3f of energy at its tone", share)
					}
					return
				}
				checkTone(t, out, s.rate, s.ch, 0.85)
				got := len(out.pcm) / out.ch
				want := s.rate * audioTestSeconds
				if got < want-want/10 || got > want+s.rate/2 {
					t.Errorf("decoded %d samples per channel, want about %d", got, want)
				}
				t.Logf("%d packets, decode %.0fx real time", len(packets), audioTestSeconds/took.Seconds())
				t.Run("lost", func(t *testing.T) {
					lostPacket(t, dl, cfg, packets, audioCase{codec: s.codec, rate: s.rate, ch: s.ch}, out)
				})
				t.Run("allocs", func(t *testing.T) { decoderAllocs(t, dl, cfg, packets) })
			})
		}
	}
	// AAC with the configuration given out of band instead of ADTS headers.
	for _, b := range l.Backends(AAC, Decode) {
		t.Run("aac_raw/"+b.Name, func(t *testing.T) {
			s := fixtureSpecs[0]
			data := audioFixtures[s.name]
			if data == nil {
				if !haveFFmpeg() {
					t.Skip("no fixture and no ffmpeg")
				}
				data = ffmpegEncode(t, s)
			}
			packets, _ := splitADTS(data)
			h, _ := parseADTS(packets[0].Data)
			raw := make([]Packet, len(packets))
			for i, p := range packets {
				hh, _ := parseADTS(p.Data)
				raw[i] = Packet{Data: p.Data[hh.headerLen:], PTS: p.PTS, Keyframe: true}
			}
			cfg := AudioConfig{Codec: AAC, SampleRate: s.rate, Channels: s.ch, Extra: h.config()}
			out, corrupt, _ := decodeAudio(t, New(Options{Backend: b.Name}), cfg, raw, nil)
			if corrupt != 0 {
				t.Errorf("%d packets reported as corrupt", corrupt)
			}
			checkTone(t, out, s.rate, s.ch, 0.85)
		})
	}
}

// --- output of our encoders, read by ffmpeg ---

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func oggPage(seq uint32, flags byte, granule int64, packet []byte) []byte {
	var segs []byte
	n := len(packet)
	for ; n >= 255; n -= 255 {
		segs = append(segs, 255)
	}
	segs = append(segs, byte(n))
	h := make([]byte, 27, 27+len(segs)+len(packet))
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	binary.LittleEndian.PutUint32(h[14:], 0x47414131)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = byte(len(segs))
	h = append(append(h, segs...), packet...)
	binary.LittleEndian.PutUint32(h[22:], oggCRC(h))
	return h
}

func muxForFFmpeg(t *testing.T, c audioCase, e *encoded) (name string, data []byte, format []string) {
	var out []byte
	switch c.codec {
	case AAC:
		_, _, _, ok := aacConfigInfo(e.config)
		if !ok {
			t.Fatalf("bad AudioSpecificConfig %x", e.config)
		}
		idx := -1
		for i, r := range aacRates {
			if r == c.rate {
				idx = i
			}
		}
		for _, p := range e.packets {
			n := len(p.Data) + 7
			out = append(out, 0xFF, 0xF1, byte(1<<6|idx<<2|aacChannelConfig(c.ch)>>2),
				byte(aacChannelConfig(c.ch)<<6|n>>11), byte(n>>3), byte(n<<5|0x1F), 0xFC)
			out = append(out, p.Data...)
		}
		return "t.aac", out, []string{"-f", "aac"}
	case MP3:
		for _, p := range e.packets {
			out = append(out, p.Data...)
		}
		return "t.mp3", out, []string{"-f", "mp3"}
	case FLAC:
		out = append(out, e.config...)
		for _, p := range e.packets {
			out = append(out, p.Data...)
		}
		return "t.flac", out, []string{"-f", "flac"}
	case Vorbis:
		parts := xiphUnlace(e.config)
		if len(parts) != 3 {
			t.Fatal("bad Vorbis config")
		}
		out = append(out, oggPage(0, 2, 0, parts[0])...)
		out = append(out, oggPage(1, 0, 0, parts[1])...)
		out = append(out, oggPage(2, 0, 0, parts[2])...)
		for i, p := range e.packets {
			flags, g := byte(0), int64(p.PTS.Seconds()*float64(c.rate)+0.5)
			if i+1 < len(e.packets) {
				g = int64(e.packets[i+1].PTS.Seconds()*float64(c.rate) + 0.5)
			} else {
				flags = 4
				g += 2048
			}
			out = append(out, oggPage(uint32(3+i), flags, g, p.Data)...)
		}
		return "t.ogg", out, []string{"-f", "ogg"}
	case PCMU, PCMA:
		for _, p := range e.packets {
			out = append(out, p.Data...)
		}
		f := "mulaw"
		if c.codec == PCMA {
			f = "alaw"
		}
		return "t.raw", out, []string{"-f", f, "-ar", fmt.Sprint(c.rate), "-ac", fmt.Sprint(c.ch)}
	}
	return "", nil, nil
}

// TestAudioFFmpegDecodesOurs checks that ffmpeg's own decoders read our streams.
func TestAudioFFmpegDecodesOurs(t *testing.T) {
	if !haveFFmpeg() {
		t.Skip("ffmpeg is not installed")
	}
	base := New(Options{})
	for _, c := range audioCases {
		for _, b := range base.Backends(c.codec, Encode) {
			t.Run(c.String()+"/"+b.Name, func(t *testing.T) {
				pcm := tonePCM(c.rate, c.ch, audioTestSeconds)
				e := encodeAudio(t, New(Options{Backend: b.Name}), c.config(), pcm)
				name, data, format := muxForFFmpeg(t, c, e)
				if name == "" {
					t.Skip("no muxer for this codec")
				}
				dir := t.TempDir()
				in, out := filepath.Join(dir, name), filepath.Join(dir, "out.raw")
				if err := os.WriteFile(in, data, 0o644); err != nil {
					t.Fatal(err)
				}
				args := append(format, "-i", in, "-f", "s16le", "-ac", fmt.Sprint(c.ch), "-ar", fmt.Sprint(c.rate), out)
				ffmpegRun(t, args...)
				raw, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				got := make([]int16, len(raw)/2)
				for i := range got {
					got[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
				}
				d := &decoded{pcm: got, rate: c.rate, ch: c.ch}
				checkTone(t, d, c.rate, c.ch, 0.9)
				if c.lossless {
					if len(got) < len(pcm) {
						t.Fatalf("ffmpeg decoded %d of %d samples", len(got), len(pcm))
					}
					for i := range pcm {
						if got[i] != pcm[i] {
							t.Fatalf("lossless output differs at sample %d", i)
						}
					}
				}
				if c.codec == PCMU || c.codec == PCMA {
					own, _, _ := decodeAudio(t, New(Options{Backend: "g711"}), c.config(), e.packets, nil)
					for i := range own.pcm {
						if own.pcm[i] != got[i] {
							t.Fatalf("ffmpeg and g711 differ at sample %d: %d and %d", i, got[i], own.pcm[i])
						}
					}
				}
			})
		}
	}
}

// TestVorbisHeadersInBand starts a decoder from header packets in the stream.
func TestVorbisHeadersInBand(t *testing.T) {
	l := New(Options{})
	if len(l.Backends(Vorbis, Encode)) == 0 || len(l.Backends(Vorbis, Decode)) == 0 {
		t.Skip("no Vorbis encoder and decoder")
	}
	c := audioCase{codec: Vorbis, rate: 48000, ch: 2}
	e := encodeAudio(t, l, c.config(), tonePCM(c.rate, c.ch, audioTestSeconds))
	var packets []Packet
	for _, h := range xiphUnlace(e.config) {
		packets = append(packets, Packet{Data: h})
	}
	out, corrupt, _ := decodeAudio(t, l, c.config(), append(packets, e.packets...), nil)
	if corrupt != 0 {
		t.Errorf("%d packets reported as corrupt", corrupt)
	}
	checkTone(t, out, c.rate, c.ch, 0.9)
}

// TestAudioGarbage feeds damaged input to every decoder. Nothing may crash, and
// only ErrCorrupt may be reported.
func TestAudioGarbage(t *testing.T) {
	base := New(Options{})
	for _, c := range audioCases {
		encs := base.Backends(c.codec, Encode)
		if len(encs) == 0 {
			continue
		}
		for _, db := range base.Backends(c.codec, Decode) {
			t.Run(c.String()+"/"+db.Name, func(t *testing.T) {
				e := encodeAudio(t, New(Options{Backend: encs[0].Name}), c.config(), tonePCM(c.rate, c.ch, 1))
				cfg := c.config()
				cfg.Bitrate, cfg.Extra = 0, e.config
				rng := rand.New(rand.NewSource(2))
				edit := func(i int, p Packet) Packet {
					q := clonePacket(p)
					q.PTS = time.Duration(i) * 20 * time.Millisecond
					switch rng.Intn(4) {
					case 0:
						rng.Read(q.Data)
					case 1:
						for k := 0; k < 8; k++ {
							q.Data[rng.Intn(len(q.Data))] ^= byte(1 << rng.Intn(8))
						}
					case 2:
						q.Data = q.Data[:rng.Intn(len(q.Data))+1]
					}
					return q
				}
				var packets []Packet
				for i := 0; i < 4 && len(packets) < 400; i++ {
					packets = append(packets, e.packets...)
				}
				decodeAudio(t, New(Options{Backend: db.Name}), cfg, packets, edit)
			})
		}
	}
}
