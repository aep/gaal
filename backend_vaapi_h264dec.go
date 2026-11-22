//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"bytes"
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// H.264 decoding: reference management, picture order and output reordering
// are done here (clause 8.2), the hardware does the rest.

const vaMaxPics = 40

// vaPic is a decoded frame known to the DPB. A slot is free when it is neither
// a reference nor waiting for output.
type vaPic struct {
	surface             uint32
	top, bottom, poc    int32
	frameNum, wrap      int32
	longIdx             int32
	short, long, output bool
	nonExisting         bool
	pts                 time.Duration
}

func (p *vaPic) ref() bool  { return p.short || p.long }
func (p *vaPic) busy() bool { return p.short || p.long || p.output }

type vaH264Decoder struct {
	s        *vaSession
	info     BackendInfo
	displays []*vaDisplay

	sps    [32]*vaSPS
	pps    [256]*vaPPS
	tmpSPS vaSPS
	tmpPPS vaPPS
	seq    uint32

	// active is a copy: the table entry may be overwritten by a new SPS with
	// the same id while pictures of the old sequence still await output.
	active    vaSPS
	hasActive bool
	endpoint  vaEndpoint
	w, h      int
	codedW    int
	codedH    int

	pics    [vaMaxPics]vaPic
	cur     *vaPic
	slices  []vaSlice
	nslices int
	pts     time.Duration
	started bool
	lists   [2][33]*vaPic
	sorted  [33]*vaPic
	maxLong int32 // MaxLongTermFrameIdx + 1, 0 means none

	prevPocMsb, prevPocLsb     int32
	prevFrameNum, prevFrameOff int32
	prevRefFrameNum            int32

	iq       []byte
	iqSPS    uint32
	iqPPS    uint32
	iqValid  bool
	picParam []byte
	sliceBuf []byte
	scaling  vaLists

	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
	closed  bool

	lowDelay bool // output in decoding order, without waiting for reordering
}

func (d *vaH264Decoder) setLowDelay() { d.lowDelay = true }

func newVAH264Decoder(info BackendInfo) (VideoDecoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaH264Profiles, vaVLD)
	if err != nil {
		return nil, err
	}
	d := &vaH264Decoder{info: info, s: newVASession(a, ds[0]), displays: ds,
		iq: make([]byte, vaSizeDecIQ), picParam: make([]byte, vaSizeDecPic), sliceBuf: make([]byte, vaSizeDecSlice)}
	return d, nil
}

// vaStartCode is shared so that searching for it does not allocate.
var vaStartCode = []byte{0, 0, 1}

// vaNextNAL returns the first NAL unit in b and the input after it.
func vaNextNAL(b []byte) (nal, rest []byte) {
	i := bytes.Index(b, vaStartCode)
	if i < 0 {
		return nil, nil
	}
	b = b[i+3:]
	end := bytes.Index(b, vaStartCode)
	if end < 0 {
		nal, rest = b, nil
	} else {
		nal, rest = b[:end], b[end:]
	}
	for len(nal) > 0 && nal[len(nal)-1] == 0 {
		nal = nal[:len(nal)-1]
	}
	return nal, rest
}

func (d *vaH264Decoder) Send(p Packet) error {
	if d.closed {
		return ErrClosed
	}
	if d.flushed {
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	if len(p.Data) == 0 {
		return fmt.Errorf("%w: empty packet", ErrInvalid)
	}
	d.pts = p.PTS
	var first error
	seen := false
	for nal, rest := vaNextNAL(p.Data); rest != nil || nal != nil; nal, rest = vaNextNAL(rest) {
		seen = true
		if len(nal) == 0 {
			continue
		}
		if err := d.handleNAL(nal); err != nil && first == nil {
			first = err
		}
	}
	if err := d.finishPicture(); err != nil && first == nil {
		first = err
	}
	if !seen && first == nil {
		first = errH264Slice
	}
	return first
}

func (d *vaH264Decoder) handleNAL(nal []byte) error {
	switch t := nal[0] & 31; t {
	case 1, 5:
		return d.handleSlice(nal)
	case 2, 3, 4:
		return errH264Part
	case 7:
		if err := parseSPS(nal, &d.tmpSPS); err != nil {
			return err
		}
		id := d.tmpSPS.id
		if old := d.sps[id]; old != nil {
			d.tmpSPS.version = old.version
			if *old == d.tmpSPS {
				return nil
			}
		}
		// A changed set must not affect slices already collected.
		if err := d.finishPicture(); err != nil {
			return err
		}
		if d.sps[id] == nil {
			d.sps[id] = new(vaSPS)
		}
		d.seq++
		d.tmpSPS.version = d.seq
		*d.sps[id] = d.tmpSPS
	case 8:
		err := parsePPS(nal, &d.tmpPPS, func(id int) bool { return d.sps[id] != nil && d.sps[id].chroma == 3 })
		if err != nil {
			return err
		}
		id := d.tmpPPS.id
		if old := d.pps[id]; old != nil {
			d.tmpPPS.version = old.version
			if *old == d.tmpPPS {
				return nil
			}
		}
		if err := d.finishPicture(); err != nil {
			return err
		}
		if d.pps[id] == nil {
			d.pps[id] = new(vaPPS)
		}
		d.seq++
		d.tmpPPS.version = d.seq
		*d.pps[id] = d.tmpPPS
	case 10, 11: // end of sequence, end of stream
		return d.finishPicture()
	}
	return nil
}

func (d *vaH264Decoder) handleSlice(nal []byte) error {
	if d.nslices == len(d.slices) {
		d.slices = append(d.slices, vaSlice{})
	}
	s := &d.slices[d.nslices]
	if err := parseSlice(nal, s, &d.sps, &d.pps); err != nil {
		s.data = nil
		return err
	}
	if s.redundant != 0 {
		s.data = nil
		return nil
	}
	if d.nslices > 0 && !samePicture(&d.slices[0], s) {
		// Finishing reuses slot 0, so move the new slice out of the way.
		next := *s
		s.data = nil
		if err := d.finishPicture(); err != nil {
			d.slices[0] = next
			d.nslices = 1
			return err
		}
		d.slices[0] = next
		d.nslices = 1
		return nil
	}
	d.nslices++
	return nil
}

func (d *vaH264Decoder) dropSlices() {
	for i := 0; i < d.nslices; i++ {
		d.slices[i].data = nil
	}
	d.nslices = 0
}

// finishPicture decodes the collected slices as one frame.
func (d *vaH264Decoder) finishPicture() error {
	if d.nslices == 0 {
		return nil
	}
	defer d.dropSlices()
	first := &d.slices[0]
	sps, pps := first.sps, first.pps
	if !d.hasActive || sps.version != d.active.version {
		if err := d.activate(sps, first); err != nil {
			return err
		}
	}
	intra := true
	for i := 0; i < d.nslices; i++ {
		intra = intra && d.slices[i].typ == h264I
	}
	if !d.started && !intra {
		return errH264Ref // joined mid-stream: wait for a picture that stands alone
	}
	if first.idr() {
		// All earlier pictures precede an IDR in output order.
		if err := d.outputAll(); err != nil {
			return err
		}
		for i := range d.pics {
			d.pics[i].short, d.pics[i].long = false, false
		}
		d.maxLong = 0
		d.prevRefFrameNum = 0
	} else if d.gapDetected(first, sps) {
		if sps.gaps {
			d.fillGap(first, sps)
		}
	}
	d.updateWraps(first.frameNum, sps)
	if !intra && !d.anyRef() {
		return errH264Ref
	}
	cur, err := d.allocate()
	if err != nil {
		return err
	}
	*cur = vaPic{surface: cur.surface, frameNum: first.frameNum, wrap: first.frameNum, pts: d.pts}
	d.computePOC(first, sps, cur)
	d.cur = cur
	if err := d.submit(first, sps, pps, cur); err != nil {
		d.cur = nil
		return err
	}
	d.started = true
	err = d.store(first, sps, cur)
	d.cur = nil
	return err
}

// pickProfile chooses the render node and entry point for a sequence. Lower
// profiles are subsets of higher ones, so a device lacking the exact profile can
// still decode with a higher one.
func (d *vaH264Decoder) pickProfile(sps *vaSPS) (*vaDisplay, vaEndpoint, error) {
	var order []int32
	switch sps.profile {
	case 66:
		order = vaOrderCB
	case 77:
		order = vaOrderMain
	case 100, 110, 122, 244, 44:
		order = vaOrderHigh
	default:
		return nil, vaEndpoint{}, errH264Profile
	}
	for _, disp := range d.displays {
		if ep := vaPick(disp, order, vaVLD); ep.profile >= 0 {
			return disp, ep, nil
		}
	}
	return nil, vaEndpoint{}, errH264Profile
}

var (
	vaOrderCB   = []int32{vaProfileH264CB, vaProfileH264Main, vaProfileH264High}
	vaOrderMain = []int32{vaProfileH264Main, vaProfileH264High}
	vaOrderHigh = []int32{vaProfileH264High}
)

// activate starts a new coded video sequence.
func (d *vaH264Decoder) activate(sps *vaSPS, first *vaSlice) error {
	if d.hasActive && !first.idr() {
		// Picture size cannot change without an IDR. Same-layout updates that
		// merely rewrite the SPS continue with the existing surfaces.
		if sps.mbW == d.active.mbW && sps.mbH == d.active.mbH && sps.cropX == d.active.cropX &&
			sps.cropY == d.active.cropY && sps.width == d.active.width && sps.height == d.active.height &&
			d.surfaceCount(sps) <= len(d.s.surfaces) {
			d.active = *sps
			return nil
		}
		return errH264Slice
	}
	if sps.chroma != 1 || sps.depthY != 8 || sps.depthC != 8 || sps.bypass {
		return errH264Format
	}
	disp, ep, err := d.pickProfile(sps)
	if err != nil {
		return err
	}
	// Everything of the old sequence leaves with the old crop and size.
	if err := d.outputAll(); err != nil {
		return err
	}
	same := d.hasActive && disp == d.s.disp && ep == d.endpoint && sps.mbW == d.active.mbW && sps.mbH == d.active.mbH &&
		d.surfaceCount(sps) == len(d.s.surfaces) && sps.frameMbsOnly == d.active.frameMbsOnly
	if same {
		d.active = *sps
		d.w, d.h = sps.width, sps.height
		return nil
	}
	for i := range d.pics {
		d.pics[i] = vaPic{}
	}
	s := d.s
	s.destroyVideo()
	s.destroyConfig()
	s.disp, s.dpy = disp, disp.dpy
	d.hasActive = false
	if err := s.createConfig(ep, vaAttrRTFormat, vaRTYUV420); err != nil {
		return err
	}
	d.codedW, d.codedH = sps.mbW*16, sps.mbH*16
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, d.codedW, d.codedH, d.surfaceCount(sps)); err != nil {
		return err
	}
	var flags uint32
	if sps.frameMbsOnly {
		flags = vaProgressive
	}
	if err := s.createContext(d.codedW, d.codedH, flags); err != nil {
		s.destroyVideo()
		return err
	}
	d.endpoint, d.active, d.hasActive = ep, *sps, true
	d.w, d.h = sps.width, sps.height
	d.iqValid = false
	return nil
}

// surfaceCount covers references, pictures waiting for output and the picture
// being decoded, plus one spare.
func (d *vaH264Decoder) surfaceCount(sps *vaSPS) int {
	if sps.reorder >= 0 {
		return max(sps.numRef, 1) + sps.reorder + 2
	}
	return sps.dpbFrames + 2
}

func (d *vaH264Decoder) gapDetected(s *vaSlice, sps *vaSPS) bool {
	if s.frameNum == d.prevRefFrameNum {
		return false
	}
	return s.frameNum != (d.prevRefFrameNum+1)&(1<<sps.log2FrameNum-1)
}

// fillGap inserts the "non-existing" frames of 8.2.5.2. They occupy reference
// slots but share the surface of the latest real reference.
func (d *vaH264Decoder) fillGap(s *vaSlice, sps *vaSPS) {
	maxNum := int32(1) << sps.log2FrameNum
	mask := maxNum - 1
	missing := (s.frameNum - d.prevRefFrameNum - 1) & mask
	fn := (d.prevRefFrameNum + 1) & mask
	// Older ones would slide straight out of the window again.
	if keep := int32(max(sps.numRef, 1)); missing > keep {
		fn = (s.frameNum - keep) & mask
	}
	surface := uint32(vaInvalidID)
	if p := d.latestRef(); p != nil {
		surface = p.surface
	}
	for ; fn != s.frameNum; fn = (fn + 1) & mask {
		if d.prevFrameNum > fn {
			d.prevFrameOff += maxNum // the skipped frames wrapped frame_num
		}
		d.updateWraps(fn, sps)
		d.slidingWindow(sps)
		p := d.freeSlot()
		if p == nil {
			return
		}
		*p = vaPic{surface: surface, frameNum: fn, wrap: fn, short: true, nonExisting: true}
		d.prevRefFrameNum, d.prevFrameNum = fn, fn
	}
}

func (d *vaH264Decoder) latestRef() *vaPic {
	var best *vaPic
	for i := range d.pics {
		p := &d.pics[i]
		if p.short && !p.nonExisting && (best == nil || p.wrap > best.wrap) {
			best = p
		}
	}
	if best == nil {
		for i := range d.pics {
			if p := &d.pics[i]; p.long && !p.nonExisting {
				return p
			}
		}
	}
	return best
}

func (d *vaH264Decoder) anyRef() bool {
	for i := range d.pics {
		if d.pics[i].ref() && !d.pics[i].nonExisting {
			return true
		}
	}
	return false
}

func (d *vaH264Decoder) updateWraps(frameNum int32, sps *vaSPS) {
	maxNum := int32(1) << sps.log2FrameNum
	for i := range d.pics {
		p := &d.pics[i]
		if p.short {
			p.wrap = p.frameNum
			if p.frameNum > frameNum {
				p.wrap -= maxNum
			}
		}
	}
}

func (d *vaH264Decoder) freeSlot() *vaPic {
	for i := range d.pics {
		if !d.pics[i].busy() && &d.pics[i] != d.cur {
			return &d.pics[i]
		}
	}
	return nil
}

func (d *vaH264Decoder) surfaceFree(id uint32) bool {
	for i := range d.pics {
		if p := &d.pics[i]; p.busy() && p.surface == id {
			return false
		}
	}
	return true
}

var errVANoSurface = errors.New("vaapi: out of decode surfaces")

// allocate returns a free slot with a free surface, forcing output if a
// damaged stream holds more pictures than its headers promised.
func (d *vaH264Decoder) allocate() (*vaPic, error) {
	for {
		if p := d.freeSlot(); p != nil {
			for _, id := range d.s.surfaces {
				if d.surfaceFree(id) {
					p.surface = id
					return p, nil
				}
			}
		}
		ok, err := d.outputOne()
		if err != nil {
			return nil, err
		}
		if !ok {
			// Nothing left to output, so references are what fills the pool.
			if !d.dropOldestRef() {
				return nil, errVANoSurface
			}
		}
	}
}

func (d *vaH264Decoder) dropOldestRef() bool {
	var old *vaPic
	for i := range d.pics {
		p := &d.pics[i]
		if p.short && (old == nil || p.wrap < old.wrap) {
			old = p
		}
	}
	if old == nil {
		for i := range d.pics {
			if d.pics[i].long {
				old = &d.pics[i]
				break
			}
		}
	}
	if old == nil {
		return false
	}
	old.short, old.long = false, false
	return true
}

// computePOC implements 8.2.1 for frames.
func (d *vaH264Decoder) computePOC(s *vaSlice, sps *vaSPS, p *vaPic) {
	idr := s.idr()
	maxNum := int32(1) << sps.log2FrameNum
	switch sps.pocType {
	case 0:
		prevMsb, prevLsb := d.prevPocMsb, d.prevPocLsb
		if idr {
			prevMsb, prevLsb = 0, 0
		}
		maxLsb := int32(1) << sps.log2PocLsb
		msb := prevMsb
		if s.pocLsb < prevLsb && prevLsb-s.pocLsb >= maxLsb/2 {
			msb += maxLsb
		} else if s.pocLsb > prevLsb && s.pocLsb-prevLsb > maxLsb/2 {
			msb -= maxLsb
		}
		p.top = msb + s.pocLsb
		p.bottom = p.top + s.deltaBottom
		if s.refIdc != 0 {
			d.prevPocMsb, d.prevPocLsb = msb, s.pocLsb
		}
	default:
		offset := d.prevFrameOff
		if idr {
			offset = 0
		} else if d.prevFrameNum > s.frameNum {
			offset += maxNum
		}
		if sps.pocType == 1 {
			var abs int32
			if sps.cycle != 0 {
				abs = offset + s.frameNum
			}
			if s.refIdc == 0 && abs > 0 {
				abs--
			}
			var expected int32
			if abs > 0 {
				var delta int32
				for i := 0; i < sps.cycle; i++ {
					delta += sps.offsetRef[i]
				}
				cycles := (abs - 1) / int32(sps.cycle)
				inCycle := (abs - 1) % int32(sps.cycle)
				expected = cycles * delta
				for i := int32(0); i <= inCycle; i++ {
					expected += sps.offsetRef[i]
				}
			}
			if s.refIdc == 0 {
				expected += sps.offsetNonRef
			}
			p.top = expected + s.delta[0]
			p.bottom = p.top + sps.offsetTopBottom + s.delta[1]
		} else {
			v := 2 * (offset + s.frameNum)
			if idr {
				v = 0
			} else if s.refIdc == 0 {
				v--
			}
			p.top, p.bottom = v, v
		}
		d.prevFrameOff = offset
	}
	d.prevFrameNum = s.frameNum
	p.poc = min(p.top, p.bottom)
}

func vaPutPic(b []byte, off int, p *vaPic) {
	if p == nil {
		vaPutInvalid(b, off)
		return
	}
	var flags uint32
	idx := p.frameNum
	switch {
	case p.long:
		flags = vaPicLongRef
		idx = p.longIdx
	case p.short:
		flags = vaPicShortRef
	}
	if p.nonExisting {
		flags |= vaPicNonExisting
	}
	le32(b, off+vaoPID, p.surface)
	le32(b, off+vaoPFrameIdx, uint32(idx))
	le32(b, off+vaoPFlags, flags)
	le32(b, off+vaoPTop, uint32(p.top))
	le32(b, off+vaoPBottom, uint32(p.bottom))
}

func vaPutInvalid(b []byte, off int) {
	le32(b, off+vaoPID, vaInvalidID)
	le32(b, off+vaoPFrameIdx, 0)
	le32(b, off+vaoPFlags, vaPicInvalid)
	le32(b, off+vaoPTop, 0)
	le32(b, off+vaoPBottom, 0)
}

func vaBool(b bool, shift uint) uint32 {
	if b {
		return 1 << shift
	}
	return 0
}

func (d *vaH264Decoder) fillPicParam(s *vaSlice, sps *vaSPS, pps *vaPPS, cur *vaPic) {
	b := d.picParam
	clear(b)
	le32(b, vaoDPCurr+vaoPID, cur.surface)
	le32(b, vaoDPCurr+vaoPFrameIdx, uint32(cur.frameNum))
	le32(b, vaoDPCurr+vaoPTop, uint32(cur.top))
	le32(b, vaoDPCurr+vaoPBottom, uint32(cur.bottom))
	n := 0
	for i := range d.pics {
		if p := &d.pics[i]; p.ref() && n < 16 {
			vaPutPic(b, vaoDPRefs+n*vaSizePicH264, p)
			n++
		}
	}
	for ; n < 16; n++ {
		vaPutInvalid(b, vaoDPRefs+n*vaSizePicH264)
	}
	vaPut16(b, vaoDPWidth, uint16(sps.mbW-1))
	vaPut16(b, vaoDPHeight, uint16(sps.mbH-1))
	b[vaoDPNumRef] = uint8(sps.numRef)
	seq := uint32(sps.chroma)<<vaBitSChroma |
		vaBool(sps.gaps, vaBitSGaps) | vaBool(sps.frameMbsOnly, vaBitSFrameMbs) |
		vaBool(sps.mbaff, vaBitSMbaff) | vaBool(sps.direct8, vaBitSDirect8) |
		vaBool(sps.level >= 31, vaBitSMinBi) |
		uint32(sps.log2FrameNum-4)<<vaBitSFrameNum | uint32(sps.pocType)<<vaBitSPocType |
		vaBool(sps.deltaZero, vaBitSDeltaZero)
	if sps.pocType == 0 {
		seq |= uint32(sps.log2PocLsb-4) << vaBitSPocLsb
	}
	le32(b, vaoDPSeq, seq)
	b[vaoDPQP] = byte(int8(pps.qp))
	b[vaoDPQS] = byte(int8(pps.qs))
	b[vaoDPChromaQP] = byte(int8(pps.chromaQP))
	b[vaoDPChromaQP2] = byte(int8(pps.chromaQP2))
	le32(b, vaoDPPic, vaBool(pps.cabac, vaBitPEntropy)|vaBool(pps.weightedPred, vaBitPWeighted)|
		uint32(pps.weightedBipred)<<vaBitPBipred|vaBool(pps.t8x8, vaBitPT8x8)|
		vaBool(pps.constrainedIntra, vaBitPIntra)|vaBool(pps.bottomPOC, vaBitPPicOrder)|
		vaBool(pps.deblockCtl, vaBitPDeblock)|vaBool(pps.redundant, vaBitPRedundant)|
		vaBool(s.refIdc != 0, vaBitPRef))
	vaPut16(b, vaoDPFrameNum, uint16(cur.frameNum))
}

// fillIQ rebuilds the scaling matrices when the parameter sets changed.
func (d *vaH264Decoder) fillIQ(sps *vaSPS, pps *vaPPS) {
	if d.iqValid && d.iqSPS == sps.version && d.iqPPS == pps.version {
		return
	}
	d.iqValid, d.iqSPS, d.iqPPS = true, sps.version, pps.version
	vaResolveScaling(sps, pps, &d.scaling)
	for i := range d.scaling.l4 {
		for j, v := range d.scaling.l4[i] {
			d.iq[vaoIQ4+i*16+int(vaZigzag4[j])] = v
		}
	}
	for i := range d.scaling.l8 {
		for j, v := range d.scaling.l8[i] {
			d.iq[vaoIQ8+i*64+int(vaZigzag8[j])] = v
		}
	}
}

// sortRefs orders pics in place with less, without allocating.
func vaInsertSorted(list []*vaPic, n int, p *vaPic, before func(a, b *vaPic) bool) int {
	i := n
	for i > 0 && before(p, list[i-1]) {
		list[i] = list[i-1]
		i--
	}
	list[i] = p
	return n + 1
}

func vaByWrapDesc(a, b *vaPic) bool { return a.wrap > b.wrap }
func vaByPocDesc(a, b *vaPic) bool  { return a.poc > b.poc }
func vaByPocAsc(a, b *vaPic) bool   { return a.poc < b.poc }
func vaByLongAsc(a, b *vaPic) bool  { return a.longIdx < b.longIdx }

// initList appends to list the short-term frames selected by keep, sorted, then
// returns the new length. It uses d.sorted as scratch space.
func (d *vaH264Decoder) appendRefs(list []*vaPic, n int, long bool, poc int32, mode int) int {
	m := 0
	for i := range d.pics {
		p := &d.pics[i]
		if m == len(d.sorted)-1 {
			break
		}
		switch {
		case long:
			if p.long {
				m = vaInsertSorted(d.sorted[:], m, p, vaByLongAsc)
			}
		case !p.short:
		case mode == 0:
			m = vaInsertSorted(d.sorted[:], m, p, vaByWrapDesc)
		case p.nonExisting:
		case mode == 1 && p.poc < poc:
			m = vaInsertSorted(d.sorted[:], m, p, vaByPocDesc)
		case mode == 2 && p.poc > poc:
			m = vaInsertSorted(d.sorted[:], m, p, vaByPocAsc)
		}
	}
	for i := 0; i < m && n < len(list); i++ {
		list[n] = d.sorted[i]
		n++
	}
	return n
}

// buildLists creates the reference lists of a slice (8.2.4).
func (d *vaH264Decoder) buildLists(s *vaSlice, sps *vaSPS, cur *vaPic) {
	for l := range d.lists {
		clear(d.lists[l][:])
	}
	switch s.typ {
	case h264P:
		l0 := d.lists[0][:]
		n := d.appendRefs(l0, 0, false, 0, 0)
		d.appendRefs(l0, n, true, 0, 0)
	case h264B:
		l0, l1 := d.lists[0][:], d.lists[1][:]
		n := d.appendRefs(l0, 0, false, cur.poc, 1)
		n = d.appendRefs(l0, n, false, cur.poc, 2)
		n = d.appendRefs(l0, n, true, 0, 0)
		m := d.appendRefs(l1, 0, false, cur.poc, 2)
		m = d.appendRefs(l1, m, false, cur.poc, 1)
		m = d.appendRefs(l1, m, true, 0, 0)
		if m > 1 && m == n {
			same := true
			for i := 0; i < m; i++ {
				same = same && l0[i] == l1[i]
			}
			if same {
				l1[0], l1[1] = l1[1], l1[0]
			}
		}
	}
	for l := 0; l < 2; l++ {
		n := s.numRef[l]
		if n == 0 {
			continue
		}
		clear(d.lists[l][n:]) // the initial list is truncated to the active size
		d.modifyList(s, sps, l, n, cur)
		// Damaged streams can leave holes, which hardware must not follow.
		var fallback *vaPic
		for _, p := range d.lists[l][:n] {
			if p != nil {
				fallback = p
				break
			}
		}
		if fallback == nil {
			fallback = d.latestRef()
		}
		for i := 0; i < n; i++ {
			if d.lists[l][i] == nil {
				d.lists[l][i] = fallback
			}
		}
	}
}

func (d *vaH264Decoder) findShort(picNum int32) *vaPic {
	for i := range d.pics {
		if p := &d.pics[i]; p.short && p.wrap == picNum {
			return p
		}
	}
	return nil
}

func (d *vaH264Decoder) findLong(idx int32) *vaPic {
	for i := range d.pics {
		if p := &d.pics[i]; p.long && p.longIdx == idx {
			return p
		}
	}
	return nil
}

// modifyList applies ref_pic_list_modification (8.2.4.3).
func (d *vaH264Decoder) modifyList(s *vaSlice, sps *vaSPS, l, n int, cur *vaPic) {
	list := &d.lists[l]
	maxPicNum := int32(1) << sps.log2FrameNum
	pred := cur.frameNum
	idx := 0
	for _, m := range s.mods[l][:s.nmods[l]] {
		if idx >= n {
			return
		}
		var p *vaPic
		if m.op == 2 {
			p = d.findLong(int32(m.val))
		} else {
			abs := int32(m.val) + 1
			if m.op == 0 {
				pred -= abs
				if pred < 0 {
					pred += maxPicNum
				}
			} else {
				pred += abs
				if pred >= maxPicNum {
					pred -= maxPicNum
				}
			}
			num := pred
			if num > cur.frameNum {
				num -= maxPicNum
			}
			p = d.findShort(num)
		}
		if p == nil {
			continue
		}
		copy(list[idx+1:n+1], list[idx:n])
		list[idx] = p
		idx++
		w := idx
		for c := idx; c <= n; c++ {
			if list[c] != p {
				list[w] = list[c]
				w++
			}
		}
		for ; w <= n; w++ {
			list[w] = nil
		}
	}
}

func (d *vaH264Decoder) fillSlice(s *vaSlice, sps *vaSPS, cur *vaPic) {
	b := d.sliceBuf
	clear(b)
	le32(b, vaoDSSize, uint32(len(s.data)))
	le32(b, vaoDSFlag, vaSliceDataAll)
	vaPut16(b, vaoDSBitOffset, uint16(s.bitOffset))
	vaPut16(b, vaoDSFirstMB, uint16(s.firstMB))
	b[vaoDSType] = s.typ
	if s.direct {
		b[vaoDSDirect] = 1
	}
	if s.numRef[0] > 0 {
		b[vaoDSNumL0] = uint8(s.numRef[0] - 1)
	}
	if s.numRef[1] > 0 {
		b[vaoDSNumL1] = uint8(s.numRef[1] - 1)
	}
	b[vaoDSCabac] = s.cabacInit
	b[vaoDSQP] = byte(s.qpDelta)
	b[vaoDSDeblock] = s.deblock
	b[vaoDSAlpha] = byte(s.alpha)
	b[vaoDSBeta] = byte(s.beta)
	d.buildLists(s, sps, cur)
	offs := [2]int{vaoDSList0, vaoDSList1}
	for l, off := range offs {
		for i := 0; i < 32; i++ {
			var p *vaPic
			if i < s.numRef[l] {
				p = d.lists[l][i]
			}
			vaPutPic(b, off+i*vaSizePicH264, p)
		}
	}
	if !s.hasTable {
		return
	}
	b[vaoDSLumaDenom] = s.lumaDenom
	b[vaoDSChromaDenom] = s.chromaDenom
	d.fillWeights(s, 0, vaoDSLumaFlag0, vaoDSLumaW0, vaoDSLumaO0, vaoDSChromaFlag0, vaoDSChromaW0, vaoDSChromaO0)
	d.fillWeights(s, 1, vaoDSLumaFlag1, vaoDSLumaW1, vaoDSLumaO1, vaoDSChromaFlag1, vaoDSChromaW1, vaoDSChromaO1)
}

func (d *vaH264Decoder) fillWeights(s *vaSlice, l, lf, lw, lo, cf, cw, co int) {
	b := d.sliceBuf
	if s.lumaFlag[l] {
		b[lf] = 1
	}
	if s.chromaFlag[l] {
		b[cf] = 1
	}
	for i := 0; i < s.numRef[l]; i++ {
		vaPut16(b, lw+2*i, uint16(s.lumaW[l][i]))
		vaPut16(b, lo+2*i, uint16(s.lumaO[l][i]))
		for c := 0; c < 2; c++ {
			vaPut16(b, cw+4*i+2*c, uint16(s.chromaW[l][i][c]))
			vaPut16(b, co+4*i+2*c, uint16(s.chromaO[l][i][c]))
		}
	}
}

func (d *vaH264Decoder) submit(first *vaSlice, sps *vaSPS, pps *vaPPS, cur *vaPic) error {
	s := d.s
	d.fillPicParam(first, sps, pps, cur)
	d.fillIQ(sps, pps)
	if err := s.addBuffer(vaBufPicParam, unsafe.Pointer(&d.picParam[0]), vaSizeDecPic); err != nil {
		s.dropBuffers()
		return err
	}
	if err := s.addBuffer(vaBufIQ, unsafe.Pointer(&d.iq[0]), vaSizeDecIQ); err != nil {
		s.dropBuffers()
		return err
	}
	for i := 0; i < d.nslices; i++ {
		sl := &d.slices[i]
		d.fillSlice(sl, sps, cur)
		if err := s.addBuffer(vaBufSliceParam, unsafe.Pointer(&d.sliceBuf[0]), vaSizeDecSlice); err != nil {
			s.dropBuffers()
			return err
		}
		if err := s.addBuffer(vaBufSliceData, unsafe.Pointer(&sl.data[0]), len(sl.data)); err != nil {
			s.dropBuffers()
			return err
		}
	}
	return s.submit(cur.surface)
}

func (d *vaH264Decoder) slidingWindow(sps *vaSPS) {
	n := 0
	var oldest *vaPic
	for i := range d.pics {
		p := &d.pics[i]
		if p == d.cur {
			continue
		}
		if p.ref() {
			n++
		}
		if p.short && (oldest == nil || p.wrap < oldest.wrap) {
			oldest = p
		}
	}
	if n >= max(sps.numRef, 1) && oldest != nil {
		oldest.short = false
	}
}

// applyMMCO runs memory_management_control_operations (8.2.5.4).
func (d *vaH264Decoder) applyMMCO(s *vaSlice, cur *vaPic) {
	for _, m := range s.mmco[:s.nmmco] {
		switch m.op {
		case 1:
			if p := d.findShort(cur.frameNum - int32(m.a) - 1); p != nil {
				p.short = false
			}
		case 2:
			if p := d.findLong(int32(m.a)); p != nil {
				p.long = false
			}
		case 3:
			idx := int32(m.b)
			p := d.findShort(cur.frameNum - int32(m.a) - 1)
			if old := d.findLong(idx); old != nil && old != p {
				old.long = false
			}
			if p != nil {
				p.short, p.long, p.longIdx = false, true, idx
			}
		case 4:
			d.maxLong = int32(m.a)
			for i := range d.pics {
				if p := &d.pics[i]; p.long && p.longIdx >= d.maxLong {
					p.long = false
				}
			}
		case 5:
			for i := range d.pics {
				d.pics[i].short, d.pics[i].long = false, false
			}
			d.maxLong = 0
		case 6:
			idx := int32(m.a)
			if old := d.findLong(idx); old != nil {
				old.long = false
			}
			cur.long, cur.longIdx = true, idx
		}
	}
}

// store marks references (8.2.5), adds the picture to the DPB and outputs what
// display order now permits.
func (d *vaH264Decoder) store(s *vaSlice, sps *vaSPS, cur *vaPic) error {
	if s.mmco5 {
		// Everything already decoded is displayed first.
		if err := d.outputAll(); err != nil {
			return err
		}
	}
	if s.refIdc != 0 {
		switch {
		case s.idr():
			if s.longTermRef {
				cur.long, cur.longIdx = true, 0
				d.maxLong = 1
			} else {
				cur.short = true
			}
		case s.adaptive:
			d.applyMMCO(s, cur)
			if !cur.long {
				cur.short = true
			}
		default:
			d.slidingWindow(sps)
			cur.short = true
		}
		d.prevRefFrameNum = s.frameNum
	}
	if s.mmco5 {
		temp := min(cur.top, cur.bottom)
		cur.top -= temp
		cur.bottom -= temp
		cur.poc = 0
		cur.frameNum, cur.wrap = 0, 0
		d.prevRefFrameNum, d.prevFrameNum, d.prevFrameOff = 0, 0, 0
		d.prevPocMsb, d.prevPocLsb = 0, cur.top
	}
	if d.lowDelay {
		return d.emit(cur)
	}
	cur.output = true
	return d.bump(sps)
}

func (d *vaH264Decoder) bump(sps *vaSPS) error {
	for {
		waiting, held := 0, 0
		for i := range d.pics {
			p := &d.pics[i]
			if p.output {
				waiting++
			}
			if p.busy() {
				held++
			}
		}
		if waiting == 0 {
			return nil
		}
		if sps.reorder >= 0 {
			if waiting <= sps.reorder {
				return nil
			}
		} else if held <= sps.dpbFrames {
			// Without a stated limit, follow the C.4.5.3 bumping process: wait
			// until the DPB would overflow.
			return nil
		}
		if _, err := d.outputOne(); err != nil {
			return err
		}
	}
}

// outputOne emits the waiting picture with the lowest POC.
func (d *vaH264Decoder) outputOne() (bool, error) {
	var best *vaPic
	for i := range d.pics {
		p := &d.pics[i]
		if p.output && (best == nil || p.poc < best.poc) {
			best = p
		}
	}
	if best == nil {
		return false, nil
	}
	best.output = false
	return true, d.emit(best)
}

func (d *vaH264Decoder) outputAll() error {
	var first error
	for {
		ok, err := d.outputOne()
		if err != nil && first == nil {
			first = err
		}
		if !ok {
			return first
		}
	}
}

func (d *vaH264Decoder) emit(p *vaPic) error {
	s := d.s
	if err := s.sync(p.surface); err != nil {
		return err
	}
	f := d.frames.Get(d.w, d.h, NV12)
	f.PTS = p.pts
	if err := s.readSurface(p.surface, f, d.active.cropX, d.active.cropY, d.codedW, d.codedH); err != nil {
		f.Release()
		return err
	}
	d.out.push(f)
	return nil
}

func (d *vaH264Decoder) Receive() (*VideoFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *vaH264Decoder) Flush() error {
	if d.flushed {
		return nil
	}
	d.flushed = true
	err := d.finishPicture()
	if e := d.outputAll(); err == nil {
		err = e
	}
	return err
}

func (d *vaH264Decoder) Backend() BackendInfo { return d.info }

func (d *vaH264Decoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.s.close()
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}
