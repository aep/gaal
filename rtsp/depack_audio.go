package rtsp

import (
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// frameDepacketizer passes each payload through as one frame: PCMU, PCMA, Opus.
type frameDepacketizer struct {
	t *track
	b unitBuilder
}

func (d *frameDepacketizer) reset() {}
func (d *frameDepacketizer) lost()  {}

func (d *frameDepacketizer) push(h rtpHeader, p []byte, arrival time.Time) {
	if len(p) == 0 {
		return
	}
	d.b.begin(0)
	if !d.b.write(p) {
		d.b.discard()
		d.t.dropUnit()
		return
	}
	d.t.emit(d.b.take(), h.ts, true, arrival)
}

// aacParams configures RFC 3640 depacketization.
type aacParams struct {
	sizeLength, indexLength, indexDeltaLength int
	frameTicks                                uint32
}

// aacDepacketizer handles mpeg4-generic AAC, including fragmented frames.
type aacDepacketizer struct {
	t       *track
	cfg     aacParams
	b       unitBuilder
	inFrag  bool
	fragLen int
	fragTS  uint32
}

func (d *aacDepacketizer) reset() { d.inFrag = false; d.b.discard() }
func (d *aacDepacketizer) lost()  { d.dropFragment() }

func (d *aacDepacketizer) dropFragment() {
	if d.inFrag {
		d.inFrag = false
		d.b.discard()
		d.t.dropUnit()
	}
}

func (d *aacDepacketizer) push(h rtpHeader, p []byte, arrival time.Time) {
	if len(p) < 2 {
		d.dropFragment()
		return
	}
	bits := int(p[0])<<8 | int(p[1])
	hdrBytes := (bits + 7) / 8
	if 2+hdrBytes > len(p) || d.cfg.sizeLength == 0 {
		d.dropFragment()
		return
	}
	r := spsReader{b: p[2 : 2+hdrBytes]}
	data := p[2+hdrBytes:]
	ts := h.ts
	for i := 0; r.pos < bits; i++ {
		size := int(r.u(d.cfg.sizeLength))
		if i == 0 {
			r.skip(d.cfg.indexLength)
		} else {
			r.skip(d.cfg.indexDeltaLength)
		}
		if r.bad || r.pos > bits {
			d.dropFragment()
			return
		}
		if d.inFrag {
			if i != 0 || size != d.fragLen || h.ts != d.fragTS {
				d.dropFragment()
			} else {
				d.continueFragment(data, arrival)
				return
			}
		}
		if size > len(data) {
			if i == 0 {
				d.startFragment(size, h.ts, data, arrival)
			} else {
				d.t.dropUnit()
			}
			return
		}
		if size > 0 {
			d.b.begin(0)
			if d.b.write(data[:size]) {
				d.t.emit(d.b.take(), ts, true, arrival)
			} else {
				d.b.discard()
				d.t.dropUnit()
			}
		}
		data = data[size:]
		ts += d.cfg.frameTicks
	}
}

func (d *aacDepacketizer) startFragment(size int, ts uint32, data []byte, arrival time.Time) {
	if size > d.t.c.cfg.MaxUnitSize {
		d.t.dropUnit()
		return
	}
	d.b.begin(0)
	d.inFrag, d.fragLen, d.fragTS = true, size, ts
	d.continueFragment(data, arrival)
}

func (d *aacDepacketizer) continueFragment(data []byte, arrival time.Time) {
	if d.b.size()+len(data) > d.fragLen || !d.b.write(data) {
		d.dropFragment()
		return
	}
	if d.b.size() == d.fragLen {
		d.inFrag = false
		d.t.emit(d.b.take(), d.fragTS, true, arrival)
	}
}

// latmDepacketizer handles MP4A-LATM with out-of-band configuration and one
// subframe per audio mux element.
type latmDepacketizer struct {
	t      *track
	b      unitBuilder
	active bool
	bad    bool
	ts     uint32
}

func (d *latmDepacketizer) reset() { d.active, d.bad = false, false; d.b.discard() }
func (d *latmDepacketizer) lost()  { d.bad = d.active }

func (d *latmDepacketizer) push(h rtpHeader, p []byte, arrival time.Time) {
	if !d.active || h.ts != d.ts {
		if d.active {
			d.t.dropUnit()
		}
		d.b.begin(0)
		d.active, d.bad, d.ts = true, false, h.ts
	}
	if !d.b.write(p) {
		d.bad = true
	}
	if !h.marker {
		return
	}
	d.active = false
	if d.bad {
		d.b.discard()
		d.t.dropUnit()
		return
	}
	b := d.b.bytes()
	size, i := 0, 0
	for i < len(b) {
		v := int(b[i])
		i++
		size += v
		if v != 255 {
			break
		}
	}
	if size == 0 || i+size > len(b) {
		d.b.discard()
		d.t.dropUnit()
		return
	}
	d.b.off += i
	d.b.end = d.b.off + size
	d.t.emit(d.b.take(), h.ts, true, arrival)
}

func fmtpInt(f map[string]string, k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(f[k]))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// parseASC reads the rate, channels and frame length of an AudioSpecificConfig.
func parseASC(b []byte) (rate, channels, frame int, bits int, ok bool) {
	r := spsReader{b: b}
	aot := r.u(5)
	if aot == 31 {
		aot = 32 + r.u(6)
	}
	idx := r.u(4)
	if idx == 15 {
		rate = int(r.u(24))
	} else if int(idx) < len(aacRates) {
		rate = aacRates[idx]
	}
	channels = int(r.u(4))
	if channels == 7 {
		channels = 8
	}
	frame = 1024
	if aot == 1 || aot == 2 || aot == 3 || aot == 4 {
		if r.bit() == 1 {
			frame = 960
		}
		if r.bit() == 1 {
			r.skip(14)
		}
		r.skip(1)
	}
	return rate, channels, frame, r.pos, !r.bad && rate > 0
}

func aacConfigRate(b []byte) int {
	r, _, _, _, ok := parseASC(b)
	if !ok {
		return 0
	}
	return r
}

// latmConfig extracts the AudioSpecificConfig from a StreamMuxConfig.
func latmConfig(b []byte) ([]byte, bool) {
	r := spsReader{b: b}
	if r.bit() != 0 { // audioMuxVersion
		return nil, false
	}
	r.skip(1) // allStreamsSameTimeFraming
	sub, prog, layer := r.u(6), r.u(4), r.u(3)
	if r.bad || sub != 0 || prog != 0 || layer != 0 {
		return nil, false
	}
	rest := spsReader{b: b, pos: r.pos}
	var probe []byte
	for rest.pos+8 <= len(b)*8 {
		probe = append(probe, byte(rest.u(8)))
	}
	_, _, _, bits, ok := parseASC(probe)
	if !ok {
		return nil, false
	}
	return probe[:(bits+7)/8], true
}

func decodeHex(s string) []byte {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return b
}
