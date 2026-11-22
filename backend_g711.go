package gaal

import (
	"fmt"
	"sync"
)

// G.711 mu-law and A-law, in pure Go. The patents on G.711 expired long ago and
// the codec is a pair of lookup tables. Every sample is one byte, so packets can
// hold any number of them, and encoder output keeps the framing of its input.

type g711Tables struct {
	ulawEnc [1 << 14]uint8 // indexed by the top 14 bits of a sample
	alawEnc [1 << 13]uint8 // indexed by the top 13 bits
	ulawDec [256]int16
	alawDec [256]int16
}

func ulawEncode(s int) uint8 {
	const bias, clip = 0x84, 32635
	sign := 0
	if s < 0 {
		s, sign = -s, 0x80
	}
	s = min(s, clip) + bias
	exp := 7
	for mask := 0x4000; exp > 0 && s&mask == 0; mask >>= 1 {
		exp--
	}
	return ^uint8(sign | exp<<4 | s>>(exp+3)&15)
}

func ulawDecode(v uint8) int16 {
	v = ^v
	t := (int(v&15)<<3 + 0x84) << (v >> 4 & 7)
	if v&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

func alawEncode(s int) uint8 {
	sign := 0x80
	if s < 0 {
		s, sign = -s-1, 0
	}
	s = min(s, 32767)
	exp := 7
	for mask := 0x4000; exp > 0 && s&mask == 0; mask >>= 1 {
		exp--
	}
	shift := exp + 3
	if exp == 0 {
		shift = 4
	}
	return uint8(sign|exp<<4|s>>shift&15) ^ 0x55
}

func alawDecode(v uint8) int16 {
	v ^= 0x55
	exp := int(v >> 4 & 7)
	t := int(v&15)<<4 + 8
	if exp > 0 {
		t = (t + 0x100) << (exp - 1)
	}
	if v&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

var loadG711 = sync.OnceValue(func() *g711Tables {
	t := new(g711Tables)
	for i := range t.ulawEnc {
		t.ulawEnc[i] = ulawEncode(int(int16(i<<2)) + 2)
	}
	for i := range t.alawEnc {
		t.alawEnc[i] = alawEncode(int(int16(i<<3)) + 4)
	}
	for i := 0; i < 256; i++ {
		t.ulawDec[i], t.alawDec[i] = ulawDecode(uint8(i)), alawDecode(uint8(i))
	}
	return t
})

func g711Backends() []backend {
	var out []backend
	for _, c := range []Codec{PCMU, PCMA} {
		dec := BackendInfo{"g711", c, Decode, false}
		enc := BackendInfo{"g711", c, Encode, false}
		out = append(out,
			backend{BackendInfo: dec, adec: func(_ *Library, cfg AudioConfig) (AudioDecoder, error) {
				return newG711Decoder(cfg, dec), nil
			}},
			backend{BackendInfo: enc, aenc: func(_ *Library, cfg AudioConfig) (AudioEncoder, error) {
				return newG711Encoder(cfg, enc), nil
			}})
	}
	return out
}

type g711Decoder struct {
	audioDecBase
	table *[256]int16
}

func newG711Decoder(cfg AudioConfig, info BackendInfo) *g711Decoder {
	d := &g711Decoder{}
	d.initDec(cfg, info, cfg.SampleRate/50)
	d.table = &loadG711().ulawDec
	if cfg.Codec == PCMA {
		d.table = &loadG711().alawDec
	}
	return d
}

func (d *g711Decoder) Send(p Packet) error {
	if err := d.checkSend(); err != nil {
		return err
	}
	if len(p.Data) == 0 {
		d.silence(p.PTS)
		return nil
	}
	if len(p.Data)%d.channels != 0 {
		return fmt.Errorf("%w: g711: %d bytes for %d channels", ErrCorrupt, len(p.Data), d.channels)
	}
	f := d.frames.get(len(p.Data))
	t := d.table
	s := f.Samples[:len(p.Data)]
	for i, v := range p.Data {
		s[i] = t[v]
	}
	d.push(f, len(p.Data)/d.channels, p.PTS)
	return nil
}

func (d *g711Decoder) Close() error {
	if !d.closed {
		d.closed = true
		d.dropOutput()
	}
	return nil
}

type g711Encoder struct {
	audioEncBase
	ulaw *[1 << 14]uint8
	alaw *[1 << 13]uint8
}

func newG711Encoder(cfg AudioConfig, info BackendInfo) *g711Encoder {
	e := &g711Encoder{}
	e.cfg, e.info = cfg, info
	t := loadG711()
	if cfg.Codec == PCMA {
		e.alaw = &t.alawEnc
	} else {
		e.ulaw = &t.ulawEnc
	}
	return e
}

func (e *g711Encoder) Send(f *AudioFrame) error {
	if err := e.checkSend(f); err != nil {
		return err
	}
	if len(f.Samples) == 0 {
		return nil
	}
	p := e.packets.get(len(f.Samples))
	d := p.Data[:len(f.Samples)]
	if e.ulaw != nil {
		t := e.ulaw
		for i, s := range f.Samples {
			d[i] = t[uint16(s)>>2]
		}
	} else {
		t := e.alaw
		for i, s := range f.Samples {
			d[i] = t[uint16(s)>>3]
		}
	}
	p.PTS, p.Keyframe = f.PTS, true
	e.out.push(p)
	return nil
}

func (e *g711Encoder) Flush() error { e.flushed = true; return nil }

func (e *g711Encoder) Close() error {
	if !e.closed {
		e.closed = true
		e.dropOutput()
	}
	return nil
}
