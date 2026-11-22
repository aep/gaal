//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"unsafe"
)

// H.265 encoding. The hardware does motion search, entropy coding and
// reconstruction. This file supplies parameters and packed headers. The stream
// is low latency: every frame references only the previous one, nothing is
// reordered, and sps_max_num_reorder_pics is zero so decoders output at once.

const (
	vaHEVCLog2PocLsb = 8
	vaHEVCNalTrailR  = 1
	vaHEVCInitQP     = 26
	vaHEVCLog2Ctb    = 6
)

// vaHEVCLevel picks the lowest Main tier level_idc that fits the stream.
func vaHEVCLevel(lumaPs, fps, bitrate int) int {
	for _, l := range [...]struct{ idc, ps, sps, kbps int }{
		{30, 36864, 552960, 128}, {60, 122880, 3686400, 1500}, {63, 245760, 7372800, 3000},
		{90, 552960, 16588800, 6000}, {93, 983040, 33177600, 10000},
		{120, 2228224, 66846720, 12000}, {123, 2228224, 133693440, 20000},
		{150, 8912896, 267386880, 25000}, {153, 8912896, 534773760, 40000},
		{156, 8912896, 1069547520, 60000}, {180, 35651584, 1069547520, 60000},
		{183, 35651584, 2139095040, 120000}, {186, 35651584, 4278190080, 240000},
	} {
		if lumaPs <= l.ps && lumaPs*fps <= l.sps && bitrate <= l.kbps*1000 {
			return l.idc
		}
	}
	return 186
}

// vaHEVCNAL returns a NAL unit with a four-byte start code.
func vaHEVCNAL(typ byte, body func(*vaBitWriter)) []byte {
	w := &vaBitWriter{out: []byte{0, 0, 0, 1, typ << 1, 1}}
	body(w)
	w.trailing()
	return w.out
}

type vaH265Encoder struct {
	s    *vaSession
	cfg  VideoConfig
	info BackendInfo

	surfW, surfH int
	ctbs         int
	level        int

	input    uint32
	recon    [2]uint32
	coded    uint32
	sinceIDR int
	poc      int32
	started  bool

	seq, pic, slice, rc, fr, hrd []byte
	rate                         vaRate
	vpsNAL, spsNAL, ppsNAL       []byte
	vpsParam, spsParam, ppsParam []byte
	sliceParam                   []byte
	sliceHdr                     vaBitWriter

	packets packetPool
	out     fifo[*EncodedPacket]
	flushed bool
	closed  bool
}

func newVAH265Encoder(cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaH265Profiles, vaEncEntries)
	if err != nil {
		return nil, err
	}
	var first error
	for _, d := range ds {
		e := &vaH265Encoder{s: newVASession(a, d), cfg: cfg, info: info}
		err := e.setup(vaPick(d, vaH265Profiles, vaEncEntries))
		if err == nil {
			return e, nil
		}
		e.Close()
		if first == nil {
			first = err
		}
	}
	return nil, first
}

func (e *vaH265Encoder) setup(ep vaEndpoint) error {
	s, cfg := e.s, e.cfg
	if cfg.Width < h265MinDim || cfg.Height < h265MinDim {
		return errH265Size
	}
	for _, q := range [...]struct {
		typ   uint32
		limit int
	}{{vaAttrMaxPicWidth, cfg.Width}, {vaAttrMaxPicHeight, cfg.Height}} {
		if v, err := s.attribute(ep, q.typ); err == nil && v != vaAttribNotSupported && q.limit > int(v) {
			return errH265Size
		}
	}
	rc, err := s.attribute(ep, vaAttrRateControl)
	if err != nil {
		return err
	}
	if rc == vaAttribNotSupported || rc&vaRCCBR == 0 {
		return errors.New("vaapi: driver has no constant bitrate mode")
	}
	packed, err := s.attribute(ep, vaAttrPackedHeaders)
	if err != nil {
		return err
	}
	const want = vaPackedSequence | vaPackedPicture | vaPackedSlice
	if packed == vaAttribNotSupported || packed&want != want {
		return errors.New("vaapi: driver does not take packed h265 headers")
	}
	if err := s.createConfig(ep, vaAttrRTFormat, vaRTYUV420, vaAttrRateControl, vaRCCBR, vaAttrPackedHeaders, want); err != nil {
		return err
	}
	// Coded sizes are multiples of the minimum coding block.
	e.surfW, e.surfH = (cfg.Width+7)&^7, (cfg.Height+7)&^7
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, e.surfW, e.surfH, 3); err != nil {
		return err
	}
	e.input, e.recon = s.surfaces[0], [2]uint32{s.surfaces[1], s.surfaces[2]}
	if err := s.createContext(e.surfW, e.surfH, vaProgressive); err != nil {
		return err
	}
	if e.coded, err = s.newBuffer(vaBufEncCoded, nil, max(e.surfW*e.surfH*3/2, 1<<16)); err != nil {
		return err
	}
	ctb := 1 << vaHEVCLog2Ctb
	e.ctbs = (e.surfW + ctb - 1) / ctb * ((e.surfH + ctb - 1) / ctb)
	e.level = vaHEVCLevel(e.surfW*e.surfH, cfg.FPS, cfg.Bitrate)
	e.buildParams()
	e.buildHeaders()
	return nil
}

func (e *vaH265Encoder) buildParams() {
	cfg := e.cfg
	seq := make([]byte, vaSizeHESeq)
	seq[vaoHEProfile] = 1
	seq[vaoHELevel] = byte(e.level)
	le32(seq, vaoHEIntra, uint32(cfg.KeyframeEvery))
	le32(seq, vaoHEIDR, uint32(cfg.KeyframeEvery))
	le32(seq, vaoHEIP, 1)
	le32(seq, vaoHEBps, uint32(cfg.Bitrate))
	vaPut16(seq, vaoHEW, uint16(e.surfW))
	vaPut16(seq, vaoHEH, uint16(e.surfH))
	le32(seq, vaoHEFields, 1<<vaBitHEChroma|1<<vaBitHESao|1<<vaBitHELowDelay)
	seq[vaoHEMinCb] = 0
	seq[vaoHEDiffCb] = vaHEVCLog2Ctb - 3
	seq[vaoHEMinTb] = 0
	seq[vaoHEDiffTb] = 3
	seq[vaoHEDepthInter] = 3
	seq[vaoHEDepthIntra] = 3
	seq[vaoHEVUI] = 1
	le32(seq, vaoHEVUIFields, 1<<vaBitHEVTiming)
	le32(seq, vaoHETick, 1)
	le32(seq, vaoHETimeScale, uint32(cfg.FPS))
	e.seq = seq

	e.pic = make([]byte, vaSizeHEPic)
	e.slice = make([]byte, vaSizeHESlice)

	// One frame of rate-control buffer keeps every frame near its budget, as
	// the other low-latency encoders here do.
	vbv := uint32(cfg.Bitrate / cfg.FPS)
	e.rc = make([]byte, vaoEMData+vaSizeERC)
	le32(e.rc, vaoEMType, vaMiscRateControl)
	rc := e.rc[vaoEMData:]
	le32(rc, vaoERBps, uint32(cfg.Bitrate))
	le32(rc, vaoERTarget, 100)
	le32(rc, vaoERWindow, uint32(1000/cfg.FPS)+1)
	le32(rc, vaoERFlags, 1<<vaBitERNoSkip|1<<vaBitERNoStuff)

	e.fr = make([]byte, vaoEMData+vaSizeEFR)
	le32(e.fr, vaoEMType, vaMiscFrameRate)
	le32(e.fr[vaoEMData:], vaoEFRate, uint32(cfg.FPS))

	e.hrd = make([]byte, vaoEMData+vaSizeEHRD)
	le32(e.hrd, vaoEMType, vaMiscHRD)
	le32(e.hrd[vaoEMData:], vaoEHFull, vbv)
	le32(e.hrd[vaoEMData:], vaoEHSize, vbv)
}

// profileTierLevel writes profile_tier_level(1, 0) for the Main profile.
func (e *vaH265Encoder) profileTierLevel(w *vaBitWriter) {
	w.u(2, 0) // general_profile_space
	w.u(1, 0) // general_tier_flag
	w.u(5, 1) // general_profile_idc
	w.u(32, 0x60000000)
	w.u(1, 1) // general_progressive_source_flag
	w.u(1, 0)
	w.u(1, 0)
	w.u(1, 1) // general_frame_only_constraint_flag
	w.u(32, 0)
	w.u(12, 0)
	w.u(8, uint32(e.level))
}

func (e *vaH265Encoder) buildHeaders() {
	cfg := e.cfg
	e.vpsNAL = vaHEVCNAL(h265NalVPS, func(w *vaBitWriter) {
		w.u(4, 0)
		w.u(1, 1) // vps_base_layer_internal_flag
		w.u(1, 1) // vps_base_layer_available_flag
		w.u(6, 0)
		w.u(3, 0)
		w.u(1, 1) // vps_temporal_id_nesting_flag
		w.u(16, 0xffff)
		e.profileTierLevel(w)
		w.u(1, 1) // vps_sub_layer_ordering_info_present_flag
		w.ue(1)   // vps_max_dec_pic_buffering_minus1
		w.ue(0)   // vps_max_num_reorder_pics
		w.ue(0)   // vps_max_latency_increase_plus1
		w.u(6, 0) // vps_max_layer_id
		w.ue(0)   // vps_num_layer_sets_minus1
		w.u(1, 0) // vps_timing_info_present_flag
		w.u(1, 0) // vps_extension_flag
	})
	e.spsNAL = vaHEVCNAL(h265NalSPS, func(w *vaBitWriter) {
		w.u(4, 0)
		w.u(3, 0)
		w.u(1, 1) // sps_temporal_id_nesting_flag
		e.profileTierLevel(w)
		w.ue(0) // sps_seq_parameter_set_id
		w.ue(1) // chroma_format_idc
		w.ue(uint32(e.surfW))
		w.ue(uint32(e.surfH))
		if e.surfW != cfg.Width || e.surfH != cfg.Height {
			w.u(1, 1)
			w.ue(0)
			w.ue(uint32(e.surfW-cfg.Width) / 2)
			w.ue(0)
			w.ue(uint32(e.surfH-cfg.Height) / 2)
		} else {
			w.u(1, 0)
		}
		w.ue(0)
		w.ue(0)
		w.ue(vaHEVCLog2PocLsb - 4)
		w.u(1, 1) // sps_sub_layer_ordering_info_present_flag
		w.ue(1)   // sps_max_dec_pic_buffering_minus1
		w.ue(0)   // sps_max_num_reorder_pics
		w.ue(0)   // sps_max_latency_increase_plus1
		w.ue(0)   // log2_min_luma_coding_block_size_minus3
		w.ue(vaHEVCLog2Ctb - 3)
		w.ue(0)   // log2_min_luma_transform_block_size_minus2
		w.ue(3)   // log2_diff_max_min_luma_transform_block_size
		w.ue(3)   // max_transform_hierarchy_depth_inter
		w.ue(3)   // max_transform_hierarchy_depth_intra
		w.u(1, 0) // scaling_list_enabled_flag
		w.u(1, 0) // amp_enabled_flag
		w.u(1, 1) // sample_adaptive_offset_enabled_flag
		w.u(1, 0) // pcm_enabled_flag
		w.ue(1)   // num_short_term_ref_pic_sets
		w.ue(1)   // num_negative_pics
		w.ue(0)   // num_positive_pics
		w.ue(0)   // delta_poc_s0_minus1
		w.u(1, 1) // used_by_curr_pic_s0_flag
		w.u(1, 0) // long_term_ref_pics_present_flag
		w.u(1, 0) // sps_temporal_mvp_enabled_flag
		w.u(1, 0) // strong_intra_smoothing_enabled_flag
		w.u(1, 1) // vui_parameters_present_flag
		w.u(1, 0) // aspect_ratio_info_present_flag
		w.u(1, 0) // overscan_info_present_flag
		w.videoSignal(cfg.Range)
		w.u(1, 0) // chroma_loc_info_present_flag
		w.u(1, 0) // neutral_chroma_indication_flag
		w.u(1, 0) // field_seq_flag
		w.u(1, 0) // frame_field_info_present_flag
		w.u(1, 0) // default_display_window_flag
		w.u(1, 1) // vui_timing_info_present_flag
		w.u(32, 1)
		w.u(32, uint32(cfg.FPS))
		w.u(1, 0) // vui_poc_proportional_to_timing_flag
		w.u(1, 0) // vui_hrd_parameters_present_flag
		w.u(1, 0) // bitstream_restriction_flag
		w.u(1, 0) // sps_extension_present_flag
	})
	e.ppsNAL = vaHEVCNAL(h265NalPPS, func(w *vaBitWriter) {
		w.ue(0)
		w.ue(0)
		w.u(1, 0) // dependent_slice_segments_enabled_flag
		w.u(1, 0) // output_flag_present_flag
		w.u(3, 0) // num_extra_slice_header_bits
		w.u(1, 0) // sign_data_hiding_enabled_flag
		w.u(1, 0) // cabac_init_present_flag
		w.ue(0)
		w.ue(0)
		w.se(vaHEVCInitQP - 26)
		w.u(1, 0) // constrained_intra_pred_flag
		w.u(1, 0) // transform_skip_enabled_flag
		w.u(1, 1) // cu_qp_delta_enabled_flag
		w.ue(0)   // diff_cu_qp_delta_depth
		w.se(0)
		w.se(0)
		w.u(1, 0) // pps_slice_chroma_qp_offsets_present_flag
		w.u(1, 0) // weighted_pred_flag
		w.u(1, 0) // weighted_bipred_flag
		w.u(1, 0) // transquant_bypass_enabled_flag
		w.u(1, 0) // tiles_enabled_flag
		w.u(1, 0) // entropy_coding_sync_enabled_flag
		w.u(1, 0) // pps_loop_filter_across_slices_enabled_flag
		w.u(1, 0) // deblocking_filter_control_present_flag
		w.u(1, 0) // pps_scaling_list_data_present_flag
		w.u(1, 0) // lists_modification_present_flag
		w.ue(0)   // log2_parallel_merge_level_minus2
		w.u(1, 0) // slice_segment_header_extension_present_flag
		w.u(1, 0) // pps_extension_present_flag
	})
	param := func(typ uint32, nal []byte) []byte {
		p := make([]byte, vaSizeEPH)
		le32(p, vaoEHType, typ)
		le32(p, vaoEHBits, uint32(len(nal)*8))
		p[vaoEHEmu] = 1
		return p
	}
	e.vpsParam = param(vaPackedTypeHEVCVPS, e.vpsNAL)
	e.spsParam = param(vaPackedTypeHEVCSPS, e.spsNAL)
	e.ppsParam = param(vaPackedTypeHEVCPPS, e.ppsNAL)
	e.sliceParam = param(vaPackedTypeHEVCSlice, nil)
	e.sliceHdr.out = make([]byte, 0, 64)
}

// queueSliceHeader writes the slice segment header. The driver takes the values
// it needs from here and has the hardware write the final header.
func (e *vaH265Encoder) queueSliceHeader(idr bool) error {
	w := &e.sliceHdr
	if idr {
		w.reset(0, 0, 0, 1, h265NalIDRW<<1, 1)
	} else {
		w.reset(0, 0, 0, 1, vaHEVCNalTrailR<<1, 1)
	}
	w.u(1, 1) // first_slice_segment_in_pic_flag
	if idr {
		w.u(1, 0) // no_output_of_prior_pics_flag
	}
	w.ue(0) // slice_pic_parameter_set_id
	if idr {
		w.ue(h265SliceI)
	} else {
		w.ue(h265SliceP)
		w.u(vaHEVCLog2PocLsb, uint32(e.poc)&(1<<vaHEVCLog2PocLsb-1))
		w.u(1, 1) // short_term_ref_pic_set_sps_flag
	}
	w.u(1, 1) // slice_sao_luma_flag
	w.u(1, 1) // slice_sao_chroma_flag
	if !idr {
		w.u(1, 0) // num_ref_idx_active_override_flag
		w.ue(0)   // five_minus_max_num_merge_cand
	}
	w.se(0) // slice_qp_delta
	le32(e.sliceParam, vaoEHBits, uint32(w.flushPartial()))
	return e.queuePacked(e.sliceParam, w.out)
}

func vaPutHEVCEnc(b []byte, off int, surface uint32, poc int32, flags uint32) {
	le32(b, off+vaoHID, surface)
	le32(b, off+vaoHPoc, uint32(poc))
	le32(b, off+vaoHFlags, flags)
}

func (e *vaH265Encoder) Send(f *VideoFrame) error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	s := e.s
	if err := s.uploadFrame(e.input, f, e.surfW, e.surfH); err != nil {
		return err
	}
	idr := !e.started || f.ForceKeyframe || e.sinceIDR >= e.cfg.KeyframeEvery
	if idr {
		e.poc, e.sinceIDR = 0, 0
	}
	if err := e.queue(idr); err != nil {
		s.dropBuffers()
		return err
	}
	if err := s.submit(e.input); err != nil {
		return err
	}
	if err := s.sync(e.input); err != nil {
		return err
	}
	if err := e.collect(f, idr); err != nil {
		return err
	}
	e.started = true
	e.recon[0], e.recon[1] = e.recon[1], e.recon[0]
	e.poc++
	e.sinceIDR++
	return nil
}

func (e *vaH265Encoder) queue(idr bool) error {
	s := e.s
	if idr {
		if err := s.addBuffer(vaBufEncSeq, unsafe.Pointer(&e.seq[0]), len(e.seq)); err != nil {
			return err
		}
		for _, m := range [...][]byte{e.rc, e.fr, e.hrd} {
			if err := s.addBuffer(vaBufEncMisc, unsafe.Pointer(&m[0]), len(m)); err != nil {
				return err
			}
		}
		if err := e.queuePacked(e.vpsParam, e.vpsNAL); err != nil {
			return err
		}
		if err := e.queuePacked(e.spsParam, e.spsNAL); err != nil {
			return err
		}
		if err := e.queuePacked(e.ppsParam, e.ppsNAL); err != nil {
			return err
		}
	}
	if err := e.rate.queue(s, idr); err != nil {
		return err
	}
	p := e.pic
	clear(p)
	vaPutHEVCEnc(p, vaoHEPCurr, e.recon[0], e.poc, 0)
	first := 0
	if !idr {
		vaPutHEVCEnc(p, vaoHEPRefs, e.recon[1], e.poc-1, vaPicHEVCBefore)
		first = 1
	}
	for i := first; i < 15; i++ {
		vaPutHEVCEnc(p, vaoHEPRefs+i*vaSizePicHEVC, vaInvalidID, 0, vaPicHEVCInvalid)
	}
	le32(p, vaoHEPCoded, e.coded)
	p[vaoHEPCollocated] = 0xff
	p[vaoHEPInitQP] = vaHEVCInitQP
	coding := uint32(2) // P
	nal := byte(vaHEVCNalTrailR)
	fields := uint32(1<<vaBitHEPRef | 1<<vaBitHEPCuQp)
	if idr {
		coding, nal = 1, h265NalIDRW
		fields |= 1 << vaBitHEPIDR
	}
	p[vaoHEPNal] = nal
	le32(p, vaoHEPFields, fields|coding<<vaBitHEPCoding)
	if err := s.addBuffer(vaBufEncPic, unsafe.Pointer(&p[0]), len(p)); err != nil {
		return err
	}
	if err := e.queueSliceHeader(idr); err != nil {
		return err
	}
	l := e.slice
	clear(l)
	le32(l, vaoHELNum, uint32(e.ctbs))
	l[vaoHELType] = h265SliceP
	if idr {
		l[vaoHELType] = h265SliceI
	}
	for i := 0; i < 15; i++ {
		vaPutHEVCEnc(l, vaoHELList0+i*vaSizePicHEVC, vaInvalidID, 0, vaPicHEVCInvalid)
		vaPutHEVCEnc(l, vaoHELList1+i*vaSizePicHEVC, vaInvalidID, 0, vaPicHEVCInvalid)
	}
	if !idr {
		vaPutHEVCEnc(l, vaoHELList0, e.recon[1], e.poc-1, vaPicHEVCBefore)
	}
	l[vaoHELMerge] = 5
	le32(l, vaoHELFields, 1<<vaBitHELLast|1<<vaBitHELSaoY|1<<vaBitHELSaoC)
	return s.addBuffer(vaBufEncSlice, unsafe.Pointer(&l[0]), len(l))
}

func (e *vaH265Encoder) queuePacked(param, nal []byte) error {
	if err := e.s.addBuffer(vaBufEncPackedParam, unsafe.Pointer(&param[0]), len(param)); err != nil {
		return err
	}
	return e.s.addBuffer(vaBufEncPackedData, unsafe.Pointer(&nal[0]), len(nal))
}

// vaHasVPS reports whether a VPS comes before the first slice.
func vaHasVPS(b []byte) bool {
	for nal, rest := vaNextNAL(b); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) == 0 {
			continue
		}
		t := nal[0] >> 1 & 63
		if t == h265NalVPS {
			return true
		}
		if t < h265NalVPS {
			return false
		}
	}
	return false
}

// collect copies the coded segments into a pooled packet.
func (e *vaH265Encoder) collect(f *VideoFrame, idr bool) error {
	s := e.s
	head, err := s.mapBuf(e.coded, vaMapRead)
	if err != nil {
		return err
	}
	defer s.unmapBuf(e.coded)
	n := 0
	for seg := head; seg != 0; seg = rdPtr(cMem(seg, vaSizeCodedSeg), vaoCSNext) {
		n += int(rd32(cMem(seg, vaSizeCodedSeg), vaoCSSize))
	}
	if n == 0 {
		return errors.New("vaapi: encoder produced no data")
	}
	var prefix int
	if idr {
		// Drivers normally emit the packed headers. If this one did not, the
		// parameter sets are added here.
		h := cMem(head, vaSizeCodedSeg)
		first := cMem(rdPtr(h, vaoCSBuf), int(rd32(h, vaoCSSize)))
		if !vaHasVPS(first) {
			prefix = len(e.vpsNAL) + len(e.spsNAL) + len(e.ppsNAL)
		}
	}
	pkt := e.packets.get(prefix + n)
	at := 0
	if prefix != 0 {
		at = copy(pkt.Data, e.vpsNAL)
		at += copy(pkt.Data[at:], e.spsNAL)
		at += copy(pkt.Data[at:], e.ppsNAL)
	}
	for seg := head; seg != 0; {
		h := cMem(seg, vaSizeCodedSeg)
		at += copy(pkt.Data[at:], cMem(rdPtr(h, vaoCSBuf), int(rd32(h, vaoCSSize))))
		seg = rdPtr(h, vaoCSNext)
	}
	pkt.PTS, pkt.Keyframe = f.PTS, idr
	e.out.push(pkt)
	return nil
}

func (e *vaH265Encoder) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *vaH265Encoder) Flush() error {
	e.flushed = true
	return nil
}

func (e *vaH265Encoder) Backend() BackendInfo { return e.info }

func (e *vaH265Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	s := e.s
	if e.coded != 0 {
		s.c.C2(s.a.destroyBuffer, s.dpy, uintptr(e.coded))
		e.coded = 0
	}
	s.close()
	for {
		p, ok := e.out.pop()
		if !ok {
			return nil
		}
		p.Release()
	}
}
