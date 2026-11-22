//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"math/bits"
)

// H.264 parameter set and slice header parsing, for the VA-API decoder. Only
// headers are read: slice data goes to the hardware untouched.

var (
	errH264SPS   = fmt.Errorf("%w: h264 sequence parameter set", ErrCorrupt)
	errH264PPS   = fmt.Errorf("%w: h264 picture parameter set", ErrCorrupt)
	errH264Slice = fmt.Errorf("%w: h264 slice header", ErrCorrupt)
	errH264NoPPS = fmt.Errorf("%w: h264 slice refers to a missing parameter set", ErrCorrupt)
	errH264Ref   = fmt.Errorf("%w: h264 picture has no reference", ErrCorrupt)

	// These describe valid streams the hardware path cannot take, so they are
	// plain errors and let failover try another backend.
	errH264Field   = errors.New("vaapi: h264 field pictures are not supported")
	errH264FMO     = errors.New("vaapi: h264 slice groups are not supported")
	errH264Part    = errors.New("vaapi: h264 data partitioning is not supported")
	errH264Format  = errors.New("vaapi: only 8-bit 4:2:0 h264 is supported")
	errH264Profile = errors.New("vaapi: h264 profile not supported by this device")
)

// vaBits reads a NAL unit bit by bit, skipping emulation prevention bytes, so
// nothing needs to be copied.
type vaBits struct {
	b     []byte
	i     int
	bit   uint8
	zeros uint8
	epb   int
	bad   bool
}

func (r *vaBits) u1() uint32 {
	if r.i >= len(r.b) {
		r.bad = true
		return 0
	}
	c := r.b[r.i]
	v := c >> (7 - r.bit) & 1
	r.bit++
	if r.bit == 8 {
		r.bit = 0
		if c == 0 {
			r.zeros++
		} else {
			r.zeros = 0
		}
		r.i++
		if r.zeros >= 2 && r.i < len(r.b) && r.b[r.i] == 3 {
			r.i++
			r.epb++
			r.zeros = 0
		}
	}
	return uint32(v)
}

func (r *vaBits) u(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		v = v<<1 | r.u1()
	}
	return v
}

func (r *vaBits) flag() bool { return r.u1() != 0 }

func (r *vaBits) ue() uint32 {
	n := 0
	for r.u1() == 0 {
		n++
		if n > 31 || r.bad {
			r.bad = true
			return 0
		}
	}
	return 1<<n - 1 + r.u(n)
}

func (r *vaBits) se() int32 {
	v := r.ue()
	if v&1 != 0 {
		return int32(v/2) + 1
	}
	return -int32(v / 2)
}

// rbspBits is the read position with emulation prevention bytes removed.
func (r *vaBits) rbspBits() int { return (r.i-r.epb)*8 + int(r.bit) }

// moreData implements more_rbsp_data().
func (r *vaBits) moreData() bool {
	n := len(r.b)
	for n > 0 && r.b[n-1] == 0 {
		n--
	}
	if n == 0 || r.bad {
		return false
	}
	last := (n-1)*8 + 7 - bits.TrailingZeros8(r.b[n-1])
	return r.i*8+int(r.bit) < last
}

// Scaling lists are kept in coded (zigzag) order with their fallback state,
// because resolving them needs both parameter sets.
const (
	vaListAbsent uint8 = iota
	vaListDefault
	vaListCoded
)

type vaScaling struct {
	present bool
	state   [12]uint8
	l4      [6][16]uint8
	l8      [6][64]uint8
}

func (r *vaBits) scalingList(dst []uint8) (useDefault bool) {
	last, next := int32(8), int32(8)
	for j := range dst {
		if next != 0 {
			d := r.se()
			if d < -128 || d > 127 {
				r.bad = true
				return false
			}
			next = (last + d + 256) % 256
			useDefault = j == 0 && next == 0
		}
		if next != 0 {
			last = next
		}
		dst[j] = uint8(last)
	}
	return useDefault
}

func (r *vaBits) scaling(s *vaScaling, count int) {
	s.present = true
	for i := 0; i < count; i++ {
		if !r.flag() {
			continue
		}
		var d bool
		if i < 6 {
			d = r.scalingList(s.l4[i][:])
		} else {
			d = r.scalingList(s.l8[i-6][:])
		}
		s.state[i] = vaListCoded
		if d {
			s.state[i] = vaListDefault
		}
	}
}

type vaSPS struct {
	id, profile, constraints, level   int
	chroma, depthY, depthC            int
	bypass                            bool
	scaling                           vaScaling
	log2FrameNum, pocType, log2PocLsb int
	deltaZero                         bool
	offsetNonRef, offsetTopBottom     int32
	cycle                             int
	offsetRef                         [255]int32
	numRef                            int
	gaps                              bool
	mbW, mapH, mbH                    int
	frameMbsOnly, mbaff, direct8      bool
	width, height, cropX, cropY       int
	restricted                        bool
	vuiReorder, vuiDecBuffering       int
	dpbFrames                         int
	reorder                           int // -1 when the stream does not bound it
	version                           uint32
}

type vaPPS struct {
	id, spsID                    int
	cabac, bottomPOC, fmo        bool
	numRef                       [2]int
	weightedPred                 bool
	weightedBipred               int
	qp, qs, chromaQP, chromaQP2  int
	deblockCtl, constrainedIntra bool
	redundant, t8x8              bool
	scaling                      vaScaling
	version                      uint32
}

func (r *vaBits) hrd() {
	n := r.ue() + 1
	if n > 32 {
		r.bad = true
		return
	}
	r.u(8)
	for i := uint32(0); i < n && !r.bad; i++ {
		r.ue()
		r.ue()
		r.u1()
	}
	r.u(20)
}

// vui reads the fields up to the reordering limits. A damaged or truncated VUI
// only loses those limits, so it does not invalidate the SPS.
func (r *vaBits) vui(s *vaSPS) {
	if r.flag() && r.u(8) == 255 { // aspect_ratio_idc, Extended_SAR
		r.u(32)
	}
	if r.flag() { // overscan
		r.u1()
	}
	if r.flag() { // video signal
		r.u(4)
		if r.flag() {
			r.u(24)
		}
	}
	if r.flag() { // chroma location
		r.ue()
		r.ue()
	}
	if r.flag() { // timing
		r.u(32)
		r.u(32)
		r.u1()
	}
	nal := r.flag()
	if nal {
		r.hrd()
	}
	vcl := r.flag()
	if vcl {
		r.hrd()
	}
	if nal || vcl {
		r.u1()
	}
	r.u1() // pic_struct_present_flag
	if !r.flag() || r.bad {
		return
	}
	r.u1()
	r.ue()
	r.ue()
	r.ue()
	r.ue()
	reorder, buffering := r.ue(), r.ue()
	if !r.bad && reorder <= 16 && buffering <= 16 && reorder <= buffering {
		s.restricted, s.vuiReorder, s.vuiDecBuffering = true, int(reorder), int(buffering)
	}
}

func vaMaxDpbMbs(s *vaSPS) int {
	switch l := s.level; {
	case l == 9 || l == 10 || (l == 11 && s.constraints&0x10 != 0 && (s.profile == 66 || s.profile == 77)):
		return 396
	case l == 11:
		return 900
	case l <= 20:
		return 2376
	case l == 21:
		return 4752
	case l <= 30:
		return 8100
	case l == 31:
		return 18000
	case l == 32:
		return 20480
	case l <= 41:
		return 32768
	case l == 42:
		return 34816
	case l == 50:
		return 110400
	case l <= 52:
		return 184320
	}
	return 696320
}

// parseSPS reads a sequence parameter set. b includes the NAL header.
func parseSPS(b []byte, s *vaSPS) error {
	*s = vaSPS{chroma: 1, depthY: 8, depthC: 8}
	r := vaBits{b: b}
	r.u(8)
	s.profile = int(r.u(8))
	s.constraints = int(r.u(8))
	s.level = int(r.u(8))
	id := r.ue()
	if r.bad || id > 31 {
		return errH264SPS
	}
	s.id = int(id)
	switch s.profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		c := r.ue()
		if c > 3 {
			return errH264SPS
		}
		s.chroma = int(c)
		if c == 3 && r.flag() { // separate_colour_plane_flag
			return errH264Format
		}
		dy, dc := r.ue(), r.ue()
		if dy > 6 || dc > 6 {
			return errH264SPS
		}
		s.depthY, s.depthC = int(dy)+8, int(dc)+8
		s.bypass = r.flag()
		if r.flag() {
			n := 8
			if c == 3 {
				n = 12
			}
			r.scaling(&s.scaling, n)
		}
	}
	lf, pt := r.ue(), r.ue()
	if lf > 12 || pt > 2 {
		return errH264SPS
	}
	s.log2FrameNum, s.pocType = int(lf)+4, int(pt)
	switch pt {
	case 0:
		l := r.ue()
		if l > 12 {
			return errH264SPS
		}
		s.log2PocLsb = int(l) + 4
	case 1:
		s.deltaZero = r.flag()
		s.offsetNonRef, s.offsetTopBottom = r.se(), r.se()
		n := r.ue()
		if n > 255 {
			return errH264SPS
		}
		s.cycle = int(n)
		for i := 0; i < s.cycle; i++ {
			s.offsetRef[i] = r.se()
		}
	}
	nr := r.ue()
	if nr > 16 {
		return errH264SPS
	}
	s.numRef = int(nr)
	s.gaps = r.flag()
	mw, mh := r.ue(), r.ue()
	if r.bad || mw > 1023 || mh > 1023 {
		return errH264SPS
	}
	s.mbW, s.mapH = int(mw)+1, int(mh)+1
	s.frameMbsOnly = r.flag()
	s.mbH = s.mapH
	if !s.frameMbsOnly {
		s.mbH *= 2
		s.mbaff = r.flag()
	}
	s.direct8 = r.flag()
	s.width, s.height = s.mbW*16, s.mbH*16
	if r.flag() {
		l, rr, t, bt := r.ue(), r.ue(), r.ue(), r.ue()
		if r.bad || l > 1<<16 || rr > 1<<16 || t > 1<<16 || bt > 1<<16 {
			return errH264SPS
		}
		ux, uy := 1, 1
		if s.chroma == 1 || s.chroma == 2 {
			ux = 2
		}
		if s.chroma == 1 {
			uy = 2
		}
		if !s.frameMbsOnly {
			uy *= 2
		}
		s.cropX, s.cropY = int(l)*ux, int(t)*uy
		s.width -= int(l+rr) * ux
		s.height -= int(t+bt) * uy
		if s.width <= 0 || s.height <= 0 {
			return errH264SPS
		}
	}
	if r.bad {
		return errH264SPS
	}
	if r.flag() {
		v := r // a bad VUI must not poison the checks above
		v.vui(s)
	}
	s.dpbFrames = min(vaMaxDpbMbs(s)/(s.mbW*s.mbH), 16)
	if s.restricted {
		s.dpbFrames = s.vuiDecBuffering
	}
	s.dpbFrames = max(s.dpbFrames, s.numRef, 1)
	intraOnly := s.constraints&0x10 != 0 && (s.profile == 44 || s.profile == 100 || s.profile == 110 || s.profile == 122 || s.profile == 244)
	switch {
	case s.pocType == 2, s.profile == 66, intraOnly:
		s.reorder = 0
	case s.restricted:
		s.reorder = s.vuiReorder
	default:
		s.reorder = -1
	}
	return nil
}

// parsePPS reads a picture parameter set. chroma444 reports the chroma format
// of a referenced SPS, which sets the number of 8x8 scaling lists.
func parsePPS(b []byte, p *vaPPS, chroma444 func(id int) bool) error {
	*p = vaPPS{}
	r := vaBits{b: b}
	r.u(8)
	id, sid := r.ue(), r.ue()
	if r.bad || id > 255 || sid > 31 {
		return errH264PPS
	}
	p.id, p.spsID = int(id), int(sid)
	p.cabac = r.flag()
	p.bottomPOC = r.flag()
	if r.ue() > 0 {
		// Nothing after the slice group map is needed: such a stream is refused
		// when a slice uses this PPS.
		p.fmo = true
		if r.bad {
			return errH264PPS
		}
		return nil
	}
	n0, n1 := r.ue(), r.ue()
	if n0 > 31 || n1 > 31 {
		return errH264PPS
	}
	p.numRef = [2]int{int(n0) + 1, int(n1) + 1}
	p.weightedPred = r.flag()
	p.weightedBipred = int(r.u(2))
	qp, qs, cq := r.se(), r.se(), r.se()
	if qp < -62 || qp > 25 || qs < -26 || qs > 25 || cq < -12 || cq > 12 || p.weightedBipred > 2 {
		return errH264PPS
	}
	p.qp, p.qs, p.chromaQP, p.chromaQP2 = int(qp), int(qs), int(cq), int(cq)
	p.deblockCtl = r.flag()
	p.constrainedIntra = r.flag()
	p.redundant = r.flag()
	if r.bad {
		return errH264PPS
	}
	if r.moreData() {
		p.t8x8 = r.flag()
		if r.flag() {
			n := 6
			if p.t8x8 {
				n = 8
				if chroma444(p.spsID) {
					n = 12
				}
			}
			r.scaling(&p.scaling, n)
		}
		c2 := r.se()
		if r.bad || c2 < -12 || c2 > 12 {
			return errH264PPS
		}
		p.chromaQP2 = int(c2)
	}
	return nil
}

var (
	vaDefault4 = [2][16]uint8{
		{6, 13, 13, 20, 20, 20, 28, 28, 28, 28, 32, 32, 32, 37, 37, 42},
		{10, 14, 14, 20, 20, 20, 24, 24, 24, 24, 27, 27, 27, 30, 30, 34},
	}
	vaDefault8 = [2][64]uint8{
		{6, 10, 10, 13, 11, 13, 16, 16, 16, 16, 18, 18, 18, 18, 18, 23,
			23, 23, 23, 23, 23, 25, 25, 25, 25, 25, 25, 25, 27, 27, 27, 27,
			27, 27, 27, 27, 29, 29, 29, 29, 29, 29, 29, 31, 31, 31, 31, 31,
			31, 33, 33, 33, 33, 33, 36, 36, 36, 36, 38, 38, 38, 40, 40, 42},
		{9, 13, 13, 15, 13, 15, 17, 17, 17, 17, 19, 19, 19, 19, 19, 21,
			21, 21, 21, 21, 21, 22, 22, 22, 22, 22, 22, 22, 24, 24, 24, 24,
			24, 24, 24, 24, 25, 25, 25, 25, 25, 25, 25, 27, 27, 27, 27, 27,
			27, 28, 28, 28, 28, 28, 30, 30, 30, 30, 32, 32, 32, 33, 33, 35},
	}
	vaZigzag4 = [16]uint8{0, 1, 4, 8, 5, 2, 3, 6, 9, 12, 13, 10, 7, 11, 14, 15}
	vaZigzag8 = [64]uint8{0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
		12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
		35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
		58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63}
)

// vaLists are resolved scaling lists in zigzag order.
type vaLists struct {
	l4 [6][16]uint8
	l8 [2][64]uint8
}

// resolve applies one parameter set's lists over fallback, per table 7-2:
// the first list of each group falls back to base, later ones to their
// predecessor.
func (l *vaLists) resolve(s *vaScaling, base *vaLists) {
	for i := 0; i < 6; i++ {
		switch {
		case s.state[i] == vaListCoded:
			l.l4[i] = s.l4[i]
		case s.state[i] == vaListDefault:
			l.l4[i] = vaDefault4[i/3]
		case i == 0 || i == 3:
			l.l4[i] = base.l4[i]
		default:
			l.l4[i] = l.l4[i-1]
		}
	}
	for i := 0; i < 2; i++ {
		switch s.state[6+i] {
		case vaListCoded:
			l.l8[i] = s.l8[i]
		case vaListDefault:
			l.l8[i] = vaDefault8[i]
		default:
			l.l8[i] = base.l8[i]
		}
	}
}

// vaResolveScaling computes the lists in effect for a picture.
func vaResolveScaling(sps *vaSPS, pps *vaPPS, out *vaLists) {
	var defaults vaLists
	for i := range defaults.l4 {
		defaults.l4[i] = vaDefault4[i/3]
	}
	defaults.l8 = vaDefault8
	var seq vaLists
	if sps.scaling.present {
		seq.resolve(&sps.scaling, &defaults)
	} else {
		for i := range seq.l4 {
			for j := range seq.l4[i] {
				seq.l4[i][j] = 16
			}
		}
		for i := range seq.l8 {
			for j := range seq.l8[i] {
				seq.l8[i][j] = 16
			}
		}
	}
	switch {
	case !pps.scaling.present:
		*out = seq
	case sps.scaling.present:
		out.resolve(&pps.scaling, &seq) // fallback rule B
	default:
		out.resolve(&pps.scaling, &defaults) // fallback rule A
	}
}

type vaRefMod struct {
	op  uint8
	val uint32
}

type vaMMCO struct {
	op   uint8
	a, b uint32
}

// Slice types after reduction modulo 5.
const (
	h264P = iota
	h264B
	h264I
	h264SP
	h264SI
)

// vaSlice is one parsed slice. data aliases the input until the picture is sent.
type vaSlice struct {
	nalType, refIdc            uint8
	firstMB                    uint32
	typ                        uint8
	pps                        *vaPPS
	sps                        *vaSPS
	frameNum                   int32
	idrID                      uint32
	pocLsb, deltaBottom        int32
	delta                      [2]int32
	redundant                  uint32
	direct                     bool
	numRef                     [2]int
	nmods                      [2]int
	mods                       [2][33]vaRefMod
	lumaDenom, chromaDenom     uint8
	lumaFlag, chromaFlag       [2]bool
	lumaW, lumaO               [2][32]int16
	chromaW, chromaO           [2][32][2]int16
	hasTable                   bool
	noOutputPrior, longTermRef bool
	adaptive, mmco5            bool
	nmmco                      int
	mmco                       [66]vaMMCO
	cabacInit                  uint8
	qpDelta                    int8
	deblock                    uint8
	alpha, beta                int8
	bitOffset                  int
	data                       []byte
}

func (s *vaSlice) idr() bool { return s.nalType == 5 }

// parseSlice reads a slice header. d supplies the parameter sets.
func parseSlice(b []byte, s *vaSlice, spss *[32]*vaSPS, ppss *[256]*vaPPS) error {
	*s = vaSlice{data: b}
	r := vaBits{b: b}
	h := r.u(8)
	s.refIdc, s.nalType = uint8(h>>5&3), uint8(h&31)
	s.firstMB = r.ue()
	t := r.ue()
	id := r.ue()
	if r.bad || t > 9 || id > 255 {
		return errH264Slice
	}
	s.typ = uint8(t % 5)
	if s.nalType == 5 && s.typ != h264I && s.typ != h264SI {
		return errH264Slice
	}
	pps := ppss[id]
	if pps == nil || spss[pps.spsID] == nil {
		return errH264NoPPS
	}
	sps := spss[pps.spsID]
	s.pps, s.sps = pps, sps
	if pps.fmo {
		return errH264FMO
	}
	if s.typ == h264SP || s.typ == h264SI {
		return errH264Profile
	}
	if s.firstMB >= uint32(sps.mbW*sps.mbH) {
		return errH264Slice
	}
	s.frameNum = int32(r.u(sps.log2FrameNum))
	if !sps.frameMbsOnly && r.flag() {
		return errH264Field
	}
	if s.idr() {
		s.idrID = r.ue()
	}
	switch sps.pocType {
	case 0:
		s.pocLsb = int32(r.u(sps.log2PocLsb))
		if pps.bottomPOC {
			s.deltaBottom = r.se()
		}
	case 1:
		if !sps.deltaZero {
			s.delta[0] = r.se()
			if pps.bottomPOC {
				s.delta[1] = r.se()
			}
		}
	}
	if pps.redundant {
		s.redundant = r.ue()
	}
	if s.typ == h264B {
		s.direct = r.flag()
	}
	if s.typ == h264P || s.typ == h264B {
		s.numRef[0] = pps.numRef[0]
		if s.typ == h264B {
			s.numRef[1] = pps.numRef[1]
		}
		if r.flag() {
			n := r.ue()
			if n > 31 {
				return errH264Slice
			}
			s.numRef[0] = int(n) + 1
			if s.typ == h264B {
				n = r.ue()
				if n > 31 {
					return errH264Slice
				}
				s.numRef[1] = int(n) + 1
			}
		}
		lists := 1
		if s.typ == h264B {
			lists = 2
		}
		for l := 0; l < lists; l++ {
			if !r.flag() {
				continue
			}
			for {
				op := r.ue()
				if op == 3 {
					break
				}
				if r.bad || op > 3 || s.nmods[l] == len(s.mods[l]) {
					return errH264Slice
				}
				s.mods[l][s.nmods[l]] = vaRefMod{uint8(op), r.ue()}
				s.nmods[l]++
			}
		}
		if (pps.weightedPred && s.typ == h264P) || (pps.weightedBipred == 1 && s.typ == h264B) {
			if err := r.predWeights(s, sps, lists); err != nil {
				return err
			}
		}
	}
	if s.refIdc != 0 {
		if s.idr() {
			s.noOutputPrior = r.flag()
			s.longTermRef = r.flag()
		} else if s.adaptive = r.flag(); s.adaptive {
			for {
				op := r.ue()
				if op == 0 {
					break
				}
				if r.bad || op > 6 || s.nmmco == len(s.mmco) {
					return errH264Slice
				}
				m := vaMMCO{op: uint8(op)}
				if op != 5 {
					m.a = r.ue()
				}
				if op == 3 {
					m.b = r.ue()
				}
				s.mmco5 = s.mmco5 || op == 5
				s.mmco[s.nmmco] = m
				s.nmmco++
			}
		}
	}
	if pps.cabac && s.typ != h264I {
		c := r.ue()
		if c > 2 {
			return errH264Slice
		}
		s.cabacInit = uint8(c)
	}
	qd := r.se()
	if qd < -87 || qd > 77 {
		return errH264Slice
	}
	s.qpDelta = int8(qd)
	if pps.deblockCtl {
		dd := r.ue()
		if dd > 2 {
			return errH264Slice
		}
		s.deblock = uint8(dd)
		if dd != 1 {
			a, bt := r.se(), r.se()
			if a < -6 || a > 6 || bt < -6 || bt > 6 {
				return errH264Slice
			}
			s.alpha, s.beta = int8(a), int8(bt)
		}
	}
	if r.bad {
		return errH264Slice
	}
	s.bitOffset = r.rbspBits()
	return nil
}

func (r *vaBits) predWeights(s *vaSlice, sps *vaSPS, lists int) error {
	ld := r.ue()
	var cd uint32
	if sps.chroma != 0 {
		cd = r.ue()
	}
	if ld > 7 || cd > 7 {
		return errH264Slice
	}
	s.hasTable = true
	s.lumaDenom, s.chromaDenom = uint8(ld), uint8(cd)
	for l := 0; l < lists; l++ {
		for i := 0; i < s.numRef[l]; i++ {
			s.lumaW[l][i] = 1 << ld
			s.chromaW[l][i] = [2]int16{1 << cd, 1 << cd}
			if r.flag() {
				s.lumaFlag[l] = true
				w, o := r.se(), r.se()
				if w < -128 || w > 127 || o < -128 || o > 127 {
					return errH264Slice
				}
				s.lumaW[l][i], s.lumaO[l][i] = int16(w), int16(o)
			}
			if sps.chroma != 0 && r.flag() {
				s.chromaFlag[l] = true
				for c := 0; c < 2; c++ {
					w, o := r.se(), r.se()
					if w < -128 || w > 127 || o < -128 || o > 127 {
						return errH264Slice
					}
					s.chromaW[l][i][c], s.chromaO[l][i][c] = int16(w), int16(o)
				}
			}
		}
	}
	return nil
}

// samePicture implements the first-slice detection of 7.4.1.2.4.
func samePicture(a, b *vaSlice) bool {
	return a.pps.id == b.pps.id && a.frameNum == b.frameNum &&
		(a.refIdc == 0) == (b.refIdc == 0) && a.idr() == b.idr() &&
		a.idrID == b.idrID && a.pocLsb == b.pocLsb && a.deltaBottom == b.deltaBottom &&
		a.delta == b.delta
}

// videoSignal writes video_signal_type_present_flag and its fields, which are
// the same in H.264 and H.265 VUI.
func (w *vaBitWriter) videoSignal(r ColorRange) {
	if r == RangeUnspecified {
		w.u(1, 0)
		return
	}
	full := uint32(0)
	if r == RangeFull {
		full = 1
	}
	w.u(1, 1)
	w.u(3, 5) // video_format: unspecified
	w.u(1, full)
	w.u(1, 0) // colour_description_present_flag
}
