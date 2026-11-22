package rtsp

import (
	"bytes"
	"time"
)

var startCode = []byte{0, 0, 0, 1}

// Indexes of cached parameter sets, in the order they are inserted.
const (
	paramVPS = iota
	paramSPS
	paramPPS
	paramKinds
)

// depacketizer rebuilds units from in-order RTP payloads.
type depacketizer interface {
	push(h rtpHeader, payload []byte, arrival time.Time)
	// lost says packets were missing before the next push.
	lost()
	// reset drops partial state at the start of a connection.
	reset()
}

// nalDepacketizer handles H.264 (RFC 6184) and H.265 (RFC 7798).
type nalDepacketizer struct {
	t    *track
	h265 bool
	b    unitBuilder

	building    bool
	ts          uint32
	arrival     time.Time
	damaged     bool
	gap         bool
	inFU        bool
	fuStart     int
	hasVCL      bool
	key         bool
	seen        [paramKinds]bool
	pending     [paramKinds][]byte
	cache       [paramKinds][]byte
	lastSPS     []byte
	emitted     bool
	lastEmitTS  uint32
	ignoreMark  bool
	insertParts [3][]byte
	spsIn       []byte // last SPS seen, before any low-delay rewrite
	spsOut      []byte // its rewritten form; empty when it was left alone
}

func newNALDepacketizer(t *track, h265 bool) *nalDepacketizer {
	d := &nalDepacketizer{t: t, h265: h265, b: unitBuilder{t: t}}
	for _, nal := range t.info.ParameterSets {
		nal = d.lowDelay(nal)
		if k, ok := d.paramKind(nal); ok {
			d.cache[k] = append(append(d.cache[k], startCode...), nal...)
			if k == paramSPS && d.lastSPS == nil {
				d.noteSPS(nal)
			}
		}
	}
	return d
}

// lowDelay applies Config.LowDelay to an SPS. Other NAL units pass through. The
// result is cached, so a repeated SPS costs one comparison.
func (d *nalDepacketizer) lowDelay(nal []byte) []byte {
	if !d.t.c.cfg.LowDelay || d.h265 || len(nal) == 0 || nal[0]&0x1F != 7 {
		return nal
	}
	if !bytes.Equal(nal, d.spsIn) {
		d.spsIn = append(d.spsIn[:0], nal...)
		d.spsOut, _ = h264LowDelaySPS(nal)
	}
	if len(d.spsOut) > 0 {
		return d.spsOut
	}
	return nal
}

func (d *nalDepacketizer) nalType(nal []byte) int {
	if d.h265 {
		return int(nal[0] >> 1 & 0x3F)
	}
	return int(nal[0] & 0x1F)
}

func (d *nalDepacketizer) headerSize() int {
	if d.h265 {
		return 2
	}
	return 1
}

func (d *nalDepacketizer) paramKind(nal []byte) (int, bool) {
	if len(nal) < d.headerSize() {
		return 0, false
	}
	switch t := d.nalType(nal); {
	case d.h265 && t == 32:
		return paramVPS, true
	case d.h265 && t == 33, !d.h265 && t == 7:
		return paramSPS, true
	case d.h265 && t == 34, !d.h265 && t == 8:
		return paramPPS, true
	}
	return 0, false
}

func (d *nalDepacketizer) isVCL(t int) bool {
	if d.h265 {
		return t < 32
	}
	return t >= 1 && t <= 5
}

func (d *nalDepacketizer) isKey(t int) bool {
	if d.h265 {
		return t >= 16 && t <= 21
	}
	return t == 5
}

// isDroppable marks access unit delimiters, which would have to stay first, and
// filler data.
func (d *nalDepacketizer) isDroppable(t int) bool {
	if d.h265 {
		return t == 35 || t == 38
	}
	return t == 9 || t == 12
}

func (d *nalDepacketizer) reset() {
	d.b.discard()
	d.building, d.damaged, d.inFU, d.hasVCL, d.key = false, false, false, false, false
	d.gap, d.emitted = false, false
	d.seen = [paramKinds]bool{}
	for i := range d.pending {
		d.pending[i] = d.pending[i][:0]
	}
}

func (d *nalDepacketizer) lost() {
	d.gap = true
	d.inFU = false
	if d.building {
		d.damaged = true
	}
}

func (d *nalDepacketizer) headroom() int {
	return len(d.cache[0]) + len(d.cache[1]) + len(d.cache[2]) + 64
}

func (d *nalDepacketizer) startUnit(h rtpHeader, payload []byte) {
	d.b.begin(d.headroom())
	d.building = true
	d.ts = h.ts
	d.hasVCL, d.key, d.inFU = false, false, false
	d.damaged = false
	d.seen = [paramKinds]bool{}
	for i := range d.pending {
		d.pending[i] = d.pending[i][:0]
	}
	if d.gap {
		d.gap = false
		d.damaged = !d.beginsUnit(payload)
	}
}

// beginsUnit reports whether the payload starts a new picture, so that a loss
// just before it did not touch this access unit.
func (d *nalDepacketizer) beginsUnit(p []byte) bool {
	hdr := d.headerSize()
	if len(p) < hdr+1 {
		return false
	}
	t, body := d.nalType(p), p[hdr:]
	if d.h265 {
		switch t {
		case 48:
			if len(body) < 3+hdr {
				return false
			}
			return d.nalStartsUnit(body[2:])
		case 49:
			if len(body) < 2 || body[0]&0x80 == 0 {
				return false
			}
			ft := int(body[0] & 0x3F)
			return !d.isVCL(ft) || body[1]&0x80 != 0
		}
	} else {
		switch t {
		case 24:
			if len(body) < 3+hdr {
				return false
			}
			return d.nalStartsUnit(body[2:])
		case 28:
			if len(body) < 2 || body[0]&0x80 == 0 {
				return false
			}
			ft := int(body[0] & 0x1F)
			return !d.isVCL(ft) || body[1]&0x80 != 0
		}
	}
	return d.nalStartsUnit(p)
}

func (d *nalDepacketizer) nalStartsUnit(nal []byte) bool {
	hdr := d.headerSize()
	if len(nal) <= hdr {
		return false
	}
	if !d.isVCL(d.nalType(nal)) {
		return true
	}
	return nal[hdr]&0x80 != 0 // first_mb_in_slice == 0 or first_slice_segment_in_pic
}

func (d *nalDepacketizer) push(h rtpHeader, p []byte, arrival time.Time) {
	if d.building && h.ts != d.ts {
		d.finish()
		if d.building { // only parameter sets so far: they join the next picture
			d.ts = h.ts
		}
	}
	if !d.building {
		if d.emitted && h.ts == d.lastEmitTS {
			d.ignoreMark = true // this sender marks more than the last packet
		}
		d.startUnit(h, p)
	}
	d.arrival = arrival
	if len(p) >= d.headerSize() {
		if d.h265 {
			d.pushH265(p)
		} else {
			d.pushH264(p)
		}
	} else {
		d.damaged = true
	}
	if h.marker && !d.ignoreMark {
		d.finish()
	}
}

func (d *nalDepacketizer) pushH264(p []byte) {
	switch t := int(p[0] & 0x1F); {
	case t >= 1 && t <= 23:
		d.inFU = false
		d.addNAL(p)
	case t == 24: // STAP-A
		d.inFU = false
		d.aggregate(p[1:])
	case t == 28: // FU-A
		if len(p) < 2 {
			d.damaged = true
			return
		}
		fu := p[1]
		var hdr [1]byte
		hdr[0] = p[0]&0xE0 | fu&0x1F
		d.fragment(fu&0x80 != 0, fu&0x40 != 0, int(fu&0x1F), hdr[:], p[2:])
	case t >= 25 && t <= 29: // STAP-B, MTAP and FU-B need packetization-mode 2
		d.damaged = true
	}
}

func (d *nalDepacketizer) pushH265(p []byte) {
	switch t := int(p[0] >> 1 & 0x3F); {
	case t < 48:
		d.inFU = false
		d.addNAL(p)
	case t == 48: // aggregation packet
		d.inFU = false
		d.aggregate(p[2:])
	case t == 49: // fragmentation unit
		if len(p) < 3 {
			d.damaged = true
			return
		}
		fu := p[2]
		var hdr [2]byte
		hdr[0] = p[0]&0x81 | fu&0x3F<<1
		hdr[1] = p[1]
		d.fragment(fu&0x80 != 0, fu&0x40 != 0, int(fu&0x3F), hdr[:], p[3:])
	case t == 50: // PACI
		d.damaged = true
	}
}

func (d *nalDepacketizer) aggregate(b []byte) {
	for len(b) > 0 {
		if len(b) < 2 {
			d.damaged = true
			return
		}
		n := int(b[0])<<8 | int(b[1])
		b = b[2:]
		if n > len(b) {
			d.damaged = true
			return
		}
		if n > 0 {
			d.addNAL(b[:n])
		}
		b = b[n:]
	}
}

func (d *nalDepacketizer) addNAL(nal []byte) {
	if len(nal) < d.headerSize() {
		d.damaged = true
		return
	}
	t := d.nalType(nal)
	if d.isDroppable(t) {
		return
	}
	nal = d.lowDelay(nal)
	if !d.b.write(startCode) || !d.b.write(nal) {
		d.overflow()
		return
	}
	d.noteNAL(t, nal)
}

func (d *nalDepacketizer) overflow() {
	d.damaged = true
	d.b.begin(d.headroom())
	d.inFU = false
}

func (d *nalDepacketizer) noteNAL(t int, nal []byte) {
	if d.isVCL(t) {
		d.hasVCL = true
		d.key = d.key || d.isKey(t)
		return
	}
	if k, ok := d.paramKind(nal); ok {
		d.seen[k] = true
		d.pending[k] = append(append(d.pending[k], startCode...), nal...)
	}
}

func (d *nalDepacketizer) fragment(start, end bool, t int, hdr, body []byte) {
	if start {
		if d.inFU {
			d.damaged = true
		}
		d.inFU = true
		d.fuStart = d.b.size() + len(startCode)
		if !d.b.write(startCode) || !d.b.write(hdr) {
			d.overflow()
			return
		}
	} else if !d.inFU {
		d.damaged = true
		return
	}
	if !d.b.write(body) {
		d.overflow()
		return
	}
	if end {
		d.inFU = false
		nal := d.b.bytes()[d.fuStart:]
		if d.isDroppable(t) {
			d.b.end = d.b.off + d.fuStart - len(startCode)
			return
		}
		d.noteNAL(t, nal)
	}
}

func (d *nalDepacketizer) commitParams() {
	for k := range d.seen {
		if !d.seen[k] {
			continue
		}
		d.seen[k] = false
		d.cache[k], d.pending[k] = d.pending[k], d.cache[k][:0]
		if k == paramSPS {
			d.noteSPS(firstNAL(d.cache[k]))
		}
	}
}

func firstNAL(annexB []byte) []byte {
	b := annexB[min(len(startCode), len(annexB)):]
	if i := bytes.Index(b, startCode); i >= 0 {
		b = b[:i]
	}
	return b
}

func (d *nalDepacketizer) noteSPS(nal []byte) {
	if bytes.Equal(nal, d.lastSPS) {
		return
	}
	d.lastSPS = append(d.lastSPS[:0], nal...)
	var w, h int
	var ok bool
	if d.h265 {
		w, h, ok = h265SPSSize(nal)
	} else {
		w, h, ok = h264SPSSize(nal)
	}
	if ok {
		d.t.setSize(w, h)
	}
}

// finish closes the current unit. A unit without picture data stays open and
// its parameter sets are carried into the next picture.
func (d *nalDepacketizer) finish() {
	if !d.building {
		return
	}
	if d.inFU {
		d.damaged = true
		d.inFU = false
	}
	if d.damaged {
		d.commitParams()
		d.b.discard()
		d.building, d.damaged = false, false
		d.hasVCL, d.key = false, false
		d.t.dropUnit()
		return
	}
	if !d.hasVCL {
		return
	}
	hadParams := d.seen
	d.commitParams()
	if d.key && !d.hasAllParams(&hadParams) {
		d.insertParts = [3][]byte{d.cache[0], d.cache[1], d.cache[2]}
		if !d.b.prepend(&d.insertParts) {
			d.insertParts = [3][]byte{}
			d.b.discard()
			d.building = false
			d.t.dropUnit()
			return
		}
		d.insertParts = [3][]byte{}
	}
	d.building = false
	d.emitted, d.lastEmitTS = true, d.ts
	d.t.emit(d.b.take(), d.ts, d.key, d.arrival)
}

func (d *nalDepacketizer) hasAllParams(seen *[paramKinds]bool) bool {
	if d.h265 && !seen[paramVPS] {
		return false
	}
	return seen[paramSPS] && seen[paramPPS]
}
