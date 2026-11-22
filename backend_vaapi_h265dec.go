//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"fmt"
	"time"
	"unsafe"
)

// H.265 decoding: picture order, reference picture sets, the DPB and output
// order are handled here (clauses 8.3 and C.5.2), the hardware does the rest.
//
// A decoder without a session (s == nil) runs every check and all reference
// handling but sends nothing to a device, which lets the parsers and the DPB
// logic be fuzzed without touching the GPU.

const vaH265MaxPics = 32

type vaH265Pic struct {
	surface             uint32
	poc                 int32
	short, long, output bool
	keep, toLong        bool
	latency             int
	vaIdx               int
	rpsFlags            uint32
	pts                 time.Duration
}

func (p *vaH265Pic) ref() bool  { return p.short || p.long }
func (p *vaH265Pic) busy() bool { return p.short || p.long || p.output }

type vaH265Decoder struct {
	s        *vaSession
	info     BackendInfo
	displays []*vaDisplay

	sps    [16]*h265SPS
	pps    [64]*h265PPS
	tmpSPS h265SPS
	tmpPPS h265PPS
	seq    uint32

	active    h265SPS // a copy, the table entry can change while pictures wait
	hasActive bool
	endpoint  vaEndpoint
	surfaces  []uint32
	layout    h265Layout
	boundSPS  uint32
	boundPPS  uint32

	pics        [vaH265MaxPics]vaH265Pic
	slices      []h265Slice
	nslices     int
	prevIndep   int // index of the last independent segment, -1 if none
	pts         time.Duration
	started     bool
	seqStart    bool // the next picture starts a coded video sequence
	assocNoRasl bool // the last IRAP had NoRaslOutputFlag set
	prevMsb     int32
	prevLsb     int32

	before, after, lt    [h265MaxRefs]*vaH265Pic
	nBefore, nAfter, nLt int

	iq       []byte
	iqSPS    uint32
	iqPPS    uint32
	iqValid  bool
	picParam []byte
	sliceBuf []byte

	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
	closed  bool

	lowDelay bool // output in decoding order, without waiting for reordering

	dryEmit func(poc int32, pts time.Duration) // test hook for s == nil
}

func (d *vaH265Decoder) setLowDelay() { d.lowDelay = true }

func newVAH265Decoder(info BackendInfo) (VideoDecoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaH265Profiles, vaVLD)
	if err != nil {
		return nil, err
	}
	d := newH265Decoder(info)
	d.s, d.displays = newVASession(a, ds[0]), ds
	return d, nil
}

func newH265Decoder(info BackendInfo) *vaH265Decoder {
	return &vaH265Decoder{info: info, seqStart: true, prevIndep: -1,
		iq: make([]byte, vaSizeHIQ), picParam: make([]byte, vaSizeHPic), sliceBuf: make([]byte, vaSizeHSlice)}
}

func (d *vaH265Decoder) Send(p Packet) error {
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
		first = errH265Slice
	}
	return first
}

func (d *vaH265Decoder) handleNAL(nal []byte) error {
	if len(nal) < 2 || nal[0]&0x80 != 0 || nal[1]&7 == 0 {
		return errH265Slice
	}
	if nal[0]&1 != 0 || nal[1]>>3 != 0 {
		return nil // nuh_layer_id > 0 is outside version 1
	}
	t := nal[0] >> 1 & 63
	switch {
	case t <= h265NalRASLR || (t >= h265NalBLALP && t <= h265NalCRA):
		return d.handleSlice(nal)
	case t == h265NalSPS:
		if err := parseH265SPS(nal, &d.tmpSPS); err != nil {
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
			d.sps[id] = new(h265SPS)
		}
		d.seq++
		d.tmpSPS.version = d.seq
		*d.sps[id] = d.tmpSPS
	case t == h265NalPPS:
		if err := parseH265PPS(nal, &d.tmpPPS); err != nil {
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
			d.pps[id] = new(h265PPS)
		}
		d.seq++
		d.tmpPPS.version = d.seq
		*d.pps[id] = d.tmpPPS
	case t == h265NalEOS || t == h265NalEOB:
		err := d.finishPicture()
		if e := d.outputAll(); err == nil {
			err = e
		}
		d.seqStart = true
		return err
	}
	return nil
}

func (d *vaH265Decoder) handleSlice(nal []byte) error {
	if len(nal) < 3 {
		return errH265Slice
	}
	var err error
	if nal[2]&0x80 != 0 { // first_slice_segment_in_pic_flag
		err = d.finishPicture()
	}
	if d.nslices == h265MaxSlices {
		return errH265Slice
	}
	if d.nslices == len(d.slices) {
		d.slices = append(d.slices, h265Slice{})
	}
	s := &d.slices[d.nslices]
	var prev *h265Slice
	if d.prevIndep >= 0 {
		prev = &d.slices[d.prevIndep]
	}
	if e := parseH265Slice(nal, s, prev, &d.sps, &d.pps); e != nil {
		s.data = nil
		if err == nil {
			err = e
		}
		return err
	}
	if s.rasl() && d.assocNoRasl {
		s.data = nil // RASL pictures of a CRA that starts a sequence are not decodable
		return err
	}
	if !s.dependent {
		d.prevIndep = d.nslices
	}
	d.nslices++
	return err
}

func (d *vaH265Decoder) dropSlices() {
	for i := 0; i < d.nslices; i++ {
		d.slices[i].data = nil
	}
	d.nslices = 0
	d.prevIndep = -1
}

// checkSlices verifies that the collected segments can form one picture.
func (d *vaH265Decoder) checkSlices(first *h265Slice) error {
	if !first.first {
		return errH265Slice
	}
	last := first.addr
	for i := 1; i < d.nslices; i++ {
		s := &d.slices[i]
		if s.first || s.nal != first.nal || s.pps != first.pps || s.pocLsb != first.pocLsb ||
			s.numCurr != first.numCurr {
			return errH265Slice
		}
		if !first.pps.tiles && s.addr <= last {
			return errH265Slice
		}
		last = s.addr
	}
	return nil
}

// finishPicture decodes the collected segments as one picture.
func (d *vaH265Decoder) finishPicture() error {
	if d.nslices == 0 {
		return nil
	}
	defer d.dropSlices()
	first := &d.slices[0]
	sps, pps := first.sps, first.pps
	if err := d.checkSlices(first); err != nil {
		return err
	}
	if d.boundSPS != sps.version || d.boundPPS != pps.version {
		if err := h265Bind(sps, pps, &d.layout); err != nil {
			d.boundSPS, d.boundPPS = 0, 0
			return err
		}
		d.boundSPS, d.boundPPS = sps.version, pps.version
	}
	irap := first.irap()
	noRasl := irap && (first.idr() || first.bla() || d.seqStart)
	if !d.started && !irap {
		return errH265Ref // joined mid-stream: wait for a picture that stands alone
	}
	if noRasl {
		if err := d.startSequence(first); err != nil {
			return err
		}
	}
	if !d.hasActive || sps.version != d.active.version {
		if err := d.activate(sps, noRasl); err != nil {
			return err
		}
	}
	if irap {
		d.assocNoRasl = noRasl
	}
	poc := d.computePOC(first, sps, noRasl)
	missing := d.deriveRPS(first, sps, poc)
	if missing && !irap {
		return errH265Ref
	}
	if n := d.nBefore + d.nAfter + d.nLt; n != first.numCurr && !first.idr() {
		return errH265Ref
	}
	if !noRasl {
		if err := d.bump(sps, true); err != nil {
			return err
		}
	}
	cur, err := d.allocate()
	if err != nil {
		return err
	}
	*cur = vaH265Pic{surface: cur.surface, poc: poc, pts: d.pts}
	if err := d.submit(first, sps, pps, cur); err != nil {
		return err
	}
	d.started, d.seqStart = true, false
	cur.short = true
	if first.picOutput && d.lowDelay {
		return d.emit(cur)
	}
	if first.picOutput {
		for i := range d.pics {
			if p := &d.pics[i]; p.output && p.poc > poc {
				p.latency++
			}
		}
		cur.output = true
	}
	return d.bump(sps, false)
}

// startSequence handles an IRAP picture with NoRaslOutputFlag (C.5.2.2).
func (d *vaH265Decoder) startSequence(s *h265Slice) error {
	var err error
	if s.nal == h265NalCRA || s.noOutPrior {
		for i := range d.pics {
			d.pics[i].output = false
		}
	} else {
		err = d.outputAll()
	}
	for i := range d.pics {
		d.pics[i].short, d.pics[i].long = false, false
	}
	return err
}

func h265SameLayout(a, b *h265SPS) bool {
	return a.main == b.main && a.width == b.width && a.height == b.height && a.chroma == b.chroma &&
		a.depthY == b.depthY && a.depthC == b.depthC && a.cropX == b.cropX && a.cropY == b.cropY &&
		a.outW == b.outW && a.outH == b.outH && a.log2Ctb == b.log2Ctb && a.maxDecMinus1 <= b.maxDecMinus1
}

func h265Supported(s *h265SPS) error {
	if !s.main {
		return errH265Profile
	}
	if s.chroma != 1 || s.separatePlanes || s.depthY != 8 || s.depthC != 8 {
		return errH265Format
	}
	if s.width < h265MinDim || s.height < h265MinDim {
		return errH265Size
	}
	return nil
}

// activate starts using an SPS. A new resolution is only possible at the start
// of a coded video sequence.
func (d *vaH265Decoder) activate(sps *h265SPS, noRasl bool) error {
	if d.hasActive && h265SameLayout(sps, &d.active) {
		d.active = *sps
		return nil
	}
	if !noRasl {
		return errH265Slice
	}
	if err := h265Supported(sps); err != nil {
		return err
	}
	if err := d.outputAll(); err != nil {
		return err
	}
	count := sps.maxDecMinus1 + 3
	if d.s == nil {
		for i := range d.pics {
			d.pics[i] = vaH265Pic{}
		}
		d.surfaces = d.surfaces[:0]
		for i := 0; i < count; i++ {
			d.surfaces = append(d.surfaces, uint32(i+1))
		}
		d.active, d.hasActive = *sps, true
		return nil
	}
	s := d.s
	var disp *vaDisplay
	var ep vaEndpoint
	for _, c := range d.displays {
		if e := vaPick(c, vaH265Profiles, vaVLD); e.profile >= 0 {
			disp, ep = c, e
			break
		}
	}
	if disp == nil {
		return errH265Profile
	}
	if maxW, err := s.attribute(ep, vaAttrMaxPicWidth); err == nil && maxW != vaAttribNotSupported && sps.width > int(maxW) {
		return errH265Size
	}
	if maxH, err := s.attribute(ep, vaAttrMaxPicHeight); err == nil && maxH != vaAttribNotSupported && sps.height > int(maxH) {
		return errH265Size
	}
	for i := range d.pics {
		d.pics[i] = vaH265Pic{}
	}
	s.destroyVideo()
	s.destroyConfig()
	s.disp, s.dpy = disp, disp.dpy
	d.hasActive = false
	if err := s.createConfig(ep, vaAttrRTFormat, vaRTYUV420); err != nil {
		return err
	}
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, sps.width, sps.height, count); err != nil {
		return err
	}
	if err := s.createContext(sps.width, sps.height, vaProgressive); err != nil {
		s.destroyVideo()
		return err
	}
	d.surfaces = s.surfaces
	d.endpoint, d.active, d.hasActive = ep, *sps, true
	d.iqValid = false
	return nil
}

// computePOC implements 8.3.1.
func (d *vaH265Decoder) computePOC(s *h265Slice, sps *h265SPS, noRasl bool) int32 {
	maxLsb := int32(1) << sps.log2MaxPocLsb
	msb := int32(0)
	if !noRasl {
		switch {
		case s.pocLsb < d.prevLsb && d.prevLsb-s.pocLsb >= maxLsb/2:
			msb = d.prevMsb + maxLsb
		case s.pocLsb > d.prevLsb && s.pocLsb-d.prevLsb > maxLsb/2:
			msb = d.prevMsb - maxLsb
		default:
			msb = d.prevMsb
		}
	}
	subLayerNonRef := s.nal < 16 && s.nal&1 == 0
	if s.tid == 0 && !s.rasl() && !s.radl() && !subLayerNonRef {
		d.prevMsb, d.prevLsb = msb, s.pocLsb
	}
	return msb + s.pocLsb
}

func (d *vaH265Decoder) findRef(poc, mask int32, shortOnly bool) *vaH265Pic {
	for i := range d.pics {
		p := &d.pics[i]
		if !p.ref() || (shortOnly && (!p.short || p.toLong)) {
			continue
		}
		if p.poc&mask == poc&mask {
			return p
		}
	}
	return nil
}

// deriveRPS implements 8.3.2 and reports whether a picture the current one may
// reference is absent.
func (d *vaH265Decoder) deriveRPS(s *h265Slice, sps *h265SPS, poc int32) (missing bool) {
	d.nBefore, d.nAfter, d.nLt = 0, 0, 0
	for i := range d.pics {
		p := &d.pics[i]
		p.keep, p.toLong, p.rpsFlags, p.vaIdx = false, false, 0, -1
	}
	if s.idr() {
		for i := range d.pics {
			d.pics[i].short, d.pics[i].long = false, false
		}
		return false
	}
	maxLsb := int32(1) << sps.log2MaxPocLsb
	for i := 0; i < s.numLt; i++ {
		v, mask := s.ltPoc[i], maxLsb-1
		if s.ltMsb[i] {
			v += poc - s.ltCycle[i]*maxLsb - poc&(maxLsb-1)
			mask = -1
		}
		p := d.findRef(v, mask, false)
		if p != nil {
			p.keep, p.toLong = true, true
		}
		if s.ltUsed[i] {
			if p == nil {
				missing = true
				continue
			}
			p.rpsFlags |= vaPicHEVCLtCurr
			d.lt[d.nLt] = p
			d.nLt++
		}
	}
	rps := s.stRPS()
	for i := 0; i < int(rps.numNeg); i++ {
		p := d.findRef(poc+rps.s0[i], -1, true)
		if p != nil {
			p.keep = true
		}
		if rps.used0[i] {
			if p == nil {
				missing = true
				continue
			}
			p.rpsFlags |= vaPicHEVCBefore
			d.before[d.nBefore] = p
			d.nBefore++
		}
	}
	for i := 0; i < int(rps.numPos); i++ {
		p := d.findRef(poc+rps.s1[i], -1, true)
		if p != nil {
			p.keep = true
		}
		if rps.used1[i] {
			if p == nil {
				missing = true
				continue
			}
			p.rpsFlags |= vaPicHEVCAfter
			d.after[d.nAfter] = p
			d.nAfter++
		}
	}
	for i := range d.pics {
		p := &d.pics[i]
		switch {
		case !p.keep:
			p.short, p.long = false, false
		case p.toLong:
			p.short, p.long = false, true
		}
	}
	return missing
}

func (d *vaH265Decoder) surfaceFree(id uint32) bool {
	for i := range d.pics {
		if p := &d.pics[i]; p.busy() && p.surface == id {
			return false
		}
	}
	return true
}

// allocate returns a free slot with a free surface, forcing output if a damaged
// stream holds more pictures than its headers promised.
func (d *vaH265Decoder) allocate() (*vaH265Pic, error) {
	for {
		var slot *vaH265Pic
		for i := range d.pics {
			if !d.pics[i].busy() {
				slot = &d.pics[i]
				break
			}
		}
		if slot != nil {
			for _, id := range d.surfaces {
				if d.surfaceFree(id) {
					slot.surface = id
					return slot, nil
				}
			}
		}
		ok, err := d.outputOne()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errVANoSurface
		}
	}
}

func vaPutHEVCPic(b []byte, off int, p *vaH265Pic, flags uint32) {
	le32(b, off+vaoHID, p.surface)
	le32(b, off+vaoHPoc, uint32(p.poc))
	le32(b, off+vaoHFlags, flags)
}

func vaPutHEVCInvalid(b []byte, off int) {
	le32(b, off+vaoHID, vaInvalidID)
	le32(b, off+vaoHPoc, 0)
	le32(b, off+vaoHFlags, vaPicHEVCInvalid)
}

func (d *vaH265Decoder) fillPicParam(s *h265Slice, sps *h265SPS, pps *h265PPS, cur *vaH265Pic) {
	b := d.picParam
	clear(b)
	vaPutHEVCPic(b, vaoHPCurr, cur, 0)
	n := 0
	for i := range d.pics {
		p := &d.pics[i]
		if p == cur || !p.ref() || n == 15 {
			continue
		}
		flags := p.rpsFlags
		if p.long {
			flags |= vaPicHEVCLong
		}
		vaPutHEVCPic(b, vaoHPRefs+n*vaSizePicHEVC, p, flags)
		p.vaIdx = n
		n++
	}
	for ; n < 15; n++ {
		vaPutHEVCInvalid(b, vaoHPRefs+n*vaSizePicHEVC)
	}
	vaPut16(b, vaoHPWidth, uint16(sps.width))
	vaPut16(b, vaoHPHeight, uint16(sps.height))
	le32(b, vaoHPFields, uint32(sps.chroma)<<vaBitHPChroma|
		vaBool(sps.pcm, vaBitHPPcm)|vaBool(sps.scalingEnabled, vaBitHPScaling)|
		vaBool(pps.transformSkip, vaBitHPTransformSkip)|vaBool(sps.amp, vaBitHPAmp)|
		vaBool(sps.strongIntra, vaBitHPStrong)|vaBool(pps.signHide, vaBitHPSignHide)|
		vaBool(pps.constrainedIntra, vaBitHPConstrained)|vaBool(pps.cuQPDelta, vaBitHPCuQpDelta)|
		vaBool(pps.weighted, vaBitHPWeighted)|vaBool(pps.weightedBi, vaBitHPWeightedBi)|
		vaBool(pps.bypass, vaBitHPBypass)|vaBool(pps.tiles, vaBitHPTiles)|
		vaBool(pps.entropySync, vaBitHPEntropySync)|vaBool(pps.lfSlices, vaBitHPLfSlices)|
		vaBool(pps.lfTiles, vaBitHPLfTiles)|vaBool(sps.pcmLfOff, vaBitHPPcmLf)|
		vaBool(sps.reorder == 0, vaBitHPNoReorder))
	b[vaoHPDpbMinus1] = uint8(sps.maxDecMinus1)
	b[vaoHPDepthY] = uint8(sps.depthY - 8)
	b[vaoHPDepthC] = uint8(sps.depthC - 8)
	if sps.pcm {
		b[vaoHPPcmY] = uint8(sps.pcmDepthY - 1)
		b[vaoHPPcmC] = uint8(sps.pcmDepthC - 1)
		b[vaoHPMinPcm] = uint8(sps.log2MinPcm - 3)
		b[vaoHPDiffPcm] = uint8(sps.log2MaxPcm - sps.log2MinPcm)
	}
	b[vaoHPMinCb] = uint8(sps.log2MinCb - 3)
	b[vaoHPDiffCb] = uint8(sps.log2Ctb - sps.log2MinCb)
	b[vaoHPMinTb] = uint8(sps.log2MinTb - 2)
	b[vaoHPDiffTb] = uint8(sps.log2MaxTb - sps.log2MinTb)
	b[vaoHPDepthIntra] = uint8(sps.depthIntra)
	b[vaoHPDepthInter] = uint8(sps.depthInter)
	b[vaoHPInitQP] = byte(int8(pps.initQP))
	b[vaoHPDiffCuQp] = uint8(pps.diffCuQP)
	b[vaoHPCbQp] = byte(int8(pps.cbQP))
	b[vaoHPCrQp] = byte(int8(pps.crQP))
	b[vaoHPMergeLevel] = uint8(pps.parMerge)
	b[vaoHPTileCols] = uint8(pps.tileCols - 1)
	b[vaoHPTileRows] = uint8(pps.tileRows - 1)
	if pps.tiles {
		for i := 0; i < pps.tileCols-1; i++ {
			vaPut16(b, vaoHPColW+2*i, d.layout.colW[i]-1)
		}
		for i := 0; i < pps.tileRows-1; i++ {
			vaPut16(b, vaoHPRowH+2*i, d.layout.rowH[i]-1)
		}
	}
	irap := s.irap()
	le32(b, vaoHPSliceFields, vaBool(pps.listsMod, vaBitHSListsMod)|vaBool(sps.longTerm, vaBitHSLongTerm)|
		vaBool(sps.temporalMvp, vaBitHSTemporalMvp)|vaBool(pps.cabacInit, vaBitHSCabacInit)|
		vaBool(pps.outputFlag, vaBitHSOutputFlag)|vaBool(pps.dependent, vaBitHSDependent)|
		vaBool(pps.sliceChromaQP, vaBitHSChromaQp)|vaBool(sps.sao, vaBitHSSao)|
		vaBool(pps.dbfOverride, vaBitHSDbfOverride)|vaBool(pps.dbfOff, vaBitHSDbfDisabled)|
		vaBool(pps.headerExt, vaBitHSHeaderExt)|vaBool(irap, vaBitHSRap)|
		vaBool(s.idr(), vaBitHSIdr)|vaBool(irap, vaBitHSIntra))
	b[vaoHPPocLsb] = uint8(sps.log2MaxPocLsb - 4)
	b[vaoHPNumStRps] = uint8(sps.numRPS)
	b[vaoHPNumLtSps] = uint8(sps.numLtSps)
	b[vaoHPNumL0] = uint8(pps.numRef[0] - 1)
	b[vaoHPNumL1] = uint8(pps.numRef[1] - 1)
	b[vaoHPBeta] = byte(int8(pps.beta))
	b[vaoHPTc] = byte(int8(pps.tc))
	b[vaoHPExtraBits] = uint8(pps.extraBits)
	if s.rpsIdx < 0 {
		le32(b, vaoHPStRpsBits, uint32(s.stBits))
	}
}

// fillIQ rebuilds the scaling factors when the parameter sets changed.
func (d *vaH265Decoder) fillIQ(sps *h265SPS, pps *h265PPS) {
	if d.iqValid && d.iqSPS == sps.version && d.iqPPS == pps.version {
		return
	}
	d.iqValid, d.iqSPS, d.iqPPS = true, sps.version, pps.version
	var def h265Scaling
	sl := &def
	switch {
	case pps.scalingPresent:
		sl = &pps.scaling
	case sps.scalingPresent:
		sl = &sps.scaling
	default:
		def.setAllDefault()
	}
	b := d.iq
	for i := 0; i < 6; i++ {
		copy(b[vaoHIQ4+i*16:], sl.sl[0][i][:16])
		copy(b[vaoHIQ8+i*64:], sl.sl[1][i][:])
		copy(b[vaoHIQ16+i*64:], sl.sl[2][i][:])
		b[vaoHIQDC16+i] = sl.dc[0][i]
	}
	for i := 0; i < 2; i++ {
		copy(b[vaoHIQ32+i*64:], sl.sl[3][i*3][:])
		b[vaoHIQDC32+i] = sl.dc[1][i*3]
	}
}

// fillList writes one reference list of a slice (8.3.4).
func (d *vaH265Decoder) fillList(b []byte, s *h265Slice, l int) {
	n := max(s.numRef[l], s.numCurr)
	var temp [2 * h265MaxRefs]*vaH265Pic
	a, na, c, nc := d.before[:], d.nBefore, d.after[:], d.nAfter
	if l == 1 {
		a, na, c, nc = d.after[:], d.nAfter, d.before[:], d.nBefore
	}
	r := 0
	for r < n {
		for i := 0; i < na && r < n; i++ {
			temp[r] = a[i]
			r++
		}
		for i := 0; i < nc && r < n; i++ {
			temp[r] = c[i]
			r++
		}
		for i := 0; i < d.nLt && r < n; i++ {
			temp[r] = d.lt[i]
			r++
		}
	}
	list := b[vaoHLRefs+l*15:]
	for i := 0; i < s.numRef[l]; i++ {
		e := i
		if s.modFlag[l] {
			e = int(s.entries[l][i])
		}
		list[i] = uint8(temp[e].vaIdx)
	}
}

func (d *vaH265Decoder) fillSlice(s *h265Slice, last bool) {
	b := d.sliceBuf
	clear(b)
	le32(b, vaoHLSize, uint32(len(s.data)))
	le32(b, vaoHLFlag, vaSliceDataAll)
	le32(b, vaoHLByteOffset, uint32(s.dataOffset))
	le32(b, vaoHLAddr, s.addr)
	for i := 0; i < 2*15; i++ {
		b[vaoHLRefs+i] = 0xff
	}
	le32(b, vaoHLFlags, vaBool(last, vaBitHLLast)|vaBool(s.dependent, vaBitHLDependent)|
		uint32(s.typ)<<vaBitHLType|vaBool(s.saoY, vaBitHLSaoY)|vaBool(s.saoC, vaBitHLSaoC)|
		vaBool(s.mvdZero, vaBitHLMvdZero)|vaBool(s.cabacInit, vaBitHLCabacInit)|
		vaBool(s.tmvp, vaBitHLTemporal)|vaBool(s.dbfOff, vaBitHLDbfOff)|
		vaBool(s.colFromL0, vaBitHLColL0)|vaBool(s.lfAcross, vaBitHLLfAcross))
	b[vaoHLColIdx] = 0xff
	if s.tmvp {
		b[vaoHLColIdx] = uint8(s.colIdx)
	}
	b[vaoHLQP] = byte(s.qpDelta)
	b[vaoHLCb] = byte(s.cbOff)
	b[vaoHLCr] = byte(s.crOff)
	b[vaoHLBeta] = byte(s.beta)
	b[vaoHLTc] = byte(s.tc)
	if s.typ == h265SliceI {
		return
	}
	b[vaoHLNumL0] = uint8(s.numRef[0] - 1)
	d.fillList(b, s, 0)
	if s.typ == h265SliceB {
		b[vaoHLNumL1] = uint8(s.numRef[1] - 1)
		d.fillList(b, s, 1)
	}
	b[vaoHLMerge] = uint8(5 - s.maxMerge)
	if s.hasWeights {
		b[vaoHLLumaDenom] = uint8(s.lumaDenom)
		b[vaoHLChromaDenom] = byte(int8(s.chromaDenom - s.lumaDenom))
		putWeights := func(l, lw, lo, cw, co int) {
			for i := 0; i < s.numRef[l]; i++ {
				b[lw+i] = byte(s.lumaW[l][i])
				b[lo+i] = byte(s.lumaO[l][i])
				for c := 0; c < 2; c++ {
					b[cw+2*i+c] = byte(s.chromaW[l][i][c])
					b[co+2*i+c] = byte(s.chromaO[l][i][c])
				}
			}
		}
		putWeights(0, vaoHLLumaW0, vaoHLLumaO0, vaoHLChromaW0, vaoHLChromaO0)
		putWeights(1, vaoHLLumaW1, vaoHLLumaO1, vaoHLChromaW1, vaoHLChromaO1)
	}
}

func (d *vaH265Decoder) submit(first *h265Slice, sps *h265SPS, pps *h265PPS, cur *vaH265Pic) error {
	d.fillPicParam(first, sps, pps, cur)
	if sps.scalingEnabled {
		d.fillIQ(sps, pps)
	}
	s := d.s
	if s == nil {
		// Without a device the slice parameters are still built, which runs the
		// reference list construction.
		for i := 0; i < d.nslices; i++ {
			d.fillSlice(&d.slices[i], i == d.nslices-1)
		}
		return nil
	}
	if err := s.addBuffer(vaBufPicParam, unsafe.Pointer(&d.picParam[0]), vaSizeHPic); err != nil {
		s.dropBuffers()
		return err
	}
	if sps.scalingEnabled {
		if err := s.addBuffer(vaBufIQ, unsafe.Pointer(&d.iq[0]), vaSizeHIQ); err != nil {
			s.dropBuffers()
			return err
		}
	}
	for i := 0; i < d.nslices; i++ {
		sl := &d.slices[i]
		d.fillSlice(sl, i == d.nslices-1)
		if err := s.addBuffer(vaBufSliceParam, unsafe.Pointer(&d.sliceBuf[0]), vaSizeHSlice); err != nil {
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

// bump runs the output process of C.5.2. Before decoding (pre), DPB fullness
// also counts: room is needed for the current picture.
func (d *vaH265Decoder) bump(sps *h265SPS, pre bool) error {
	for {
		waiting, held := 0, 0
		late := false
		for i := range d.pics {
			p := &d.pics[i]
			if p.output {
				waiting++
				if sps.latencyInc != 0 && p.latency >= sps.reorder+sps.latencyInc-1 {
					late = true
				}
			}
			if p.busy() {
				held++
			}
		}
		if waiting == 0 {
			return nil
		}
		if waiting <= sps.reorder && !late && !(pre && held >= sps.maxDecMinus1+1) {
			return nil
		}
		if _, err := d.outputOne(); err != nil {
			return err
		}
	}
}

// outputOne emits the waiting picture with the lowest POC.
func (d *vaH265Decoder) outputOne() (bool, error) {
	var best *vaH265Pic
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

func (d *vaH265Decoder) outputAll() error {
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

func (d *vaH265Decoder) emit(p *vaH265Pic) error {
	s := d.s
	if s == nil {
		if d.dryEmit != nil {
			d.dryEmit(p.poc, p.pts)
		}
		return nil
	}
	if err := s.sync(p.surface); err != nil {
		return err
	}
	a := &d.active
	f := d.frames.Get(a.outW, a.outH, NV12)
	f.PTS = p.pts
	if err := s.readSurface(p.surface, f, a.cropX, a.cropY, a.width, a.height); err != nil {
		f.Release()
		return err
	}
	d.out.push(f)
	return nil
}

func (d *vaH265Decoder) Receive() (*VideoFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *vaH265Decoder) Flush() error {
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

func (d *vaH265Decoder) Backend() BackendInfo { return d.info }

func (d *vaH265Decoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.s != nil {
		d.s.close()
	}
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}
