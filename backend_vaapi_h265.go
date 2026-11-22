//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"math/bits"
)

// H.265 parameter set and slice segment header parsing for the VA-API decoder.
// Only headers are read, and everything the hardware will rely on is range
// checked here: a stream that fails these checks never reaches the driver.

var (
	errH265SPS   = fmt.Errorf("%w: h265 sequence parameter set", ErrCorrupt)
	errH265PPS   = fmt.Errorf("%w: h265 picture parameter set", ErrCorrupt)
	errH265Slice = fmt.Errorf("%w: h265 slice header", ErrCorrupt)
	errH265NoPPS = fmt.Errorf("%w: h265 slice refers to a missing parameter set", ErrCorrupt)
	errH265Ref   = fmt.Errorf("%w: h265 picture has no reference", ErrCorrupt)

	// These describe valid streams the hardware path cannot take, so they are
	// plain errors and let failover try another backend.
	errH265Format  = errors.New("vaapi: only 8-bit 4:2:0 h265 is supported")
	errH265Profile = errors.New("vaapi: only the h265 main profile is supported")
	errH265Size    = errors.New("vaapi: h265 picture size not supported by this device")
	errH265Tools   = errors.New("vaapi: h265 tiles combined with wavefront are not supported")
)

const (
	h265SliceB = 0
	h265SliceP = 1
	h265SliceI = 2

	h265NalRADLN  = 6
	h265NalRADLR  = 7
	h265NalRASLN  = 8
	h265NalRASLR  = 9
	h265NalBLALP  = 16
	h265NalBLARAD = 17
	h265NalBLANLP = 18
	h265NalIDRW   = 19
	h265NalIDRN   = 20
	h265NalCRA    = 21
	h265NalVPS    = 32
	h265NalSPS    = 33
	h265NalPPS    = 34
	h265NalEOS    = 36
	h265NalEOB    = 37

	h265MaxRefs   = 16 // entries of a reference picture set or list
	h265MaxSlices = 600
	h265MinDim    = 16 // below this the hardware path is not attempted
)

// h265RPS is a short-term reference picture set after derivation (7.4.8).
type h265RPS struct {
	numNeg, numPos uint8
	s0, s1         [h265MaxRefs]int32
	used0, used1   [h265MaxRefs]bool
}

// h265Scaling holds scaling factors in raster order, as VA-API wants them.
// Index 0 of sl is the 4x4 size, and 32x32 only uses matrices 0 and 3.
type h265Scaling struct {
	sl [4][6][64]uint8
	dc [2][6]uint8
}

var (
	h265DefaultIntra = [64]uint8{
		16, 16, 16, 16, 17, 18, 21, 24,
		16, 16, 16, 16, 17, 19, 22, 25,
		16, 16, 17, 18, 20, 22, 25, 29,
		16, 16, 18, 21, 24, 27, 31, 36,
		17, 17, 20, 24, 30, 35, 41, 47,
		18, 19, 22, 27, 35, 44, 54, 65,
		21, 22, 25, 31, 41, 54, 70, 88,
		24, 25, 29, 36, 47, 65, 88, 115,
	}
	h265DefaultInter = [64]uint8{
		16, 16, 16, 16, 17, 18, 20, 24,
		16, 16, 16, 17, 18, 20, 24, 25,
		16, 16, 17, 18, 20, 24, 25, 28,
		16, 17, 18, 20, 24, 25, 28, 33,
		17, 18, 20, 24, 25, 28, 33, 41,
		18, 20, 24, 25, 28, 33, 41, 54,
		20, 24, 25, 28, 33, 41, 54, 71,
		24, 25, 28, 33, 41, 54, 71, 91,
	}
	h265Diag4 = h265DiagScan(4)
	h265Diag8 = h265DiagScan(8)
)

// h265DiagScan returns the raster position of each coefficient in up-right
// diagonal order (6.5.3).
func h265DiagScan(n int) (out [64]uint8) {
	i, x, y := 0, 0, 0
	for i < n*n {
		for y >= 0 {
			if x < n && y < n {
				out[i] = uint8(y*n + x)
				i++
			}
			y--
			x++
		}
		y, x = x, 0
	}
	return out
}

func (s *h265Scaling) setDefault(size, matrix int) {
	if size == 0 {
		for i := range s.sl[0][matrix] {
			s.sl[0][matrix][i] = 16
		}
		return
	}
	if matrix < 3 {
		s.sl[size][matrix] = h265DefaultIntra
	} else {
		s.sl[size][matrix] = h265DefaultInter
	}
	if size >= 2 {
		s.dc[size-2][matrix] = 16
	}
}

func (s *h265Scaling) setAllDefault() {
	for size := 0; size < 4; size++ {
		for m := 0; m < 6; m++ {
			s.setDefault(size, m)
		}
	}
}

// h265ScalingList reads scaling_list_data() (7.3.4).
func (r *vaBits) h265ScalingList(s *h265Scaling) bool {
	s.setAllDefault()
	for size := 0; size < 4; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for m := 0; m < 6; m += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				delta := r.ue()
				if r.bad || int(delta) > m/step {
					return false
				}
				if delta == 0 {
					s.setDefault(size, m)
					continue
				}
				ref := m - int(delta)*step
				s.sl[size][m] = s.sl[size][ref]
				if size >= 2 {
					s.dc[size-2][m] = s.dc[size-2][ref]
				}
				continue
			}
			next := int32(8)
			if size >= 2 {
				dc := r.se()
				if dc < -7 || dc > 247 {
					return false
				}
				next = dc + 8
				s.dc[size-2][m] = uint8(next)
			}
			n := 16
			scan := &h265Diag4
			if size > 0 {
				n, scan = 64, &h265Diag8
			}
			for i := 0; i < n; i++ {
				d := r.se()
				if r.bad || d < -128 || d > 127 {
					return false
				}
				next = (next + d + 256) & 255
				if next == 0 { // ScalingList values are 1 to 255
					return false
				}
				s.sl[size][m][scan[i]] = uint8(next)
			}
		}
	}
	return !r.bad
}

// h265PTL reads profile_tier_level(1, maxSub) and reports whether the stream
// declares conformance to the Main profile.
func (r *vaBits) h265PTL(maxSub int) (main bool) {
	first := r.u(8)
	compat := r.u(32)
	r.u(32)
	r.u(16)
	r.u(8) // general_level_idc
	idc := first & 31
	main = first>>6 == 0 && (idc == 1 || compat>>30&1 != 0)
	var prof, level [8]bool
	for i := 0; i < maxSub; i++ {
		prof[i] = r.flag()
		level[i] = r.flag()
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.u(2)
		}
	}
	for i := 0; i < maxSub; i++ {
		if prof[i] {
			r.u(32)
			r.u(32)
			r.u(24)
		}
		if level[i] {
			r.u(8)
		}
	}
	return main
}

// h265StRPS reads st_ref_pic_set(idx) (7.3.7).
func (r *vaBits) h265StRPS(sets []h265RPS, idx, num int, out *h265RPS) bool {
	*out = h265RPS{}
	inter := false
	if idx != 0 {
		inter = r.flag()
	}
	if !inter {
		neg, pos := r.ue(), r.ue()
		if r.bad || neg > h265MaxRefs || pos > h265MaxRefs || neg+pos > h265MaxRefs {
			return false
		}
		out.numNeg, out.numPos = uint8(neg), uint8(pos)
		poc := int32(0)
		for i := 0; i < int(neg); i++ {
			d := r.ue()
			if r.bad || d >= 1<<15 {
				return false
			}
			poc -= int32(d) + 1
			out.s0[i], out.used0[i] = poc, r.flag()
		}
		poc = 0
		for i := 0; i < int(pos); i++ {
			d := r.ue()
			if r.bad || d >= 1<<15 {
				return false
			}
			poc += int32(d) + 1
			out.s1[i], out.used1[i] = poc, r.flag()
		}
		return !r.bad
	}
	deltaIdx := uint32(0)
	if idx == num {
		deltaIdx = r.ue()
	}
	if r.bad || int(deltaIdx) >= idx {
		return false
	}
	ref := &sets[idx-int(deltaIdx)-1]
	sign := r.u1()
	abs := r.ue()
	if r.bad || abs >= 1<<15 {
		return false
	}
	deltaRps := int32(abs) + 1
	if sign != 0 {
		deltaRps = -deltaRps
	}
	refNeg, refPos := int(ref.numNeg), int(ref.numPos)
	total := refNeg + refPos
	var used, useDelta [h265MaxRefs + 1]bool
	for j := 0; j <= total; j++ {
		used[j] = r.flag()
		useDelta[j] = true
		if !used[j] {
			useDelta[j] = r.flag()
		}
	}
	if r.bad {
		return false
	}
	i := 0
	add0 := func(p int32, u bool) {
		if i < h265MaxRefs {
			out.s0[i], out.used0[i] = p, u
		}
		i++
	}
	for j := refPos - 1; j >= 0; j-- {
		if p := ref.s1[j] + deltaRps; p < 0 && useDelta[refNeg+j] {
			add0(p, used[refNeg+j])
		}
	}
	if deltaRps < 0 && useDelta[total] {
		add0(deltaRps, used[total])
	}
	for j := 0; j < refNeg; j++ {
		if p := ref.s0[j] + deltaRps; p < 0 && useDelta[j] {
			add0(p, used[j])
		}
	}
	neg := i
	i = 0
	add1 := func(p int32, u bool) {
		if i < h265MaxRefs {
			out.s1[i], out.used1[i] = p, u
		}
		i++
	}
	for j := refNeg - 1; j >= 0; j-- {
		if p := ref.s0[j] + deltaRps; p > 0 && useDelta[j] {
			add1(p, used[j])
		}
	}
	if deltaRps > 0 && useDelta[total] {
		add1(deltaRps, used[total])
	}
	for j := 0; j < refPos; j++ {
		if p := ref.s1[j] + deltaRps; p > 0 && useDelta[refNeg+j] {
			add1(p, used[refNeg+j])
		}
	}
	if neg+i > h265MaxRefs {
		return false
	}
	out.numNeg, out.numPos = uint8(neg), uint8(i)
	return true
}

type h265SPS struct {
	id                                int
	main                              bool
	chroma                            int
	separatePlanes                    bool
	width, height                     int // coded luma samples
	cropX, cropY, outW, outH          int
	depthY, depthC                    int
	log2MaxPocLsb                     int
	maxDecMinus1, reorder, latencyInc int
	log2MinCb, log2Ctb                int
	log2MinTb, log2MaxTb              int
	depthInter, depthIntra            int
	scalingEnabled, scalingPresent    bool
	scaling                           h265Scaling
	amp, sao, pcm, pcmLfOff           bool
	pcmDepthY, pcmDepthC              int
	log2MinPcm, log2MaxPcm            int
	numRPS                            int
	rps                               [64]h265RPS
	longTerm                          bool
	numLtSps                          int
	ltPocLsb                          [32]int32
	ltUsed                            [32]bool
	temporalMvp, strongIntra          bool
	ctbW, ctbH, ctbs                  int
	version                           uint32
}

// parseH265SPS reads a sequence parameter set. b includes the NAL header.
func parseH265SPS(b []byte, s *h265SPS) error {
	*s = h265SPS{}
	r := vaBits{b: b}
	r.u(16)
	r.u(4) // sps_video_parameter_set_id
	maxSub := int(r.u(3))
	if maxSub > 6 {
		return errH265SPS
	}
	r.u1() // sps_temporal_id_nesting_flag
	s.main = r.h265PTL(maxSub)
	id, chroma := r.ue(), r.ue()
	if r.bad || id > 15 || chroma > 3 {
		return errH265SPS
	}
	s.id, s.chroma = int(id), int(chroma)
	if chroma == 3 {
		s.separatePlanes = r.flag()
	}
	w, h := r.ue(), r.ue()
	if r.bad || w == 0 || h == 0 || w > 16888 || h > 16888 {
		return errH265SPS
	}
	s.width, s.height = int(w), int(h)
	s.outW, s.outH = s.width, s.height
	if r.flag() { // conformance_window_flag
		ux, uy := 1, 1
		if chroma == 1 || chroma == 2 {
			ux = 2
		}
		if chroma == 1 {
			uy = 2
		}
		l, rt, t, bt := r.ue(), r.ue(), r.ue(), r.ue()
		if r.bad || l > 16888 || rt > 16888 || t > 16888 || bt > 16888 {
			return errH265SPS
		}
		s.cropX, s.cropY = int(l)*ux, int(t)*uy
		s.outW -= int(l+rt) * ux
		s.outH -= int(t+bt) * uy
		if s.outW <= 0 || s.outH <= 0 {
			return errH265SPS
		}
	}
	dy, dc, pocLsb := r.ue(), r.ue(), r.ue()
	if r.bad || dy > 8 || dc > 8 || pocLsb > 12 {
		return errH265SPS
	}
	s.depthY, s.depthC, s.log2MaxPocLsb = int(dy)+8, int(dc)+8, int(pocLsb)+4
	start := maxSub
	if r.flag() { // sps_sub_layer_ordering_info_present_flag
		start = 0
	}
	for i := start; i <= maxSub; i++ {
		dec, reorder, latency := r.ue(), r.ue(), r.ue()
		if r.bad || dec > 15 || reorder > dec || latency > 1<<31-2 {
			return errH265SPS
		}
		// The values for the highest temporal layer are the ones that apply.
		s.maxDecMinus1, s.reorder, s.latencyInc = int(dec), int(reorder), int(latency)
	}
	minCb, diffCb, minTb, diffTb, depthInter, depthIntra := r.ue(), r.ue(), r.ue(), r.ue(), r.ue(), r.ue()
	if r.bad || minCb > 3 || diffCb > 3 || minTb > 3 || diffTb > 3 {
		return errH265SPS
	}
	s.log2MinCb = int(minCb) + 3
	s.log2Ctb = s.log2MinCb + int(diffCb)
	s.log2MinTb = int(minTb) + 2
	s.log2MaxTb = s.log2MinTb + int(diffTb)
	if s.log2Ctb < 4 || s.log2Ctb > 6 || s.log2MinTb >= s.log2MinCb || s.log2MaxTb > min(s.log2Ctb, 5) {
		return errH265SPS
	}
	maxDepth := uint32(s.log2Ctb - s.log2MinTb)
	if depthInter > maxDepth || depthIntra > maxDepth {
		return errH265SPS
	}
	s.depthInter, s.depthIntra = int(depthInter), int(depthIntra)
	if mask := 1<<s.log2MinCb - 1; s.width&mask != 0 || s.height&mask != 0 {
		return errH265SPS
	}
	s.scalingEnabled = r.flag()
	if s.scalingEnabled {
		s.scalingPresent = r.flag()
		if s.scalingPresent && !r.h265ScalingList(&s.scaling) {
			return errH265SPS
		}
	}
	s.amp = r.flag()
	s.sao = r.flag()
	s.pcm = r.flag()
	if s.pcm {
		s.pcmDepthY, s.pcmDepthC = int(r.u(4))+1, int(r.u(4))+1
		minPcm, diffPcm := r.ue(), r.ue()
		if r.bad || minPcm > 2 || diffPcm > 2 || s.pcmDepthY > s.depthY || s.pcmDepthC > s.depthC {
			return errH265SPS
		}
		s.log2MinPcm = int(minPcm) + 3
		s.log2MaxPcm = s.log2MinPcm + int(diffPcm)
		if s.log2MinPcm < s.log2MinCb || s.log2MaxPcm > min(s.log2Ctb, 5) {
			return errH265SPS
		}
		s.pcmLfOff = r.flag()
	}
	n := r.ue()
	if r.bad || n > 64 {
		return errH265SPS
	}
	s.numRPS = int(n)
	for i := 0; i < s.numRPS; i++ {
		if !r.h265StRPS(s.rps[:], i, s.numRPS, &s.rps[i]) {
			return errH265SPS
		}
	}
	s.longTerm = r.flag()
	if s.longTerm {
		n := r.ue()
		if r.bad || n > 32 {
			return errH265SPS
		}
		s.numLtSps = int(n)
		for i := 0; i < s.numLtSps; i++ {
			s.ltPocLsb[i] = int32(r.u(s.log2MaxPocLsb))
			s.ltUsed[i] = r.flag()
		}
	}
	s.temporalMvp = r.flag()
	s.strongIntra = r.flag()
	if r.bad {
		return errH265SPS
	}
	// VUI and extensions are not needed. Main streams carry no extension that
	// changes decoding, and reordering limits are in the fields above.
	ctb := 1 << s.log2Ctb
	s.ctbW, s.ctbH = (s.width+ctb-1)>>s.log2Ctb, (s.height+ctb-1)>>s.log2Ctb
	s.ctbs = s.ctbW * s.ctbH
	return nil
}

type h265PPS struct {
	id, spsID                       int
	dependent, outputFlag           bool
	extraBits                       int
	signHide, cabacInit             bool
	numRef                          [2]int
	initQP                          int
	constrainedIntra                bool
	transformSkip, cuQPDelta        bool
	diffCuQP                        int
	cbQP, crQP                      int
	sliceChromaQP                   bool
	weighted, weightedBi            bool
	bypass, tiles, entropySync      bool
	tileCols, tileRows              int
	uniform                         bool
	colW                            [20]uint16
	rowH                            [22]uint16
	lfTiles, lfSlices               bool
	dbfControl, dbfOverride, dbfOff bool
	beta, tc                        int
	scalingPresent                  bool
	scaling                         h265Scaling
	listsMod                        bool
	parMerge                        int
	headerExt                       bool
	version                         uint32
}

// parseH265PPS reads a picture parameter set. Checks that need the SPS are done
// by h265Bind when a picture uses it.
func parseH265PPS(b []byte, p *h265PPS) error {
	*p = h265PPS{tileCols: 1, tileRows: 1, uniform: true, lfTiles: true}
	r := vaBits{b: b}
	r.u(16)
	id, sid := r.ue(), r.ue()
	if r.bad || id > 63 || sid > 15 {
		return errH265PPS
	}
	p.id, p.spsID = int(id), int(sid)
	p.dependent = r.flag()
	p.outputFlag = r.flag()
	p.extraBits = int(r.u(3))
	p.signHide = r.flag()
	p.cabacInit = r.flag()
	n0, n1 := r.ue(), r.ue()
	if r.bad || n0 > 14 || n1 > 14 {
		return errH265PPS
	}
	p.numRef = [2]int{int(n0) + 1, int(n1) + 1}
	qp := r.se()
	if r.bad || qp < -26 || qp > 25 {
		return errH265PPS
	}
	p.initQP = int(qp)
	p.constrainedIntra = r.flag()
	p.transformSkip = r.flag()
	p.cuQPDelta = r.flag()
	if p.cuQPDelta {
		d := r.ue()
		if r.bad || d > 3 {
			return errH265PPS
		}
		p.diffCuQP = int(d)
	}
	cb, cr := r.se(), r.se()
	if r.bad || cb < -12 || cb > 12 || cr < -12 || cr > 12 {
		return errH265PPS
	}
	p.cbQP, p.crQP = int(cb), int(cr)
	p.sliceChromaQP = r.flag()
	p.weighted = r.flag()
	p.weightedBi = r.flag()
	p.bypass = r.flag()
	p.tiles = r.flag()
	p.entropySync = r.flag()
	if p.tiles {
		c, rw := r.ue(), r.ue()
		if r.bad || c > 19 || rw > 21 {
			return errH265PPS
		}
		p.tileCols, p.tileRows = int(c)+1, int(rw)+1
		p.uniform = r.flag()
		if !p.uniform {
			for i := 0; i < p.tileCols-1; i++ {
				v := r.ue()
				if r.bad || v > 1<<12 {
					return errH265PPS
				}
				p.colW[i] = uint16(v) + 1
			}
			for i := 0; i < p.tileRows-1; i++ {
				v := r.ue()
				if r.bad || v > 1<<12 {
					return errH265PPS
				}
				p.rowH[i] = uint16(v) + 1
			}
		}
		p.lfTiles = r.flag()
	}
	p.lfSlices = r.flag()
	p.dbfControl = r.flag()
	if p.dbfControl {
		p.dbfOverride = r.flag()
		p.dbfOff = r.flag()
		if !p.dbfOff {
			b, t := r.se(), r.se()
			if r.bad || b < -6 || b > 6 || t < -6 || t > 6 {
				return errH265PPS
			}
			p.beta, p.tc = int(b), int(t)
		}
	}
	p.scalingPresent = r.flag()
	if p.scalingPresent && !r.h265ScalingList(&p.scaling) {
		return errH265PPS
	}
	p.listsMod = r.flag()
	m := r.ue()
	if r.bad || m > 4 {
		return errH265PPS
	}
	p.parMerge = int(m)
	p.headerExt = r.flag()
	if r.bad {
		return errH265PPS
	}
	return nil
}

// h265Layout is what a PPS resolves to against its SPS.
type h265Layout struct {
	colW [20]uint16
	rowH [22]uint16
}

// h265Bind validates a PPS against the SPS it refers to and computes the tile
// grid. Tile sizes are checked against the level limits for the Main profile,
// so the hardware never sees a grid it cannot have been designed for.
func h265Bind(sps *h265SPS, pps *h265PPS, l *h265Layout) error {
	if pps.diffCuQP > sps.log2Ctb-sps.log2MinCb || pps.parMerge+2 > sps.log2Ctb {
		return errH265PPS
	}
	*l = h265Layout{}
	if !pps.tiles {
		l.colW[0], l.rowH[0] = uint16(sps.ctbW), uint16(sps.ctbH)
		return nil
	}
	if pps.entropySync {
		return errH265Tools
	}
	if pps.tileCols > sps.ctbW || pps.tileRows > sps.ctbH {
		return errH265PPS
	}
	if !h265Split(l.colW[:pps.tileCols], pps.colW[:pps.tileCols], sps.ctbW, pps.uniform) ||
		!h265Split(l.rowH[:pps.tileRows], pps.rowH[:pps.tileRows], sps.ctbH, pps.uniform) {
		return errH265PPS
	}
	for _, w := range l.colW[:pps.tileCols] {
		if int(w)<<sps.log2Ctb < 256 {
			return errH265PPS
		}
	}
	for _, h := range l.rowH[:pps.tileRows] {
		if int(h)<<sps.log2Ctb < 64 {
			return errH265PPS
		}
	}
	return nil
}

// h265Split divides total CTBs over len(out) tiles. coded holds the explicit
// sizes of all tiles but the last.
func h265Split(out, coded []uint16, total int, uniform bool) bool {
	n := len(out)
	if uniform {
		for i := range out {
			out[i] = uint16((i+1)*total/n - i*total/n)
		}
		return true
	}
	left := total
	for i := 0; i < n-1; i++ {
		left -= int(coded[i])
		out[i] = coded[i]
	}
	if left <= 0 {
		return false
	}
	out[n-1] = uint16(left)
	return true
}

// h265Slice is one parsed slice segment. data aliases the input until the
// picture is sent.
type h265Slice struct {
	nal, tid               uint8
	first, noOutPrior      bool
	dependent              bool
	pps                    *h265PPS
	sps                    *h265SPS
	addr                   uint32
	typ                    uint8
	picOutput              bool
	pocLsb                 int32
	rpsIdx                 int // index into sps.rps, or -1 for rps below
	rps                    h265RPS
	stBits                 int
	numLt                  int
	ltPoc, ltCycle         [h265MaxRefs]int32
	ltMsb, ltUsed          [h265MaxRefs]bool
	tmvp, saoY, saoC       bool
	numRef                 [2]int
	numCurr                int
	modFlag                [2]bool
	entries                [2][h265MaxRefs]uint8
	mvdZero, cabacInit     bool
	colFromL0              bool
	colIdx                 int
	lumaDenom, chromaDenom int
	hasWeights             bool
	lumaW, lumaO           [2][h265MaxRefs]int8
	chromaW, chromaO       [2][h265MaxRefs][2]int8
	maxMerge               int
	qpDelta, cbOff, crOff  int8
	dbfOff                 bool
	beta, tc               int8
	lfAcross               bool
	dataOffset             int
	data                   []byte
}

func (s *h265Slice) idr() bool  { return s.nal == h265NalIDRW || s.nal == h265NalIDRN }
func (s *h265Slice) irap() bool { return s.nal >= h265NalBLALP && s.nal <= 23 }
func (s *h265Slice) bla() bool  { return s.nal >= h265NalBLALP && s.nal <= h265NalBLANLP }
func (s *h265Slice) rasl() bool { return s.nal == h265NalRASLN || s.nal == h265NalRASLR }
func (s *h265Slice) radl() bool { return s.nal == h265NalRADLN || s.nal == h265NalRADLR }

func (s *h265Slice) stRPS() *h265RPS {
	if s.rpsIdx >= 0 {
		return &s.sps.rps[s.rpsIdx]
	}
	return &s.rps
}

func h265CeilLog2(n int) int { return bits.Len32(uint32(n - 1)) }

// parseH265Slice reads a slice segment header. prev is the preceding independent
// segment of the same picture, or nil.
func parseH265Slice(b []byte, s *h265Slice, prev *h265Slice, spss *[16]*h265SPS, ppss *[64]*h265PPS) error {
	nal, tid := (b[0]>>1)&63, b[1]&7-1
	r := vaBits{b: b}
	r.u(16)
	first := r.flag()
	noOutPrior := false
	if nal >= h265NalBLALP && nal <= 23 {
		noOutPrior = r.flag()
	}
	ppsID := r.ue()
	if r.bad || ppsID > 63 {
		return errH265Slice
	}
	pps := ppss[ppsID]
	if pps == nil || spss[pps.spsID] == nil {
		return errH265NoPPS
	}
	sps := spss[pps.spsID]
	dependent := false
	addr := uint32(0)
	if !first {
		if pps.dependent {
			dependent = r.flag()
		}
		addr = r.u(h265CeilLog2(sps.ctbs))
		if r.bad || int(addr) >= sps.ctbs || addr == 0 {
			return errH265Slice
		}
	}
	if dependent {
		if prev == nil || prev.pps != pps {
			return errH265Slice
		}
		*s = *prev
	} else {
		*s = h265Slice{rpsIdx: -1, colFromL0: true}
	}
	s.nal, s.tid, s.first, s.noOutPrior, s.dependent = nal, tid, first, noOutPrior, dependent
	s.pps, s.sps, s.addr, s.data = pps, sps, addr, b
	if !dependent {
		if err := r.h265SliceBody(s, sps, pps); err != nil {
			return err
		}
	}
	if pps.tiles || pps.entropySync {
		n := r.ue()
		limit := uint32(sps.ctbH - 1)
		if pps.tiles {
			limit = uint32(pps.tileCols*pps.tileRows - 1)
		}
		if r.bad || n > limit {
			return errH265Slice
		}
		if n > 0 {
			l := r.ue()
			if r.bad || l > 31 {
				return errH265Slice
			}
			for i := uint32(0); i < n; i++ {
				r.u(int(l) + 1)
			}
		}
	}
	if pps.headerExt {
		n := r.ue()
		if r.bad || n > 256 {
			return errH265Slice
		}
		for i := uint32(0); i < n; i++ {
			r.u(8)
		}
	}
	// byte_alignment()
	if r.u1() != 1 {
		return errH265Slice
	}
	for r.bit != 0 && !r.bad {
		r.u1()
	}
	if r.bad || r.i >= len(b) {
		return errH265Slice
	}
	s.dataOffset = r.i
	return nil
}

func (r *vaBits) h265SliceBody(s *h265Slice, sps *h265SPS, pps *h265PPS) error {
	for i := 0; i < pps.extraBits; i++ {
		r.u1()
	}
	t := r.ue()
	if r.bad || t > 2 {
		return errH265Slice
	}
	s.typ = uint8(t)
	if s.irap() && s.typ != h265SliceI {
		return errH265Slice
	}
	s.picOutput = true
	if pps.outputFlag {
		s.picOutput = r.flag()
	}
	if sps.separatePlanes {
		r.u(2)
	}
	if !s.idr() {
		s.pocLsb = int32(r.u(sps.log2MaxPocLsb))
		if !r.flag() { // short_term_ref_pic_set_sps_flag
			at := r.rbspBits()
			if !r.h265StRPS(sps.rps[:], sps.numRPS, sps.numRPS, &s.rps) {
				return errH265Slice
			}
			s.stBits = r.rbspBits() - at
		} else {
			if sps.numRPS == 0 {
				return errH265Slice
			}
			idx := 0
			if sps.numRPS > 1 {
				idx = int(r.u(h265CeilLog2(sps.numRPS)))
			}
			if idx >= sps.numRPS {
				return errH265Slice
			}
			s.rpsIdx = idx
		}
		if sps.longTerm {
			if err := r.h265LongTerm(s, sps); err != nil {
				return err
			}
		}
		if sps.temporalMvp {
			s.tmvp = r.flag()
		}
	}
	if r.bad {
		return errH265Slice
	}
	rps := s.stRPS()
	for i := 0; i < int(rps.numNeg); i++ {
		if rps.used0[i] {
			s.numCurr++
		}
	}
	for i := 0; i < int(rps.numPos); i++ {
		if rps.used1[i] {
			s.numCurr++
		}
	}
	for i := 0; i < s.numLt; i++ {
		if s.ltUsed[i] {
			s.numCurr++
		}
	}
	if int(rps.numNeg)+int(rps.numPos)+s.numLt > h265MaxRefs-1 || s.numCurr > h265MaxRefs-1 {
		return errH265Slice
	}
	if sps.sao {
		s.saoY = r.flag()
		if sps.chroma != 0 {
			s.saoC = r.flag()
		}
	}
	if s.typ != h265SliceI {
		if s.numCurr == 0 {
			return errH265Slice
		}
		if err := r.h265Inter(s, sps, pps); err != nil {
			return err
		}
	}
	qp := r.se()
	if r.bad || int(qp)+26+pps.initQP < 0 || int(qp)+26+pps.initQP > 51 {
		return errH265Slice
	}
	s.qpDelta = int8(qp)
	if pps.sliceChromaQP {
		cb, cr := r.se(), r.se()
		if r.bad || cb < -12 || cb > 12 || cr < -12 || cr > 12 ||
			int(cb)+pps.cbQP < -12 || int(cb)+pps.cbQP > 12 ||
			int(cr)+pps.crQP < -12 || int(cr)+pps.crQP > 12 {
			return errH265Slice
		}
		s.cbOff, s.crOff = int8(cb), int8(cr)
	}
	override := false
	if pps.dbfOverride {
		override = r.flag()
	}
	s.dbfOff, s.beta, s.tc = pps.dbfOff, int8(pps.beta), int8(pps.tc)
	if override {
		s.dbfOff = r.flag()
		if !s.dbfOff {
			b, t := r.se(), r.se()
			if r.bad || b < -6 || b > 6 || t < -6 || t > 6 {
				return errH265Slice
			}
			s.beta, s.tc = int8(b), int8(t)
		}
	}
	s.lfAcross = pps.lfSlices
	if pps.lfSlices && (s.saoY || s.saoC || !s.dbfOff) {
		s.lfAcross = r.flag()
	}
	if r.bad {
		return errH265Slice
	}
	return nil
}

func (r *vaBits) h265LongTerm(s *h265Slice, sps *h265SPS) error {
	numSps := uint32(0)
	if sps.numLtSps > 0 {
		numSps = r.ue()
	}
	numPics := r.ue()
	if r.bad || numSps > uint32(sps.numLtSps) || numSps > h265MaxRefs || numPics > h265MaxRefs || numSps+numPics > h265MaxRefs {
		return errH265Slice
	}
	s.numLt = int(numSps + numPics)
	cycle := int32(0)
	for i := 0; i < s.numLt; i++ {
		if i < int(numSps) {
			idx := 0
			if sps.numLtSps > 1 {
				idx = int(r.u(h265CeilLog2(sps.numLtSps)))
				if idx >= sps.numLtSps {
					return errH265Slice
				}
			}
			s.ltPoc[i], s.ltUsed[i] = sps.ltPocLsb[idx], sps.ltUsed[idx]
		} else {
			s.ltPoc[i], s.ltUsed[i] = int32(r.u(sps.log2MaxPocLsb)), r.flag()
		}
		s.ltMsb[i] = r.flag()
		if i == 0 || i == int(numSps) {
			cycle = 0
		}
		if s.ltMsb[i] {
			d := r.ue()
			if r.bad || d > 1<<20 {
				return errH265Slice
			}
			cycle += int32(d)
			s.ltCycle[i] = cycle
		}
	}
	if r.bad {
		return errH265Slice
	}
	return nil
}

func (r *vaBits) h265Inter(s *h265Slice, sps *h265SPS, pps *h265PPS) error {
	lists := 1
	if s.typ == h265SliceB {
		lists = 2
	}
	s.numRef[0] = pps.numRef[0]
	if lists == 2 {
		s.numRef[1] = pps.numRef[1]
	}
	if r.flag() { // num_ref_idx_active_override_flag
		for l := 0; l < lists; l++ {
			n := r.ue()
			if r.bad || n > 14 {
				return errH265Slice
			}
			s.numRef[l] = int(n) + 1
		}
	}
	if pps.listsMod && s.numCurr > 1 {
		nbits := h265CeilLog2(s.numCurr)
		for l := 0; l < lists; l++ {
			s.modFlag[l] = r.flag()
			if s.modFlag[l] {
				for i := 0; i < s.numRef[l]; i++ {
					v := r.u(nbits)
					if int(v) >= s.numCurr {
						return errH265Slice
					}
					s.entries[l][i] = uint8(v)
				}
			}
		}
	}
	if lists == 2 {
		s.mvdZero = r.flag()
	}
	if pps.cabacInit {
		s.cabacInit = r.flag()
	}
	if s.tmvp {
		if lists == 2 {
			s.colFromL0 = r.flag()
		}
		l := 1
		if s.colFromL0 {
			l = 0
		}
		if s.numRef[l] > 1 {
			idx := r.ue()
			if r.bad || int(idx) >= s.numRef[l] {
				return errH265Slice
			}
			s.colIdx = int(idx)
		}
	}
	if (pps.weighted && s.typ == h265SliceP) || (pps.weightedBi && s.typ == h265SliceB) {
		if err := r.h265PredWeights(s, sps, lists); err != nil {
			return err
		}
	}
	m := r.ue()
	if r.bad || m > 4 {
		return errH265Slice
	}
	s.maxMerge = 5 - int(m)
	return nil
}

// h265PredWeights reads pred_weight_table() (7.3.6.3).
func (r *vaBits) h265PredWeights(s *h265Slice, sps *h265SPS, lists int) error {
	s.hasWeights = true
	ld := r.ue()
	if r.bad || ld > 7 {
		return errH265Slice
	}
	s.lumaDenom = int(ld)
	chroma := sps.chroma != 0
	if chroma {
		d := r.se()
		cd := int32(ld) + d
		if r.bad || cd < 0 || cd > 7 {
			return errH265Slice
		}
		s.chromaDenom = int(cd)
	}
	var lf, cf [h265MaxRefs]bool
	for l := 0; l < lists; l++ {
		n := s.numRef[l]
		for i := 0; i < n; i++ {
			lf[i] = r.flag()
		}
		for i := 0; i < n; i++ {
			cf[i] = false
			if chroma {
				cf[i] = r.flag()
			}
		}
		for i := 0; i < n; i++ {
			if lf[i] {
				w, o := r.se(), r.se()
				if r.bad || w < -128 || w > 127 || o < -128 || o > 127 {
					return errH265Slice
				}
				s.lumaW[l][i], s.lumaO[l][i] = int8(w), int8(o)
			}
			if cf[i] {
				for c := 0; c < 2; c++ {
					w, o := r.se(), r.se()
					if r.bad || w < -128 || w > 127 || o < -512 || o > 511 {
						return errH265Slice
					}
					cw := w + 1<<s.chromaDenom
					off := 128 + o - (128*cw)>>s.chromaDenom
					s.chromaW[l][i][c], s.chromaO[l][i][c] = int8(w), int8(min(max(off, -128), 127))
				}
			}
		}
	}
	return nil
}
