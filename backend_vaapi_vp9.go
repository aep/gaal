//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// VP9 profile 0 decoding. The uncompressed header is parsed and checked here,
// including that every tile lies inside the frame, and the frame goes to the
// hardware as it is. Reference slots and segmentation and loop filter state are
// tracked across frames, because the driver and hardware take them from us.
//
// A decoder without a session (s == nil) does all of this but sends nothing to a
// device, which lets the parser and state tracking be fuzzed without the GPU.

var (
	errVP9Header = fmt.Errorf("%w: vp9 frame header", ErrCorrupt)
	errVP9Frame  = fmt.Errorf("%w: vp9 frame data", ErrCorrupt)
	errVP9Ref    = fmt.Errorf("%w: vp9 frame has no reference", ErrCorrupt)
	errVP9Index  = fmt.Errorf("%w: vp9 superframe index", ErrCorrupt)

	// These describe valid streams the hardware path cannot take, so they are
	// plain errors and let failover try another backend.
	errVP9Profile = errors.New("vaapi: only vp9 profile 0 is supported")
	errVP9Size    = errors.New("vaapi: vp9 frame size not supported by this device")
	errVP9Scaling = errors.New("vaapi: vp9 reference scaling is not supported")
	errVP9Headers = errors.New("vaapi: vp9 uncompressed header too large")
)

const (
	vp9MinDim   = 16
	vp9NumSlots = 8
	vp9Surfaces = vp9NumSlots + 2
	vp9SyncCode = 0x498342
	vp9MaxTile  = 64 // MAX_TILE_WIDTH_B64
	vp9MinTile  = 4  // MIN_TILE_WIDTH_B64
)

// vp9Bits reads a header bit by bit. Reads past the end set bad and return 0.
type vp9Bits struct {
	b   []byte
	pos int
	bad bool
}

func (r *vp9Bits) f(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		if r.pos>>3 >= len(r.b) {
			r.bad = true
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *vp9Bits) flag() bool { return r.f(1) != 0 }

// su reads an n-bit magnitude followed by a sign bit.
func (r *vp9Bits) su(n int) int32 {
	v := int32(r.f(n))
	if r.flag() {
		return -v
	}
	return v
}

// vp9State is what persists from frame to frame in the headers.
type vp9State struct {
	refDeltas  [4]int8
	modeDeltas [2]int8
	segAbs     bool
	featOn     [8][4]bool
	featVal    [8][4]int16
}

func (s *vp9State) reset() {
	*s = vp9State{refDeltas: [4]int8{1, 0, -1, -1}}
}

// vp9Slot is one entry of the reference frame map.
type vp9Slot struct {
	valid   bool
	surface uint32
	w, h    int
}

type vp9Header struct {
	showExisting                 bool
	showIdx                      int
	key, show, errRes, intraOnly bool
	resetCtx                     int
	refresh                      uint8
	refIdx                       [3]int
	bias                         [3]bool
	w, h                         int
	highPrec                     bool
	filter                       int
	refreshCtx, parallel         bool
	ctxIdx                       int
	lfLevel, lfSharp             int
	lfDelta                      bool
	baseQ                        int
	dqYDC, dqUVDC, dqUVAC        int
	lossless                     bool
	segOn, segMap, segTemporal   bool
	treeProbs                    [7]uint8
	predProbs                    [3]uint8
	tileColsLog2, tileRowsLog2   int
	compSize                     int
	hdrSize                      int
	st                           vp9State
}

func (h *vp9Header) intra() bool { return h.key || h.intraOnly }

func (r *vp9Bits) vp9FrameSize(h *vp9Header) {
	h.w, h.h = int(r.f(16))+1, int(r.f(16))+1
}

func (r *vp9Bits) vp9RenderSize() {
	if r.flag() {
		r.f(16)
		r.f(16)
	}
}

func (r *vp9Bits) vp9ColorConfig() error {
	cs := r.f(3)
	if cs == 7 { // CS_RGB needs a profile with 4:4:4
		return errVP9Header
	}
	r.f(1) // color_range
	return nil
}

// parseVP9Header reads the uncompressed header (6.2). prev is the state before
// this frame and is not modified.
func parseVP9Header(b []byte, h *vp9Header, prev *vp9State, slots *[vp9NumSlots]vp9Slot) error {
	*h = vp9Header{}
	r := vp9Bits{b: b}
	if r.f(2) != 2 {
		return errVP9Header
	}
	low, high := r.f(1), r.f(1)
	if profile := high<<1 | low; profile != 0 {
		if r.bad {
			return errVP9Header
		}
		return errVP9Profile
	}
	if r.flag() { // show_existing_frame
		h.showExisting = true
		h.showIdx = int(r.f(3))
		if r.bad {
			return errVP9Header
		}
		return nil
	}
	h.key = !r.flag()
	h.show = r.flag()
	h.errRes = r.flag()
	if h.key {
		if r.f(24) != vp9SyncCode {
			return errVP9Header
		}
		if err := r.vp9ColorConfig(); err != nil {
			return err
		}
		r.vp9FrameSize(h)
		r.vp9RenderSize()
		h.refresh = 0xff
	} else {
		if !h.show {
			h.intraOnly = r.flag()
		}
		if !h.errRes {
			h.resetCtx = int(r.f(2))
		}
		if h.intraOnly {
			if r.f(24) != vp9SyncCode {
				return errVP9Header
			}
			h.refresh = uint8(r.f(8))
			r.vp9FrameSize(h)
			r.vp9RenderSize()
		} else {
			h.refresh = uint8(r.f(8))
			for i := range h.refIdx {
				h.refIdx[i] = int(r.f(3))
				h.bias[i] = r.flag()
			}
			found := false
			for i := 0; i < 3 && !found; i++ {
				if r.flag() {
					s := &slots[h.refIdx[i]]
					if !s.valid {
						return errVP9Ref
					}
					h.w, h.h = s.w, s.h
					found = true
				}
			}
			if !found {
				r.vp9FrameSize(h)
			}
			r.vp9RenderSize()
			h.highPrec = r.flag()
			h.filter = 4 // switchable
			if !r.flag() {
				h.filter = int([4]uint8{1, 0, 2, 3}[r.f(2)])
			}
		}
	}
	if r.bad {
		return errVP9Header
	}
	if !h.errRes {
		h.refreshCtx = r.flag()
		h.parallel = r.flag()
	} else {
		h.parallel = true
	}
	h.ctxIdx = int(r.f(2))
	if h.intra() {
		h.ctxIdx = 0
	}
	h.st = *prev
	if h.intra() || h.errRes {
		h.st.reset()
	}
	if err := r.vp9LoopFilter(h); err != nil {
		return err
	}
	r.vp9Quant(h)
	if err := r.vp9Segmentation(h); err != nil {
		return err
	}
	r.vp9Tiles(h)
	h.compSize = int(r.f(16))
	if r.bad || h.compSize == 0 {
		return errVP9Header
	}
	h.hdrSize = (r.pos + 7) / 8
	if h.hdrSize > 255 {
		return errVP9Headers // the driver interface has one byte for this
	}
	return nil
}

func (r *vp9Bits) vp9LoopFilter(h *vp9Header) error {
	h.lfLevel = int(r.f(6))
	h.lfSharp = int(r.f(3))
	h.lfDelta = r.flag()
	if h.lfDelta && r.flag() {
		for i := range h.st.refDeltas {
			if r.flag() {
				h.st.refDeltas[i] = int8(r.su(6))
			}
		}
		for i := range h.st.modeDeltas {
			if r.flag() {
				h.st.modeDeltas[i] = int8(r.su(6))
			}
		}
	}
	if r.bad {
		return errVP9Header
	}
	return nil
}

func (r *vp9Bits) vp9Quant(h *vp9Header) {
	h.baseQ = int(r.f(8))
	delta := func() int {
		if r.flag() {
			return int(r.su(4))
		}
		return 0
	}
	h.dqYDC, h.dqUVDC, h.dqUVAC = delta(), delta(), delta()
	h.lossless = h.baseQ == 0 && h.dqYDC == 0 && h.dqUVDC == 0 && h.dqUVAC == 0
}

func (r *vp9Bits) vp9Segmentation(h *vp9Header) error {
	for i := range h.treeProbs {
		h.treeProbs[i] = 255
	}
	for i := range h.predProbs {
		h.predProbs[i] = 255
	}
	h.segOn = r.flag()
	if !h.segOn {
		return nil
	}
	h.segMap = r.flag()
	if h.segMap {
		for i := range h.treeProbs {
			if r.flag() {
				h.treeProbs[i] = uint8(r.f(8))
			}
		}
		h.segTemporal = r.flag()
		if h.segTemporal {
			for i := range h.predProbs {
				if r.flag() {
					h.predProbs[i] = uint8(r.f(8))
				}
			}
		}
	}
	if r.flag() { // segmentation_update_data
		st := &h.st
		st.segAbs = r.flag()
		st.featOn = [8][4]bool{}
		st.featVal = [8][4]int16{}
		for i := 0; i < 8; i++ {
			for j, bits := range [4]int{8, 6, 2, 0} {
				if !r.flag() {
					continue
				}
				st.featOn[i][j] = true
				v := int16(r.f(bits))
				if j < 2 && r.flag() {
					v = -v
				}
				st.featVal[i][j] = v
			}
		}
	}
	if r.bad {
		return errVP9Header
	}
	return nil
}

func (r *vp9Bits) vp9Tiles(h *vp9Header) {
	sb := ((h.w+7)>>3 + 7) >> 3
	minLog := 0
	for vp9MaxTile<<minLog < sb {
		minLog++
	}
	maxLog := 1
	for sb>>maxLog >= vp9MinTile {
		maxLog++
	}
	maxLog--
	h.tileColsLog2 = minLog
	for h.tileColsLog2 < maxLog && r.flag() {
		h.tileColsLog2++
	}
	if r.flag() {
		h.tileRowsLog2 = 1
		if r.flag() {
			h.tileRowsLog2 = 2
		}
	}
}

// vp9CheckTiles verifies that the compressed header and every tile lie inside
// the frame, so the hardware is never pointed outside the data it was given.
func vp9CheckTiles(b []byte, h *vp9Header) error {
	at := h.hdrSize + h.compSize
	if at >= len(b) {
		return errVP9Frame
	}
	tiles := 1 << (h.tileColsLog2 + h.tileRowsLog2)
	for i := 0; i < tiles-1; i++ {
		if at+4 > len(b) {
			return errVP9Frame
		}
		n := int(b[at])<<24 | int(b[at+1])<<16 | int(b[at+2])<<8 | int(b[at+3])
		at += 4
		if n == 0 || n > len(b)-at {
			return errVP9Frame
		}
		at += n
	}
	if at >= len(b) {
		return errVP9Frame
	}
	return nil
}

func vp9Clamp(v, hi int) int { return max(0, min(v, hi)) }

type vaVP9Decoder struct {
	s        *vaSession
	info     BackendInfo
	displays []*vaDisplay

	hdr      vp9Header
	st       vp9State
	slots    [vp9NumSlots]vp9Slot
	surfaces []uint32
	hasCtx   bool
	w, h     int
	surfW    int
	surfH    int
	pts      time.Duration

	picParam []byte
	sliceBuf []byte

	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
	closed  bool

	dryEmit func(w, h int, pts time.Duration) // test hook for s == nil
}

func newVAVP9Decoder(info BackendInfo) (VideoDecoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaVP9Profiles, vaVLD)
	if err != nil {
		return nil, err
	}
	d := newVP9Decoder(info)
	d.s, d.displays = newVASession(a, ds[0]), ds
	return d, nil
}

func newVP9Decoder(info BackendInfo) *vaVP9Decoder {
	d := &vaVP9Decoder{info: info, picParam: make([]byte, vaSizeVPic), sliceBuf: make([]byte, vaSizeVSlice)}
	d.st.reset()
	return d
}

func (d *vaVP9Decoder) Send(p Packet) error {
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
	b := p.Data
	m := b[len(b)-1]
	if m&0xe0 == 0xc0 {
		n, mag := int(m&7)+1, int(m>>3&3)+1
		size := 2 + mag*n
		if len(b) >= size && b[len(b)-size] == m {
			return d.superframe(b[:len(b)-size], b[len(b)-size+1:], n, mag)
		}
	}
	return d.decodeFrame(b)
}

// superframe decodes the frames listed in a superframe index.
func (d *vaVP9Decoder) superframe(data, index []byte, n, mag int) error {
	var first error
	at := 0
	for i := 0; i < n; i++ {
		size := 0
		for j := 0; j < mag; j++ {
			size |= int(index[i*mag+j]) << (8 * j)
		}
		if size > len(data)-at {
			if first == nil {
				first = errVP9Index
			}
			return first
		}
		if size > 0 {
			if err := d.decodeFrame(data[at : at+size]); err != nil && first == nil {
				first = err
			}
		}
		at += size
	}
	return first
}

func (d *vaVP9Decoder) decodeFrame(b []byte) error {
	h := &d.hdr
	if err := parseVP9Header(b, h, &d.st, &d.slots); err != nil {
		return err
	}
	if h.showExisting {
		s := &d.slots[h.showIdx]
		if !s.valid {
			return errVP9Ref
		}
		return d.emit(s.surface, s.w, s.h)
	}
	if err := vp9CheckTiles(b, h); err != nil {
		return err
	}
	if h.w < vp9MinDim || h.h < vp9MinDim {
		return errVP9Size
	}
	if !h.intra() {
		for _, i := range h.refIdx {
			s := &d.slots[i]
			if !s.valid {
				return errVP9Ref
			}
			if s.w != h.w || s.h != h.h {
				return errVP9Scaling
			}
		}
	}
	if !d.hasCtx || d.w != h.w || d.h != h.h {
		if !h.key && d.hasCtx {
			return errVP9Scaling
		}
		if err := d.activate(h); err != nil {
			return err
		}
	}
	cur, err := d.freeSurface()
	if err != nil {
		return err
	}
	if err := d.submit(b, h, cur); err != nil {
		return err
	}
	d.st = h.st
	for i := range d.slots {
		if h.refresh>>uint(i)&1 != 0 {
			d.slots[i] = vp9Slot{valid: true, surface: cur, w: h.w, h: h.h}
		}
	}
	if h.show {
		return d.emit(cur, h.w, h.h)
	}
	return nil
}

// activate prepares a context for a new frame size. Every reference is replaced
// by the keyframe or intra-only frame that causes this, or was already absent.
func (d *vaVP9Decoder) activate(h *vp9Header) error {
	for i := range d.slots {
		d.slots[i] = vp9Slot{}
	}
	d.hasCtx = false
	surfW, surfH := (h.w+15)&^15, (h.h+15)&^15
	if d.s == nil {
		d.surfaces = d.surfaces[:0]
		for i := 0; i < vp9Surfaces; i++ {
			d.surfaces = append(d.surfaces, uint32(i+1))
		}
		d.w, d.h, d.surfW, d.surfH, d.hasCtx = h.w, h.h, surfW, surfH, true
		return nil
	}
	s := d.s
	var disp *vaDisplay
	var ep vaEndpoint
	for _, c := range d.displays {
		if e := vaPick(c, vaVP9Profiles, vaVLD); e.profile >= 0 {
			disp, ep = c, e
			break
		}
	}
	if disp == nil {
		return errVP9Profile
	}
	if maxW, err := s.attribute(ep, vaAttrMaxPicWidth); err == nil && maxW != vaAttribNotSupported && h.w > int(maxW) {
		return errVP9Size
	}
	if maxH, err := s.attribute(ep, vaAttrMaxPicHeight); err == nil && maxH != vaAttribNotSupported && h.h > int(maxH) {
		return errVP9Size
	}
	s.destroyVideo()
	s.destroyConfig()
	s.disp, s.dpy = disp, disp.dpy
	if err := s.createConfig(ep, vaAttrRTFormat, vaRTYUV420); err != nil {
		return err
	}
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, surfW, surfH, vp9Surfaces); err != nil {
		return err
	}
	if err := s.createContext(h.w, h.h, vaProgressive); err != nil {
		s.destroyVideo()
		return err
	}
	d.surfaces = s.surfaces
	d.w, d.h, d.surfW, d.surfH, d.hasCtx = h.w, h.h, surfW, surfH, true
	return nil
}

// freeSurface returns a surface no reference slot holds.
func (d *vaVP9Decoder) freeSurface() (uint32, error) {
	for _, id := range d.surfaces {
		used := false
		for i := range d.slots {
			used = used || (d.slots[i].valid && d.slots[i].surface == id)
		}
		if !used {
			return id, nil
		}
	}
	return 0, errVANoSurface
}

func (d *vaVP9Decoder) fillPicParam(h *vp9Header) {
	b := d.picParam
	clear(b)
	vaPut16(b, vaoVPW, uint16(h.w))
	vaPut16(b, vaoVPH, uint16(h.h))
	for i := range d.slots {
		id := uint32(vaInvalidID)
		if d.slots[i].valid && !h.key {
			id = d.slots[i].surface
		}
		le32(b, vaoVPRefs+4*i, id)
	}
	le32(b, vaoVPFields, 1<<vaBitVPSubX|1<<vaBitVPSubY|
		vaBool(!h.key, vaBitVPFrameType)|vaBool(h.show, vaBitVPShow)|
		vaBool(h.errRes, vaBitVPErrRes)|vaBool(h.intraOnly, vaBitVPIntraOnly)|
		vaBool(h.highPrec, vaBitVPHighPrec)|uint32(h.filter)<<vaBitVPFilterType|
		vaBool(h.parallel, vaBitVPParallel)|uint32(h.resetCtx)<<vaBitVPResetCtx|
		vaBool(h.refreshCtx, vaBitVPRefreshCtx)|uint32(h.ctxIdx)<<vaBitVPCtxIdx|
		vaBool(h.segOn, vaBitVPSegOn)|vaBool(h.segTemporal, vaBitVPSegTemporal)|
		vaBool(h.segMap, vaBitVPSegMap)|
		uint32(h.refIdx[0])<<vaBitVPLast|vaBool(h.bias[0], vaBitVPLastBias)|
		uint32(h.refIdx[1])<<vaBitVPGolden|vaBool(h.bias[1], vaBitVPGoldenBias)|
		uint32(h.refIdx[2])<<vaBitVPAlt|vaBool(h.bias[2], vaBitVPAltBias)|
		vaBool(h.lossless, vaBitVPLossless))
	b[vaoVPFilter] = uint8(h.lfLevel)
	b[vaoVPSharp] = uint8(h.lfSharp)
	b[vaoVPTileRows] = uint8(h.tileRowsLog2)
	b[vaoVPTileCols] = uint8(h.tileColsLog2)
	b[vaoVPHdrLen] = uint8(h.hdrSize)
	vaPut16(b, vaoVPFirstPart, uint16(h.compSize))
	copy(b[vaoVPTreeProbs:], h.treeProbs[:])
	copy(b[vaoVPPredProbs:], h.predProbs[:])
	b[vaoVPProfile] = 0
	b[vaoVPDepth] = 8
}

// fillSegments computes the per-segment quantizers and loop filter levels.
func (d *vaVP9Decoder) fillSegments(h *vp9Header) {
	b := d.sliceBuf
	st := &h.st
	for i := 0; i < 8; i++ {
		seg := b[vaoVLSeg+i*vaSizeVSeg:]
		q := h.baseQ
		lvl := h.lfLevel
		var flags uint16
		if h.segOn {
			if st.featOn[i][0] {
				q = int(st.featVal[i][0])
				if !st.segAbs {
					q += h.baseQ
				}
				q = vp9Clamp(q, 255)
			}
			if st.featOn[i][1] {
				lvl = int(st.featVal[i][1])
				if !st.segAbs {
					lvl += h.lfLevel
				}
				lvl = vp9Clamp(lvl, 63)
			}
			if st.featOn[i][2] {
				flags |= 1<<vaBitVGRefOn | uint16(st.featVal[i][2])<<vaBitVGRef
			}
			if st.featOn[i][3] {
				flags |= 1 << vaBitVGSkip
			}
		}
		vaPut16(seg, vaoVGFlags, flags)
		vaPut16(seg, vaoVGLumaDC, uint16(vp9DCQ[vp9Clamp(q+h.dqYDC, 255)]))
		vaPut16(seg, vaoVGLumaAC, uint16(vp9ACQ[q]))
		vaPut16(seg, vaoVGChromaDC, uint16(vp9DCQ[vp9Clamp(q+h.dqUVDC, 255)]))
		vaPut16(seg, vaoVGChromaAC, uint16(vp9ACQ[vp9Clamp(q+h.dqUVAC, 255)]))
		f := seg[vaoVGFilter : vaoVGFilter+8]
		if !h.lfDelta {
			for j := range f {
				f[j] = uint8(lvl)
			}
			continue
		}
		// Like libvpx, the scale follows the frame level, not the segment's.
		shift := h.lfLevel >> 5
		f[0] = uint8(vp9Clamp(lvl+int(st.refDeltas[0])<<shift, 63))
		f[1] = f[0]
		for r := 1; r < 4; r++ {
			for m := 0; m < 2; m++ {
				f[2*r+m] = uint8(vp9Clamp(lvl+(int(st.refDeltas[r])+int(st.modeDeltas[m]))<<shift, 63))
			}
		}
	}
}

func (d *vaVP9Decoder) submit(data []byte, h *vp9Header, cur uint32) error {
	d.fillPicParam(h)
	b := d.sliceBuf
	clear(b)
	le32(b, vaoVLSize, uint32(len(data)))
	le32(b, vaoVLFlag, vaSliceDataAll)
	d.fillSegments(h)
	s := d.s
	if s == nil {
		return nil
	}
	if err := s.addBuffer(vaBufPicParam, unsafe.Pointer(&d.picParam[0]), vaSizeVPic); err != nil {
		s.dropBuffers()
		return err
	}
	if err := s.addBuffer(vaBufSliceParam, unsafe.Pointer(&b[0]), vaSizeVSlice); err != nil {
		s.dropBuffers()
		return err
	}
	if err := s.addBuffer(vaBufSliceData, unsafe.Pointer(&data[0]), len(data)); err != nil {
		s.dropBuffers()
		return err
	}
	return s.submit(cur)
}

func (d *vaVP9Decoder) emit(surface uint32, w, h int) error {
	s := d.s
	if s == nil {
		if d.dryEmit != nil {
			d.dryEmit(w, h, d.pts)
		}
		return nil
	}
	if err := s.sync(surface); err != nil {
		return err
	}
	f := d.frames.Get(w, h, NV12)
	f.PTS = d.pts
	if err := s.readSurface(surface, f, 0, 0, d.surfW, d.surfH); err != nil {
		f.Release()
		return err
	}
	d.out.push(f)
	return nil
}

func (d *vaVP9Decoder) Receive() (*VideoFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *vaVP9Decoder) Flush() error {
	d.flushed = true
	return nil
}

func (d *vaVP9Decoder) Backend() BackendInfo { return d.info }

func (d *vaVP9Decoder) Close() error {
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

// Quantizer tables for 8-bit video.
var vp9DCQ = [256]int16{
	4, 8, 8, 9, 10, 11, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19,
	20, 21, 22, 23, 24, 25, 26, 26, 27, 28, 29, 30, 31, 32, 32, 33,
	34, 35, 36, 37, 38, 38, 39, 40, 41, 42, 43, 43, 44, 45, 46, 47,
	48, 48, 49, 50, 51, 52, 53, 53, 54, 55, 56, 57, 57, 58, 59, 60,
	61, 62, 62, 63, 64, 65, 66, 66, 67, 68, 69, 70, 70, 71, 72, 73,
	74, 74, 75, 76, 77, 78, 78, 79, 80, 81, 81, 82, 83, 84, 85, 85,
	87, 88, 90, 92, 93, 95, 96, 98, 99, 101, 102, 104, 105, 107, 108, 110,
	111, 113, 114, 116, 117, 118, 120, 121, 123, 125, 127, 129, 131, 134, 136, 138,
	140, 142, 144, 146, 148, 150, 152, 154, 156, 158, 161, 164, 166, 169, 172, 174,
	177, 180, 182, 185, 187, 190, 192, 195, 199, 202, 205, 208, 211, 214, 217, 220,
	223, 226, 230, 233, 237, 240, 243, 247, 250, 253, 257, 261, 265, 269, 272, 276,
	280, 284, 288, 292, 296, 300, 304, 309, 313, 317, 322, 326, 330, 335, 340, 344,
	349, 354, 359, 364, 369, 374, 379, 384, 389, 395, 400, 406, 411, 417, 423, 429,
	435, 441, 447, 454, 461, 467, 475, 482, 489, 497, 505, 513, 522, 530, 539, 549,
	559, 569, 579, 590, 602, 614, 626, 640, 654, 668, 684, 700, 717, 736, 755, 775,
	796, 819, 843, 869, 896, 925, 955, 988, 1022, 1058, 1098, 1139, 1184, 1232, 1282, 1336,
}

var vp9ACQ = [256]int16{
	4, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22,
	23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38,
	39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54,
	55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 65, 66, 67, 68, 69, 70,
	71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86,
	87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102,
	104, 106, 108, 110, 112, 114, 116, 118, 120, 122, 124, 126, 128, 130, 132, 134,
	136, 138, 140, 142, 144, 146, 148, 150, 152, 155, 158, 161, 164, 167, 170, 173,
	176, 179, 182, 185, 188, 191, 194, 197, 200, 203, 207, 211, 215, 219, 223, 227,
	231, 235, 239, 243, 247, 251, 255, 260, 265, 270, 275, 280, 285, 290, 295, 300,
	305, 311, 317, 323, 329, 335, 341, 347, 353, 359, 366, 373, 380, 387, 394, 401,
	408, 416, 424, 432, 440, 448, 456, 465, 474, 483, 492, 501, 510, 520, 530, 540,
	550, 560, 571, 582, 593, 604, 615, 627, 639, 651, 663, 676, 689, 702, 715, 729,
	743, 757, 771, 786, 801, 816, 832, 848, 864, 881, 898, 915, 933, 951, 969, 988,
	1007, 1026, 1046, 1066, 1087, 1108, 1129, 1151, 1173, 1196, 1219, 1243, 1267, 1292, 1317, 1343,
	1369, 1396, 1423, 1451, 1479, 1508, 1537, 1567, 1597, 1628, 1660, 1692, 1725, 1759, 1793, 1828,
}
