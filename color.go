package gaal

import (
	"bytes"
	"strings"
	"time"
)

// ColorRange says how sample values map to black and white. gaal never converts
// samples to another range. It reports what a stream says and signals what the
// caller declares.
type ColorRange uint8

// Color ranges.
const (
	// RangeUnspecified means the stream does not say. Players then normally
	// assume limited range.
	RangeUnspecified ColorRange = iota
	RangeLimited                // luma 16-235
	RangeFull                   // luma 0-255
)

func (r ColorRange) String() string {
	switch r {
	case RangeLimited:
		return "limited"
	case RangeFull:
		return "full"
	}
	return "unspecified"
}

// bitReader reads an RBSP. Reading past the end yields zeros and sets bad.
type bitReader struct {
	b   []byte
	pos int
	bad bool
}

func (r *bitReader) bit() uint32 {
	i := r.pos >> 3
	if i >= len(r.b) {
		r.bad = true
		return 0
	}
	v := uint32(r.b[i]>>(7-r.pos&7)) & 1
	r.pos++
	return v
}

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		v = v<<1 | r.bit()
	}
	return v
}

func (r *bitReader) skip(n int) { r.pos += n }

func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if zeros++; zeros > 31 || r.bad {
			r.bad = true
			return 0
		}
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *bitReader) se() int32 {
	v := r.ue()
	if v&1 != 0 {
		return int32(v/2) + 1
	}
	return -int32(v / 2)
}

// vuiRange reads the start of H.264 or H.265 VUI, which have the same layout up
// to video_full_range_flag.
func (r *bitReader) vuiRange() (ColorRange, bool) {
	if r.bit() == 1 { // aspect_ratio_info_present_flag
		if r.u(8) == 255 {
			r.skip(32)
		}
	}
	if r.bit() == 1 { // overscan_info_present_flag
		r.skip(1)
	}
	if r.bit() == 0 { // video_signal_type_present_flag
		return RangeUnspecified, !r.bad
	}
	r.skip(3) // video_format
	full := r.bit()
	if r.bad {
		return RangeUnspecified, false
	}
	if full == 1 {
		return RangeFull, true
	}
	return RangeLimited, true
}

func (r *bitReader) h264ScalingList(size int) {
	last, next := int32(8), int32(8)
	for i := 0; i < size && !r.bad; i++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// h264SPSRange reads the range from an H.264 SPS RBSP, after the NAL header.
func h264SPSRange(b []byte) (ColorRange, bool) {
	r := bitReader{b: b}
	var s h264Seq
	s.parseHead(&r)
	if r.bit() == 0 { // vui_parameters_present_flag
		return RangeUnspecified, !r.bad
	}
	return r.vuiRange()
}

// h265SPSHead reads an H.265 SPS RBSP through log2_max_pic_order_cnt_lsb_minus4.
func h265SPSHead(r *bitReader) (subLayers int, id uint32, pocBits int, separatePlanes bool) {
	r.skip(4)
	subLayers = int(r.u(3))
	r.skip(1)
	r.skip(96) // general profile, tier and level
	var profile, level [8]bool
	for i := 0; i < subLayers; i++ {
		profile[i], level[i] = r.bit() == 1, r.bit() == 1
	}
	if subLayers > 0 {
		r.skip(2 * (8 - subLayers))
	}
	for i := 0; i < subLayers; i++ {
		if profile[i] {
			r.skip(88)
		}
		if level[i] {
			r.skip(8)
		}
	}
	id = r.ue()
	if r.ue() == 3 { // chroma_format_idc
		separatePlanes = r.bit() == 1
	}
	r.ue()
	r.ue()
	if r.bit() == 1 { // conformance_window_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue()
	r.ue()
	pocBits = int(r.ue()) + 4
	return subLayers, id, pocBits, separatePlanes
}

// h265SPSRange reads the range from an H.265 SPS RBSP, after the NAL header.
func h265SPSRange(b []byte) (ColorRange, bool) {
	r := bitReader{b: b}
	if !h265SPSToVUI(&r) {
		return RangeUnspecified, false
	}
	if r.bit() == 0 { // vui_parameters_present_flag
		return RangeUnspecified, !r.bad
	}
	return r.vuiRange()
}

// h265SPSToVUI reads an SPS up to vui_parameters_present_flag.
func h265SPSToVUI(r *bitReader) bool {
	subLayers, _, pocBits, _ := h265SPSHead(r)
	first := subLayers
	if r.bit() == 1 {
		first = 0
	}
	for i := first; i <= subLayers && !r.bad; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	for i := 0; i < 6; i++ {
		r.ue()
	}
	if r.bit() == 1 && r.bit() == 1 { // scaling lists present in the SPS
		for size := 0; size < 4; size++ {
			step := 1
			if size == 3 {
				step = 3
			}
			for m := 0; m < 6; m += step {
				if r.bit() == 0 {
					r.ue()
					continue
				}
				if size > 1 {
					r.se()
				}
				for n := min(64, 1<<(4+size*2)); n > 0 && !r.bad; n-- {
					r.se()
				}
			}
		}
	}
	r.skip(2)
	if r.bit() == 1 { // pcm_enabled_flag
		r.skip(8)
		r.ue()
		r.ue()
		r.skip(1)
	}
	sets := int(r.ue())
	if sets > 64 {
		return false
	}
	var deltas [65]int
	for i := 0; i < sets && !r.bad; i++ {
		if i != 0 && r.bit() == 1 { // inter_ref_pic_set_prediction_flag
			r.skip(1)
			r.ue()
			n := 0
			for j := 0; j <= deltas[i-1] && !r.bad; j++ {
				used := r.bit() == 1
				if used || r.bit() == 1 {
					n++
				}
			}
			deltas[i] = min(n, 16)
			continue
		}
		neg, pos := int(r.ue()), int(r.ue())
		if neg+pos > 16 {
			return false
		}
		deltas[i] = neg + pos
		for j := 0; j < neg+pos; j++ {
			r.ue()
			r.skip(1)
		}
	}
	if r.bit() == 1 { // long_term_ref_pics_present_flag
		for n := r.ue(); n > 0 && !r.bad; n-- {
			r.skip(pocBits + 1)
		}
	}
	r.skip(2)
	return !r.bad
}

// appendH265SignalRange appends an H.265 SPS NAL unit that is nal with a
// video_signal_type stating range r. It reports false if nal already has one
// or cannot be read. VideoToolbox leaves it out for limited range, so a
// decoder could not tell limited from unspecified.
func appendH265SignalRange(dst, nal []byte, r ColorRange) ([]byte, bool) {
	if len(nal) < 4 || r == RangeUnspecified {
		return dst, false
	}
	rbsp := appendRBSP(nil, nal[2:], len(nal))
	rd := bitReader{b: rbsp}
	if !h265SPSToVUI(&rd) {
		return dst, false
	}
	w := spsWriter{b: make([]byte, 0, len(rbsp)+4)}
	copyBits := func(end int) {
		for src := (bitReader{b: rbsp}); src.pos < end; {
			w.bit(src.bit())
		}
	}
	signal := func() {
		w.bit(1) // video_signal_type_present_flag
		for _, v := range [3]uint32{1, 0, 1} {
			w.bit(v) // video_format 5, unspecified
		}
		if r == RangeFull {
			w.bit(1)
		} else {
			w.bit(0)
		}
		w.bit(0) // colour_description_present_flag
	}
	var rest int // where the bits after the insertion start
	if rd.bit() == 0 {
		copyBits(rd.pos - 1)
		w.bit(1) // vui_parameters_present_flag
		w.bit(0) // aspect_ratio_info_present_flag
		w.bit(0) // overscan_info_present_flag
		signal()
		for i := 0; i < 7; i++ {
			// chroma_loc_info, neutral_chroma, field_seq, frame_field_info,
			// default_display_window, vui_timing_info, bitstream_restriction
			w.bit(0)
		}
		rest = rd.pos
	} else {
		if rd.bit() == 1 { // aspect_ratio_info_present_flag
			if rd.u(8) == 255 {
				rd.skip(32)
			}
		}
		if rd.bit() == 1 { // overscan_info_present_flag
			rd.skip(1)
		}
		if rd.bit() == 1 || rd.bad { // video_signal_type_present_flag
			return dst, false
		}
		copyBits(rd.pos - 1)
		signal()
		rest = rd.pos
	}
	for src := (bitReader{b: rbsp, pos: rest}); src.pos < 8*len(rbsp); {
		w.bit(src.bit())
	}
	for len(w.b) > 0 && w.b[len(w.b)-1] == 0 {
		w.b = w.b[:len(w.b)-1]
	}
	return appendNAL(append(dst, nal[0]), nal[1], w.b), true
}

var startCode3 = []byte{0, 0, 1}

// colorTracker follows the range a compressed stream declares.
type colorTracker struct {
	codec   Codec
	current ColorRange
	sps     []byte // last parameter set, to skip reparsing
	rbsp    []byte
}

func newColorTracker(c Codec) colorTracker {
	t := colorTracker{codec: c}
	switch c {
	case JPEG:
		t.current = RangeFull
	case VP8:
		t.current = RangeLimited
	}
	return t
}

func (t *colorTracker) parseSPS(nal []byte) {
	if bytes.Equal(nal, t.sps) {
		return
	}
	t.sps = append(t.sps[:0], nal...)
	header := 1
	if t.codec == H265 {
		header = 2
	}
	if len(nal) <= header {
		return
	}
	// Remove emulation prevention bytes.
	out, zeros := t.rbsp[:0], 0
	for _, v := range nal[header:] {
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
	t.rbsp = out
	var r ColorRange
	var ok bool
	if t.codec == H265 {
		r, ok = h265SPSRange(out)
	} else {
		r, ok = h264SPSRange(out)
	}
	if ok {
		t.current = r
	}
}

// observe updates the range from one packet. It stops at the first picture
// data, so it costs almost nothing on packets without parameter sets.
func (t *colorTracker) observe(b []byte) {
	switch t.codec {
	case VP9:
		if len(b) > 0 && b[0]&0xC0 == 0x80 {
			if f, ok := parseVP9Frame(b); ok && f.Key {
				t.current = RangeLimited
				if f.FullRange {
					t.current = RangeFull
				}
			}
		}
		return
	case H264, H265:
	default:
		return
	}
	for pos := 0; ; {
		i := bytes.Index(b[pos:], startCode3)
		if i < 0 || pos+i+3 >= len(b) {
			return
		}
		start := pos + i + 3
		var kind int
		if t.codec == H264 {
			kind = int(b[start] & 0x1F)
			if kind >= 1 && kind <= 5 {
				return
			}
		} else {
			kind = int(b[start] >> 1 & 0x3F)
			if kind < 32 {
				return
			}
		}
		end := len(b)
		if j := bytes.Index(b[start:], startCode3); j >= 0 {
			end = start + j
		}
		if (t.codec == H264 && kind == 7) || (t.codec == H265 && kind == 33) {
			nal := b[start:end]
			for len(nal) > 0 && nal[len(nal)-1] == 0 {
				nal = nal[:len(nal)-1]
			}
			t.parseSPS(nal)
		}
		pos = end
	}
}

// nalSPSRange reads the range from one SPS NAL unit.
func nalSPSRange(c Codec, nal []byte) ColorRange {
	t := colorTracker{codec: c}
	t.parseSPS(nal)
	return t.current
}

// StreamRange reports the color range declared by a compressed packet. It is
// RangeUnspecified when the packet does not say.
func StreamRange(c Codec, data []byte) ColorRange {
	t := newColorTracker(c)
	t.observe(data)
	return t.current
}

// colorDecoder labels decoded frames with the range their stream declares. It
// keeps H.264 pictures that follow a loss from the backend, gives JPEG images
// without Huffman tables the standard ones, and with
// VideoConfig.LowDelay it checks that pictures arrive in output order.
type colorDecoder struct {
	VideoDecoder
	tracker colorTracker
	guard   *reorderGuard  // nil unless low-delay H.264 or H.265
	gaps    *frameNumGuard // nil unless H.264
	jpeg    []byte         // a JPEG image completed with the standard tables
}

func (d *colorDecoder) Send(p Packet) error {
	d.tracker.observe(p.Data)
	if d.gaps != nil && !d.gaps.accept(p.Data) {
		return errRefLost
	}
	if d.tracker.codec == JPEG && JPEGNeedsTables(p.Data) {
		d.jpeg = AppendCompleteJPEG(d.jpeg[:0], p.Data)
		p.Data = d.jpeg
	}
	if d.guard != nil {
		d.guard.observe(p)
		// MediaCodec and Media Foundation decoders wait for reordering, and
		// their low-latency modes do not stop that. Every other backend can be
		// told directly. The emulator's goldfish decoder stops taking input
		// when a stream with B-frames claims none, so it is left to the guard.
		if n := d.VideoDecoder.Backend().Name; (strings.HasPrefix(n, "mediacodec") || n == "mediafoundation") && !strings.Contains(n, ".goldfish.") {
			p.Data = d.guard.statesNoReorder(p.Data)
		}
	}
	return d.VideoDecoder.Send(p)
}

func (d *colorDecoder) Receive() (*VideoFrame, error) {
	var f *VideoFrame
	var err error
	if d.guard != nil {
		f, err = d.guard.next(d.VideoDecoder)
	} else {
		f, err = d.VideoDecoder.Receive()
	}
	if err == nil {
		f.Range = d.tracker.current
	}
	return f, err
}

// ReorderStats implements ReorderReporter.
func (d *colorDecoder) ReorderStats() ReorderStats {
	if d.guard == nil {
		return ReorderStats{}
	}
	return d.guard.stats
}

func (d *colorDecoder) Close() error {
	if d.guard != nil {
		d.guard.close()
	}
	return d.VideoDecoder.Close()
}

// Wait implements Waiter.
func (d *colorDecoder) Wait(timeout time.Duration) {
	if w, ok := d.VideoDecoder.(Waiter); ok {
		w.Wait(timeout)
		return
	}
	time.Sleep(min(timeout, pollInterval))
}
