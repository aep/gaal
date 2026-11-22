package rtsp

// spsReader reads an RBSP and reports overruns through bad.
type spsReader struct {
	b   []byte
	pos int
	bad bool
}

func (r *spsReader) bit() uint32 {
	i := r.pos >> 3
	if i >= len(r.b) {
		r.bad = true
		return 0
	}
	v := uint32(r.b[i]>>(7-r.pos&7)) & 1
	r.pos++
	return v
}

func (r *spsReader) u(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		v = v<<1 | r.bit()
	}
	return v
}

func (r *spsReader) skip(n int) {
	r.pos += n
	if r.pos > len(r.b)*8 {
		r.bad = true
		r.pos = len(r.b) * 8
	}
}

func (r *spsReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		zeros++
		if zeros > 31 || r.bad {
			r.bad = true
			return 0
		}
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *spsReader) se() int32 {
	v := r.ue()
	if v&1 != 0 {
		return int32(v/2) + 1
	}
	return -int32(v / 2)
}

func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, v := range b {
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

const maxDimension = 1 << 16

func validSize(w, h int64) (int, int, bool) {
	if w <= 0 || h <= 0 || w > maxDimension || h > maxDimension {
		return 0, 0, false
	}
	return int(w), int(h), true
}

type h264SPS struct {
	width, height int
	numRef        uint32
	pocType       uint32
	vuiPos        int // bit position of vui_parameters_present_flag
	hasVUI        bool
	// restrictPos is the bit position of bitstream_restriction_flag, valid when
	// vuiOK is set. restricted is that flag.
	restrictPos int
	vuiOK       bool
	restricted  bool
	rbsp        []byte
}

func parseH264SPS(nal []byte) (s h264SPS, ok bool) {
	if len(nal) < 4 {
		return s, false
	}
	s.rbsp = unescapeRBSP(nal[1:])
	r := spsReader{b: s.rbsp}
	profile := r.u(8)
	r.skip(16)
	r.ue()
	chroma, separate := uint32(1), uint32(0)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma > 3 {
			return s, false
		}
		if chroma == 3 {
			separate = r.bit()
		}
		r.ue()
		r.ue()
		r.skip(1)
		if r.bit() == 1 {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists && !r.bad; i++ {
				if r.bit() == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for j := 0; j < size && !r.bad; j++ {
					if next != 0 {
						next = (last + r.se() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	r.ue()
	s.pocType = r.ue()
	switch s.pocType {
	case 0:
		r.ue()
	case 1:
		r.skip(1)
		r.se()
		r.se()
		n := r.ue()
		if n > 255 {
			return s, false
		}
		for ; n > 0 && !r.bad; n-- {
			r.se()
		}
	}
	s.numRef = r.ue()
	r.skip(1)
	w := int64(r.ue()) + 1
	h := int64(r.ue()) + 1
	frameMBs := int64(r.bit())
	if frameMBs == 0 {
		r.skip(1)
	}
	r.skip(1)
	var cl, cr, ct, cb int64
	if r.bit() == 1 {
		cl, cr, ct, cb = int64(r.ue()), int64(r.ue()), int64(r.ue()), int64(r.ue())
	}
	s.vuiPos = r.pos
	s.hasVUI = r.bit() == 1
	if r.bad {
		return s, false
	}
	if s.hasVUI {
		s.restrictPos, s.vuiOK = r.skipVUI()
		s.restricted = s.vuiOK && r.bit() == 1 && !r.bad
	}
	cropX, cropY := int64(1), 2-frameMBs
	if chroma != 0 && separate == 0 {
		if chroma < 3 {
			cropX = 2
		}
		if chroma == 1 {
			cropY *= 2
		}
	}
	s.width, s.height, ok = validSize(w*16-(cl+cr)*cropX, h*16*(2-frameMBs)-(ct+cb)*cropY)
	return s, ok
}

// skipVUI moves to bitstream_restriction_flag and returns its position.
func (r *spsReader) skipVUI() (int, bool) {
	if r.bit() == 1 && r.u(8) == 255 { // aspect_ratio_idc, Extended_SAR
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
	return r.pos, !r.bad
}

func (r *spsReader) skipHRD() {
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

// h264SPSSize returns the cropped picture size from an SPS NAL unit.
func h264SPSSize(nal []byte) (int, int, bool) {
	s, ok := parseH264SPS(nal)
	return s.width, s.height, ok
}

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

// h264LowDelaySPS states in an SPS that pictures are never reordered, adding to
// its VUI or creating one. Without that statement a decoder must assume the
// worst and hold back as many frames as the level allows, which costs seconds
// on the high-level, low-resolution streams cameras produce. It applies only
// when the stream cannot use reordering in practice: picture order type 2, or
// a single reference frame. An SPS with its own restriction is left alone.
func h264LowDelaySPS(nal []byte) ([]byte, bool) {
	s, ok := parseH264SPS(nal)
	if !ok || s.restricted || (s.hasVUI && !s.vuiOK) || (s.pocType != 2 && s.numRef > 1) {
		return nil, false
	}
	w := spsWriter{b: make([]byte, 0, len(s.rbsp)+8)}
	r := spsReader{b: s.rbsp}
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
			w.bit(0) // aspect ratio, overscan, signal type, chroma location, timing, HRD x2, pic_struct
		}
	}
	w.bit(1) // bitstream_restriction_flag
	w.bit(1) // motion_vectors_over_pic_boundaries_flag
	w.ue(0)  // max_bytes_per_pic_denom
	w.ue(0)  // max_bits_per_mb_denom
	w.ue(16) // log2_max_mv_length_horizontal
	w.ue(16) // log2_max_mv_length_vertical
	w.ue(0)  // max_num_reorder_frames
	w.ue(max(s.numRef, 1))
	w.bit(1) // rbsp_stop_one_bit
	out := append(make([]byte, 0, len(w.b)+len(w.b)/8+2), nal[0])
	zeros := 0
	for _, v := range w.b {
		if zeros >= 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out, true
}

// h265SPSSize returns the cropped picture size from an SPS NAL unit.
func h265SPSSize(nal []byte) (int, int, bool) {
	if len(nal) < 6 {
		return 0, 0, false
	}
	r := spsReader{b: unescapeRBSP(nal[2:])}
	r.skip(4)
	sub := int(r.u(3))
	r.skip(1)
	r.skip(96)
	var prof, lvl [8]bool
	for i := 0; i < sub; i++ {
		prof[i], lvl[i] = r.bit() == 1, r.bit() == 1
	}
	if sub > 0 {
		r.skip(2 * (8 - sub))
	}
	for i := 0; i < sub; i++ {
		if prof[i] {
			r.skip(88)
		}
		if lvl[i] {
			r.skip(8)
		}
	}
	r.ue()
	chroma := r.ue()
	if chroma > 3 {
		return 0, 0, false
	}
	separate := uint32(0)
	if chroma == 3 {
		separate = r.bit()
	}
	w, h := int64(r.ue()), int64(r.ue())
	var cl, cr, ct, cb int64
	if r.bit() == 1 {
		cl, cr, ct, cb = int64(r.ue()), int64(r.ue()), int64(r.ue()), int64(r.ue())
	}
	if r.bad {
		return 0, 0, false
	}
	subW, subH := int64(1), int64(1)
	if separate == 0 {
		if chroma == 1 || chroma == 2 {
			subW = 2
		}
		if chroma == 1 {
			subH = 2
		}
	}
	return validSize(w-(cl+cr)*subW, h-(ct+cb)*subH)
}
