package gaal

import (
	"bytes"
	"io"
)

// Low-delay decoding. Backends return each picture when it is decoded, so
// nothing here runs for a stream that is what LowDelay promises. reorderGuard
// checks the promise: it reads the picture order count of every H.264 and H.265
// picture and notices when one precedes a picture that was decoded before it.

// ReorderStats describes what a VideoConfig.LowDelay decoder found.
type ReorderStats struct {
	// Reordered counts pictures that precede, in output order, a picture
	// decoded before them. It stays zero for a stream without B-frames.
	Reordered uint64
	// Dropped counts pictures that were discarded because a later picture had
	// already been returned.
	Dropped uint64
	// Depth is the number of frames the decoder now holds back to restore
	// presentation order.
	Depth int
}

// ReorderReporter is implemented by decoders opened with VideoConfig.LowDelay.
type ReorderReporter interface {
	ReorderStats() ReorderStats
}

// maxReorderDepth is the largest reordering H.264 and H.265 allow.
const maxReorderDepth = 16

// h264Seq holds the SPS fields needed to read slice headers and output order.
type h264Seq struct {
	valid                      bool
	profile, constraints       uint32
	level                      uint32
	separatePlanes             bool
	frameBits, pocType, lsbLen int
	numRef, mbW, mbH           int
	frameMBsOnly               bool
	gapsAllowed                bool // gaps_in_frame_num_value_allowed_flag
	offsetForNonRef            int32
	restricted                 bool // the VUI states max_num_reorder_frames
	maxReorder, maxBuffering   int
	vuiPos, restrictPos        int // bit positions of the two flags
	hasVUI, vuiOK              bool
}

// parseHead reads an SPS RBSP up to vui_parameters_present_flag.
func (s *h264Seq) parseHead(r *bitReader) {
	s.profile = r.u(8)
	s.constraints = r.u(8)
	s.level = r.u(8)
	r.ue() // seq_parameter_set_id
	switch s.profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		if chroma == 3 {
			s.separatePlanes = r.bit() == 1
		}
		r.ue()
		r.ue()
		r.skip(1)
		if r.bit() == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.bit() == 1 {
					if i < 6 {
						r.h264ScalingList(16)
					} else {
						r.h264ScalingList(64)
					}
				}
			}
		}
	}
	s.frameBits = int(r.ue()) + 4
	s.pocType = int(r.ue())
	switch s.pocType {
	case 0:
		s.lsbLen = int(r.ue()) + 4
	case 1:
		r.skip(1)
		s.offsetForNonRef = r.se()
		r.se()
		for n := r.ue(); n > 0 && !r.bad; n-- {
			r.se()
		}
	}
	s.numRef = int(r.ue())
	s.gapsAllowed = r.bit() == 1
	s.mbW = int(r.ue()) + 1
	s.mbH = int(r.ue()) + 1
	s.frameMBsOnly = r.bit() == 1
	if !s.frameMBsOnly {
		s.mbH *= 2
		r.skip(1)
	}
	r.skip(1)
	if r.bit() == 1 { // frame_cropping_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
}

func (r *bitReader) skipHRD() {
	n := r.ue()
	if n > 31 {
		r.bad = true
		return
	}
	r.skip(8)
	for i := uint32(0); i <= n && !r.bad; i++ {
		r.ue()
		r.ue()
		r.skip(1)
	}
	r.skip(20)
}

// parseVUI reads the reordering limits at the end of the VUI.
func (s *h264Seq) parseVUI(r *bitReader) {
	if r.bit() == 1 && r.u(8) == 255 { // aspect_ratio_idc is Extended_SAR
		r.skip(32)
	}
	if r.bit() == 1 { // overscan_info_present_flag
		r.skip(1)
	}
	if r.bit() == 1 { // video_signal_type_present_flag
		r.skip(4)
		if r.bit() == 1 {
			r.skip(24)
		}
	}
	if r.bit() == 1 { // chroma_loc_info_present_flag
		r.ue()
		r.ue()
	}
	if r.bit() == 1 { // timing_info_present_flag
		r.skip(65)
	}
	nal := r.bit() == 1
	if nal {
		r.skipHRD()
	}
	vcl := r.bit() == 1
	if vcl {
		r.skipHRD()
	}
	if nal || vcl {
		r.skip(1) // low_delay_hrd_flag
	}
	r.skip(1) // pic_struct_present_flag
	s.restrictPos, s.vuiOK = r.pos, !r.bad
	if r.bit() == 0 {
		return
	}
	r.skip(1)
	for i := 0; i < 4; i++ {
		r.ue()
	}
	reorder, buffering := r.ue(), r.ue()
	if !r.bad && reorder <= maxReorderDepth && buffering <= maxReorderDepth && reorder <= buffering {
		s.restricted, s.maxReorder, s.maxBuffering = true, int(reorder), int(buffering)
	}
}

// parseH264Seq reads an SPS RBSP, which starts after the NAL header.
func parseH264Seq(rbsp []byte) (s h264Seq) {
	r := bitReader{b: rbsp}
	s.parseHead(&r)
	if r.bad || s.frameBits > 16 || s.lsbLen > 16 || s.pocType > 2 {
		return h264Seq{}
	}
	s.valid = true
	s.vuiPos = r.pos
	if s.hasVUI = r.bit() == 1; s.hasVUI {
		v := r // a damaged VUI must not invalidate the rest
		s.parseVUI(&v)
	}
	return s
}

// spsWriter builds an RBSP bit by bit.
type spsWriter struct {
	b []byte
	n int
}

func (w *spsWriter) bit(v uint32) {
	if w.n%8 == 0 {
		w.b = append(w.b, 0)
	}
	w.b[len(w.b)-1] |= byte(v&1) << (7 - w.n%8)
	w.n++
}

func (w *spsWriter) ue(v uint32) {
	v++
	bits := 0
	for x := v; x > 1; x >>= 1 {
		bits++
	}
	for i := 0; i < bits; i++ {
		w.bit(0)
	}
	for i := bits; i >= 0; i-- {
		w.bit(v >> i)
	}
}

// appendNoReorderSPS appends an SPS NAL unit that states that pictures are not
// reordered. It reports false if nal already says so or cannot be rewritten.
func appendNoReorderSPS(dst, nal, rbsp []byte) ([]byte, bool) {
	s := parseH264Seq(rbsp)
	if !s.valid || (s.hasVUI && !s.vuiOK) || (s.restricted && s.maxReorder == 0) {
		return dst, false
	}
	w := spsWriter{b: make([]byte, 0, len(rbsp)+8)}
	r := bitReader{b: rbsp}
	end := s.vuiPos
	if s.hasVUI {
		end = s.restrictPos
	}
	for r.pos < end {
		w.bit(r.bit())
	}
	if !s.hasVUI {
		w.bit(1) // vui_parameters_present_flag
		for i := 0; i < 8; i++ {
			w.bit(0) // aspect ratio, overscan, signal, chroma, timing, HRD x2, pic_struct
		}
	}
	w.bit(1) // bitstream_restriction_flag
	w.bit(1) // motion_vectors_over_pic_boundaries_flag
	w.ue(0)  // max_bytes_per_pic_denom
	w.ue(0)  // max_bits_per_mb_denom
	w.ue(16) // log2_max_mv_length_horizontal
	w.ue(16) // log2_max_mv_length_vertical
	w.ue(0)  // max_num_reorder_frames
	w.ue(uint32(max(s.numRef, 1)))
	w.bit(1) // rbsp_stop_one_bit
	return appendNAL(dst, nal[0], w.b), true
}

// appendNAL appends a NAL unit made of header and rbsp, with emulation
// prevention.
func appendNAL(dst []byte, header byte, rbsp []byte) []byte {
	dst = append(dst, header)
	zeros := 0
	for _, v := range rbsp {
		if zeros >= 2 && v <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		dst = append(dst, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// appendBaselineSPS appends an SPS NAL unit that is nal relabelled as
// baseline profile, which needs a High SPS to use the defaults for chroma
// format, bit depth and scaling. Decoders that only reorder pictures above
// baseline profile then return each one when it is decoded. It reports false
// if nal is already baseline or cannot be relabelled.
func appendBaselineSPS(dst, nal, rbsp []byte) ([]byte, bool) {
	r := bitReader{b: rbsp}
	profile := r.u(8)
	if profile == 66 || len(rbsp) < 4 {
		return dst, false
	}
	w := spsWriter{b: make([]byte, 0, len(rbsp))}
	for _, v := range [...]uint32{66, r.u(8), r.u(8)} {
		for i := 7; i >= 0; i-- {
			w.bit(v >> i)
		}
	}
	w.ue(r.ue()) // seq_parameter_set_id
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		// chroma_format_idc 1, 8 bits, no lossless, no scaling matrix
		if r.ue() != 1 || r.ue() != 0 || r.ue() != 0 || r.bit() != 0 || r.bit() != 0 {
			return dst, false
		}
	}
	if r.bad {
		return dst, false
	}
	for r.pos < 8*len(rbsp) {
		w.bit(r.bit())
	}
	for len(w.b) > 0 && w.b[len(w.b)-1] == 0 {
		w.b = w.b[:len(w.b)-1]
	}
	return appendNAL(dst, nal[0], w.b), true
}

// spsRewriter replaces the SPS NAL units of H.264 packets with what rewrite
// makes of them. A packet without an SPS, or whose SPS rewrite declines, is
// returned unchanged. Otherwise the result is valid until the next call.
type spsRewriter struct {
	rewrite          func(dst, nal, rbsp []byte) ([]byte, bool)
	in, out, pkt, rb []byte
}

func (w *spsRewriter) apply(b []byte) []byte {
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 || pos+i+3 >= len(b) {
			return b
		}
		start := pos + i + 3
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		switch {
		case start == end:
		case b[start]&0x1F == 1, b[start]&0x1F == 5:
			return b
		case b[start]&0x1F == 7:
			nal := b[start:end]
			for len(nal) > 0 && nal[len(nal)-1] == 0 {
				nal = nal[:len(nal)-1]
			}
			if len(nal) < 4 {
				return b
			}
			if !bytes.Equal(nal, w.in) {
				w.in = append(w.in[:0], nal...)
				w.rb = appendRBSP(w.rb[:0], nal[1:], len(nal))
				var ok bool
				w.out, ok = w.rewrite(w.out[:0], nal, w.rb)
				if !ok {
					w.out = w.out[:0]
				}
			}
			if len(w.out) == 0 {
				return b
			}
			w.pkt = append(append(append(w.pkt[:0], b[:start]...), w.out...), b[start+len(nal):]...)
			return w.pkt
		}
		pos = end
	}
}

// appendRBSP appends nal without emulation prevention, at most limit bytes.
func appendRBSP(dst, nal []byte, limit int) []byte {
	zeros, n := 0, len(dst)
	for _, v := range nal {
		if len(dst)-n == limit {
			break
		}
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		dst = append(dst, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// statesNoReorder rewrites the SPS of a packet for decoders that wait for
// reordering unless the stream rules it out, and have no setting to prevent
// that. The result is valid until the next call. The picture data is unchanged.
func (g *reorderGuard) statesNoReorder(b []byte) []byte {
	if g.codec != H264 {
		return b
	}
	if g.noReorder.rewrite == nil {
		g.noReorder.rewrite = appendNoReorderSPS
	}
	return g.noReorder.apply(b)
}

// maxDpbMbs is MaxDpbMbs of table A-1.
func (s *h264Seq) maxDpbMbs() int {
	low := s.constraints&0x10 != 0 && (s.profile == 66 || s.profile == 77)
	switch l := s.level; {
	case l == 9 || l == 10 || (l == 11 && low):
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

// reorderDepth returns how many frames can precede another in decoding and
// follow it in output. Without a stated limit that is the whole DPB (A.3.1, E.2.1).
func (s *h264Seq) reorderDepth() int {
	intra := s.constraints&0x10 != 0 && (s.profile == 44 || s.profile == 100 || s.profile == 110 || s.profile == 122 || s.profile == 244)
	switch {
	case !s.valid, s.pocType == 2, s.profile == 66, intra:
		return 0
	case s.restricted:
		return s.maxReorder
	}
	return max(min(s.maxDpbMbs()/max(s.mbW*s.mbH, 1), maxReorderDepth), s.numRef, 1)
}

// h265Seq and h265Pic hold what slice headers need from the parameter sets.
type h265Seq struct {
	valid          bool
	separatePlanes bool
	lsbLen         int
}

type h265Pic struct {
	valid     bool
	sps       uint8
	extraBits int
	outputBit bool
}

// reorderGuard follows picture order in a low-delay stream. When a picture
// turns out to precede one that was already returned, it starts holding frames
// back, sorted by timestamp, so that later output is in order again.
type reorderGuard struct {
	codec Codec
	stats ReorderStats

	buf []byte // RBSP scratch space

	noReorder spsRewriter // state of statesNoReorder

	h264SPS [32]h264Seq
	h264PPS [256]int16 // SPS id + 1
	h265SPS [16]h265Seq
	h265PPS [64]h265Pic
	started bool

	prevMsb, prevLsb int32
	recent           [maxReorderDepth]int32
	nRecent, nextPoc int

	maxPTS  int64
	hasPTS  bool
	lastOut int64
	emitted bool
	held    []*VideoFrame
	eof     bool
}

func newReorderGuard(c Codec) *reorderGuard {
	if c != H264 && c != H265 {
		return nil
	}
	return &reorderGuard{codec: c, buf: make([]byte, 0, 512)}
}

// rbsp removes emulation prevention from at most limit output bytes.
func (g *reorderGuard) rbsp(nal []byte, limit int) []byte {
	g.buf = appendRBSP(g.buf[:0], nal, limit)
	return g.buf
}

// observe reads the parameter sets and the first slice of a packet.
func (g *reorderGuard) observe(p Packet) {
	b := p.Data
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 || pos+i+3 >= len(b) {
			return
		}
		start := pos + i + 3
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		var done bool
		switch {
		case start == end:
		case g.codec == H264:
			done = g.h264NAL(b[start:end], p)
		default:
			done = g.h265NAL(b[start:end], p)
		}
		if done {
			return
		}
		pos = end
	}
}

// h264NAL handles one NAL unit and reports whether it was the picture.
func (g *reorderGuard) h264NAL(nal []byte, p Packet) bool {
	kind := nal[0] & 0x1F
	switch kind {
	case 7:
		if len(nal) < 4 {
			return false
		}
		rbsp := g.rbsp(nal[1:], cap(g.buf))
		r := bitReader{b: rbsp}
		r.skip(24)
		if id := r.ue(); !r.bad && id < 32 {
			g.h264SPS[id] = parseH264Seq(rbsp)
		}
	case 8:
		r := bitReader{b: g.rbsp(nal[1:], 16)}
		pps, sps := r.ue(), r.ue()
		if !r.bad && pps < 256 && sps < 32 {
			g.h264PPS[pps] = int16(sps) + 1
		}
	case 1, 5:
		g.h264Slice(nal, kind == 5, p)
		return true
	}
	return false
}

func (g *reorderGuard) h264Slice(nal []byte, idr bool, p Packet) {
	r := bitReader{b: g.rbsp(nal[1:], 48)}
	r.ue() // first_mb_in_slice
	sliceType := r.ue() % 5
	pps := r.ue()
	if r.bad || pps >= 256 || g.h264PPS[pps] == 0 {
		return
	}
	s := &g.h264SPS[g.h264PPS[pps]-1]
	if !s.valid {
		return
	}
	if s.separatePlanes {
		r.skip(2)
	}
	r.skip(s.frameBits)
	if !s.frameMBsOnly && r.bit() == 1 {
		return // field pictures of one frame have no useful order here
	}
	if idr {
		r.ue() // idr_pic_id
	}
	switch s.pocType {
	case 0:
		lsb := int32(r.u(s.lsbLen))
		if r.bad {
			return
		}
		if idr {
			g.prevMsb, g.prevLsb = 0, 0
		}
		msb := pocMsb(g.prevMsb, g.prevLsb, lsb, s.lsbLen)
		if nal[0]>>5&3 != 0 { // nal_ref_idc
			g.prevMsb, g.prevLsb = msb, lsb
		}
		g.note(msb+lsb, idr, p)
	case 1:
		// Only B-slices can come out of order here.
		switch {
		case idr:
			g.note(0, true, p)
		case sliceType == 1 && s.offsetForNonRef < 0:
			g.note(g.highest()-1, false, p)
		default:
			g.note(g.highest()+1, false, p)
		}
	}
}

// pocMsb derives PicOrderCntMsb (8.2.1.1, 8.3.1).
func pocMsb(prevMsb, prevLsb, lsb int32, bits int) int32 {
	wrap := int32(1) << bits
	switch {
	case lsb < prevLsb && prevLsb-lsb >= wrap/2:
		return prevMsb + wrap
	case lsb > prevLsb && lsb-prevLsb > wrap/2:
		return prevMsb - wrap
	}
	return prevMsb
}

func (g *reorderGuard) h265NAL(nal []byte, p Packet) bool {
	if len(nal) < 3 {
		return false
	}
	kind := nal[0] >> 1 & 0x3F
	switch {
	case kind == 33:
		rbsp := g.rbsp(nal[2:], cap(g.buf))
		r := bitReader{b: rbsp}
		_, id, lsb, sep := h265SPSHead(&r)
		if !r.bad && id < 16 && lsb <= 16 {
			g.h265SPS[id] = h265Seq{valid: true, separatePlanes: sep, lsbLen: lsb}
		}
	case kind == 34:
		r := bitReader{b: g.rbsp(nal[2:], 16)}
		pps, sps := r.ue(), r.ue()
		r.skip(1) // dependent_slice_segments_enabled_flag
		output := r.bit() == 1
		extra := int(r.u(3))
		if !r.bad && pps < 64 && sps < 16 {
			g.h265PPS[pps] = h265Pic{valid: true, sps: uint8(sps), extraBits: extra, outputBit: output}
		}
	case kind <= 9 || (kind >= 16 && kind <= 21):
		g.h265Slice(nal, kind, p)
		return true
	}
	return false
}

func (g *reorderGuard) h265Slice(nal []byte, kind byte, p Packet) {
	r := bitReader{b: g.rbsp(nal[2:], 48)}
	if r.bit() == 0 {
		return // not the first segment of a picture
	}
	if kind >= 16 {
		r.skip(1) // no_output_of_prior_pics_flag
	}
	id := r.ue()
	if r.bad || id >= 64 || !g.h265PPS[id].valid {
		return
	}
	pps := &g.h265PPS[id]
	s := &g.h265SPS[pps.sps]
	if !s.valid {
		return
	}
	r.skip(pps.extraBits)
	r.ue() // slice_type
	if pps.outputBit {
		r.skip(1)
	}
	if s.separatePlanes {
		r.skip(2)
	}
	idr := kind == 19 || kind == 20
	reset := idr || (kind >= 16 && kind <= 18) || (kind == 21 && !g.started)
	var lsb int32
	if !idr {
		lsb = int32(r.u(s.lsbLen))
	}
	if r.bad {
		return
	}
	var msb int32
	if !reset {
		msb = pocMsb(g.prevMsb, g.prevLsb, lsb, s.lsbLen)
	}
	subLayerNonRef := kind < 16 && kind&1 == 0
	leading := kind >= 6 && kind <= 9
	if nal[1]&7 == 1 && !subLayerNonRef && !leading { // TemporalId 0
		g.prevMsb, g.prevLsb = msb, lsb
	}
	g.note(msb+lsb, reset, p)
}

func (g *reorderGuard) highest() int32 {
	var top int32
	for _, v := range g.recent[:g.nRecent] {
		top = max(top, v)
	}
	return top
}

// note records one picture in decoding order.
func (g *reorderGuard) note(poc int32, reset bool, p Packet) {
	pts := int64(p.PTS)
	if reset {
		g.nRecent, g.nextPoc = 0, 0
		g.hasPTS = false
	}
	g.started = true
	ahead := 0
	for _, v := range g.recent[:g.nRecent] {
		if v > poc {
			ahead++
		}
	}
	// A later timestamp means the picture order count restarted (an H.264
	// memory_management_control_operation 5), not that the picture is early.
	if ahead > 0 && g.hasPTS && pts <= g.maxPTS {
		g.stats.Reordered++
		// Equal timestamps give nothing to sort by.
		if pts < g.maxPTS {
			g.stats.Depth = max(g.stats.Depth, ahead)
		}
	}
	g.recent[g.nextPoc] = poc
	g.nextPoc = (g.nextPoc + 1) % len(g.recent)
	g.nRecent = min(g.nRecent+1, len(g.recent))
	if !g.hasPTS || pts > g.maxPTS {
		g.maxPTS, g.hasPTS = pts, true
	}
}

// hold inserts f in timestamp order, after frames with an equal timestamp.
func (g *reorderGuard) hold(f *VideoFrame) {
	i := len(g.held)
	for i > 0 && g.held[i-1].PTS > f.PTS {
		i--
	}
	g.held = append(g.held, nil)
	copy(g.held[i+1:], g.held[i:])
	g.held[i] = f
}

// next returns the next frame in output order.
func (g *reorderGuard) next(d VideoDecoder) (*VideoFrame, error) {
	for {
		if n := len(g.held); n > 0 && (n > g.stats.Depth || g.eof) {
			f := g.held[0]
			copy(g.held, g.held[1:])
			g.held[n-1] = nil
			g.held = g.held[:n-1]
			if g.emitted && int64(f.PTS) < g.lastOut {
				g.stats.Dropped++
				f.Release()
				continue
			}
			g.lastOut, g.emitted = int64(f.PTS), true
			return f, nil
		}
		if g.eof {
			return nil, io.EOF
		}
		f, err := d.Receive()
		switch {
		case err == io.EOF:
			g.eof = true
		case err != nil:
			return nil, err
		case g.stats.Depth == 0 && len(g.held) == 0:
			g.lastOut, g.emitted = int64(f.PTS), true
			return f, nil
		default:
			g.hold(f)
		}
	}
}

func (g *reorderGuard) close() {
	for i, f := range g.held {
		f.Release()
		g.held[i] = nil
	}
	g.held = g.held[:0]
}

// h264NALReorder returns the reorder depth of an SPS NAL unit.
func h264NALReorder(nal []byte) int {
	if len(nal) < 4 {
		return 0
	}
	rbsp, zeros := make([]byte, 0, min(len(nal), 512)), 0
	for _, v := range nal[1:] {
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	s := parseH264Seq(rbsp)
	return s.reorderDepth()
}
