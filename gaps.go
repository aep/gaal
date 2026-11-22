package gaal

import (
	"bytes"
	"fmt"
)

// errRefLost is returned by Send for H.264 pictures that follow a lost
// reference picture.
var errRefLost = fmt.Errorf("%w: h264: a reference picture is missing, waiting for an IDR picture", ErrCorrupt)

// frameNumGuard notices H.264 access units that are missing, by frame_num:
// each reference picture counts it up by one, so a jump means one was lost.
// The pictures after it until the next IDR picture would be decoded from a
// guess, which platform decoders make without telling (error concealment that
// cannot be turned off), so they are kept from the decoder instead.
//
// A stream whose SPS sets gaps_in_frame_num_value_allowed_flag may skip
// frame_num values on purpose, for example when temporal layers are dropped,
// so its gaps are not taken as loss. A picture with memory management
// operation 5 restarts frame_num, which is not read here: the next picture is
// taken as following a loss.
type frameNumGuard struct {
	sps     [32]h264Seq
	pps     [256]int16 // SPS id + 1
	buf     []byte
	prevRef uint32 // frame_num of the last reference picture
	ok      bool   // nothing was lost since the last IDR picture
}

// accept reports whether the picture of a packet can be decoded without a
// guess. A packet without a picture is accepted.
func (g *frameNumGuard) accept(b []byte) bool {
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 || pos+i+3 >= len(b) {
			return true
		}
		start := pos + i + 3
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		nal := b[start:end]
		pos = end
		if len(nal) < 2 {
			continue
		}
		switch nal[0] & 0x1F {
		case 7:
			g.buf = appendRBSP(g.buf[:0], nal[1:], len(nal))
			r := bitReader{b: g.buf}
			r.skip(24)
			if id := r.ue(); !r.bad && id < 32 {
				g.sps[id] = parseH264Seq(g.buf)
			}
		case 8:
			g.buf = appendRBSP(g.buf[:0], nal[1:], 16)
			r := bitReader{b: g.buf}
			pps, sps := r.ue(), r.ue()
			if !r.bad && pps < 256 && sps < 32 {
				g.pps[pps] = int16(sps) + 1
			}
		case 5:
			g.ok, g.prevRef = true, 0
			return true
		case 1:
			if !g.ok {
				return false
			}
			g.ok = g.slice(nal)
			return g.ok
		}
	}
}

// slice checks the frame_num of a non-IDR slice and notes it.
func (g *frameNumGuard) slice(nal []byte) bool {
	g.buf = appendRBSP(g.buf[:0], nal[1:], 32)
	r := bitReader{b: g.buf}
	r.ue() // first_mb_in_slice
	r.ue() // slice_type
	pps := r.ue()
	if r.bad || pps >= 256 || g.pps[pps] == 0 {
		return !r.bad // unknown parameter sets: the decoder reports it
	}
	s := &g.sps[g.pps[pps]-1]
	if !s.valid {
		return true
	}
	if s.separatePlanes {
		r.skip(2) // colour_plane_id
	}
	n := r.u(s.frameBits)
	if r.bad {
		return false
	}
	mask := uint32(1)<<s.frameBits - 1
	if n != g.prevRef && n != (g.prevRef+1)&mask && !s.gapsAllowed {
		return false
	}
	if nal[0]&0x60 != 0 { // nal_ref_idc
		g.prevRef = n
	}
	return true
}
