//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"unsafe"
)

// H.264 encoding. The hardware does motion search, entropy coding and
// reconstruction. This file supplies parameters and, when the driver accepts
// them, the SPS and PPS, which carry the reordering limits that let decoders
// output immediately. Every frame references only the previous one.

const (
	vaEncLog2FrameNum = 8
	vaEncInitQP       = 26
)

// vaBitWriter builds an RBSP, inserting emulation prevention bytes as it goes.
type vaBitWriter struct {
	out   []byte
	cur   uint32
	n     int
	zeros int
}

func (w *vaBitWriter) raw(b byte) {
	if w.zeros >= 2 && b <= 3 {
		w.out = append(w.out, 3)
		w.zeros = 0
	}
	w.out = append(w.out, b)
	if b == 0 {
		w.zeros++
	} else {
		w.zeros = 0
	}
}

func (w *vaBitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		w.cur = w.cur<<1 | v>>uint(i)&1
		w.n++
		if w.n == 8 {
			w.raw(byte(w.cur))
			w.cur, w.n = 0, 0
		}
	}
}

func (w *vaBitWriter) ue(v uint32) {
	v++
	n := 0
	for t := v; t > 1; t >>= 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v)
}

func (w *vaBitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

// flushPartial pads to a byte boundary without a stop bit and returns the
// number of meaningful bits written, for slice headers that continue in the
// hardware's output.
func (w *vaBitWriter) flushPartial() int {
	pad := 0
	if w.n != 0 {
		pad = 8 - w.n
		w.u(pad, 0)
	}
	return len(w.out)*8 - pad
}

func (w *vaBitWriter) reset(prefix ...byte) {
	w.out = append(w.out[:0], prefix...)
	w.cur, w.n, w.zeros = 0, 0, 0
}

func (w *vaBitWriter) trailing() {
	w.u(1, 1)
	for w.n != 0 {
		w.u(1, 0)
	}
}

// vaNAL returns the NAL unit with a four-byte start code.
func vaNAL(header byte, body func(*vaBitWriter)) []byte {
	w := &vaBitWriter{out: []byte{0, 0, 0, 1, header}}
	body(w)
	w.trailing()
	return w.out
}

// vaLevel picks the lowest level that fits the stream.
func vaLevel(mbs, fps, bitrate int) int {
	for _, l := range [...]struct{ idc, mbps, fs, kbps int }{
		{10, 1485, 99, 64}, {11, 3000, 396, 192}, {12, 6000, 396, 384}, {13, 11880, 396, 768},
		{20, 11880, 396, 2000}, {21, 19800, 792, 4000}, {22, 20250, 1620, 4000},
		{30, 40500, 1620, 10000}, {31, 108000, 3600, 14000}, {32, 216000, 5120, 20000},
		{40, 245760, 8192, 20000}, {41, 245760, 8192, 50000}, {42, 522240, 8704, 50000},
		{50, 589824, 22080, 135000}, {51, 983040, 36864, 240000}, {52, 2073600, 36864, 240000},
	} {
		if mbs <= l.fs && mbs*fps <= l.mbps && bitrate <= l.kbps*1000 {
			return l.idc
		}
	}
	return 52
}

type vaH264Encoder struct {
	s    *vaSession
	cfg  VideoConfig
	info BackendInfo

	w16, h16, mbW, mbH int
	profileIDC         int
	level              int
	cabac              bool
	packed             bool
	packedSlice        bool

	input    uint32
	recon    [2]uint32
	coded    uint32
	frameNum uint32
	sinceIDR int
	poc      int32
	idrID    uint32
	started  bool

	seq, pic, slice, rc, fr, hrd []byte
	rate                         vaRate
	spsNAL, ppsNAL               []byte
	spsParam, ppsParam           []byte
	sliceParam                   []byte
	sliceHdr                     vaBitWriter

	packets packetPool
	out     fifo[*EncodedPacket]
	flushed bool
	closed  bool
}

func newVAH264Encoder(cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaH264Profiles, vaEncEntries)
	if err != nil {
		return nil, err
	}
	var first error
	for _, d := range ds {
		e := &vaH264Encoder{s: newVASession(a, d), cfg: cfg, info: info}
		err := e.setup(vaPick(d, vaH264Profiles, vaEncEntries))
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

func (e *vaH264Encoder) setup(ep vaEndpoint) error {
	s, cfg := e.s, e.cfg
	rc, err := s.attribute(ep, vaAttrRateControl)
	if err != nil {
		return err
	}
	if rc == vaAttribNotSupported || rc&vaRCCBR == 0 {
		return errors.New("vaapi: driver has no constant bitrate mode")
	}
	attrs := []uint32{vaAttrRTFormat, vaRTYUV420, vaAttrRateControl, vaRCCBR}
	packed, err := s.attribute(ep, vaAttrPackedHeaders)
	if err != nil {
		return err
	}
	const want = vaPackedSequence | vaPackedPicture
	if packed != vaAttribNotSupported && packed&want == want {
		e.packed = true
		mask := uint32(want)
		// radeonsi takes nal_unit_type and nal_ref_idc only from a packed slice
		// header, and writes zero for both without one.
		if packed&vaPackedSlice != 0 {
			e.packedSlice = true
			mask |= vaPackedSlice
		}
		attrs = append(attrs, vaAttrPackedHeaders, mask)
	}
	if err := s.createConfig(ep, attrs...); err != nil {
		return err
	}
	e.w16, e.h16 = (cfg.Width+15)&^15, (cfg.Height+15)&^15
	e.mbW, e.mbH = e.w16/16, e.h16/16
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, e.w16, e.h16, 3); err != nil {
		return err
	}
	e.input, e.recon = s.surfaces[0], [2]uint32{s.surfaces[1], s.surfaces[2]}
	if err := s.createContext(e.w16, e.h16, vaProgressive); err != nil {
		return err
	}
	// Worst case for one frame, well above any rate-controlled result.
	if e.coded, err = s.newBuffer(vaBufEncCoded, nil, max(e.w16*e.h16*3/2, 1<<16)); err != nil {
		return err
	}
	switch ep.profile {
	case vaProfileH264High:
		e.profileIDC, e.cabac = 100, true
	case vaProfileH264Main:
		e.profileIDC, e.cabac = 77, true
	default:
		e.profileIDC = 66
	}
	e.level = vaLevel(e.mbW*e.mbH, cfg.FPS, cfg.Bitrate)
	e.buildParams()
	e.buildHeaders()
	return nil
}

func (e *vaH264Encoder) buildParams() {
	cfg := e.cfg
	seq := make([]byte, vaSizeESeq)
	seq[vaoESLevel] = byte(e.level)
	le32(seq, vaoESIntra, uint32(cfg.KeyframeEvery))
	le32(seq, vaoESIDR, uint32(cfg.KeyframeEvery))
	le32(seq, vaoESIP, 1)
	le32(seq, vaoESBps, uint32(cfg.Bitrate))
	le32(seq, vaoESMaxRef, 1)
	vaPut16(seq, vaoESW, uint16(e.mbW))
	vaPut16(seq, vaoESH, uint16(e.mbH))
	le32(seq, vaoESFields, 1<<vaBitESChroma|1<<vaBitESFrameMbs|1<<vaBitESDirect8|
		(vaEncLog2FrameNum-4)<<vaBitESFrameNum|2<<vaBitESPocType)
	if e.w16 != cfg.Width || e.h16 != cfg.Height {
		seq[vaoESCrop] = 1
		le32(seq, vaoESCropR, uint32(e.w16-cfg.Width)/2)
		le32(seq, vaoESCropB, uint32(e.h16-cfg.Height)/2)
	}
	seq[vaoESVUI] = 1
	le32(seq, vaoESVUIFields, 1<<vaBitEVTiming|1<<vaBitEVRestrict|1<<vaBitEVOverPic|
		15<<vaBitEVMvH|15<<vaBitEVMvV)
	le32(seq, vaoESTick, 1)
	le32(seq, vaoESTimeScale, uint32(2*cfg.FPS))
	e.seq = seq

	e.pic = make([]byte, vaSizeEPic)
	e.slice = make([]byte, vaSizeESlice)

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

func (e *vaH264Encoder) buildHeaders() {
	cfg := e.cfg
	e.spsNAL = vaNAL(0x67, func(w *vaBitWriter) {
		w.u(8, uint32(e.profileIDC))
		switch e.profileIDC {
		case 66:
			w.u(8, 0xC0) // constrained baseline
		case 77:
			w.u(8, 0x40)
		default:
			w.u(8, 0)
		}
		w.u(8, uint32(e.level))
		w.ue(0)
		if e.profileIDC == 100 {
			w.ue(1) // 4:2:0
			w.ue(0)
			w.ue(0)
			w.u(1, 0)
			w.u(1, 0)
		}
		w.ue(vaEncLog2FrameNum - 4)
		w.ue(2) // POC follows decode order, so no decoder needs to reorder
		w.ue(1) // max_num_ref_frames
		w.u(1, 0)
		w.ue(uint32(e.mbW - 1))
		w.ue(uint32(e.mbH - 1))
		w.u(1, 1) // frame_mbs_only_flag
		w.u(1, 1) // direct_8x8_inference_flag
		if e.w16 != cfg.Width || e.h16 != cfg.Height {
			w.u(1, 1)
			w.ue(0)
			w.ue(uint32(e.w16-cfg.Width) / 2)
			w.ue(0)
			w.ue(uint32(e.h16-cfg.Height) / 2)
		} else {
			w.u(1, 0)
		}
		w.u(1, 1) // vui_parameters_present_flag
		w.u(1, 0) // aspect ratio
		w.u(1, 0) // overscan
		w.videoSignal(cfg.Range)
		w.u(1, 0) // chroma location
		w.u(1, 1) // timing
		w.u(32, 1)
		w.u(32, uint32(2*cfg.FPS))
		w.u(1, 0)
		w.u(1, 0) // NAL HRD
		w.u(1, 0) // VCL HRD
		w.u(1, 0) // pic_struct
		w.u(1, 1) // bitstream_restriction_flag
		w.u(1, 1)
		w.ue(0)
		w.ue(0)
		w.ue(15)
		w.ue(15)
		w.ue(0) // max_num_reorder_frames
		w.ue(1) // max_dec_frame_buffering
	})
	e.ppsNAL = vaNAL(0x68, func(w *vaBitWriter) {
		w.ue(0)
		w.ue(0)
		c := uint32(0)
		if e.cabac {
			c = 1
		}
		w.u(1, c)
		w.u(1, 0)
		w.ue(0) // one slice group
		w.ue(0)
		w.ue(0)
		w.u(1, 0)
		w.u(2, 0)
		w.se(vaEncInitQP - 26)
		w.se(0)
		w.se(0)
		w.u(1, 1) // deblocking_filter_control_present_flag
		w.u(1, 0)
		w.u(1, 0)
	})
	param := func(typ uint32, nal []byte) []byte {
		p := make([]byte, vaSizeEPH)
		le32(p, vaoEHType, typ)
		le32(p, vaoEHBits, uint32(len(nal)*8))
		p[vaoEHEmu] = 1
		return p
	}
	e.spsParam, e.ppsParam = param(vaPackedTypeSPS, e.spsNAL), param(vaPackedTypePPS, e.ppsNAL)
	e.sliceParam = param(vaPackedTypeSlice, nil)
	e.sliceHdr.out = make([]byte, 0, 64)
}

// queueSliceHeader writes the slice header for the driver to place before the
// slice data. It stops mid-byte: the hardware continues from that bit.
func (e *vaH264Encoder) queueSliceHeader(idr bool) error {
	w := &e.sliceHdr
	if idr {
		w.reset(0, 0, 0, 1, 0x65)
	} else {
		w.reset(0, 0, 0, 1, 0x41)
	}
	w.ue(0) // first_mb_in_slice
	if idr {
		w.ue(7)
	} else {
		w.ue(5)
	}
	w.ue(0) // pic_parameter_set_id
	w.u(vaEncLog2FrameNum, e.frameNum)
	if idr {
		w.ue(e.idrID)
		w.u(1, 0) // no_output_of_prior_pics_flag
		w.u(1, 0) // long_term_reference_flag
	} else {
		w.u(1, 0) // num_ref_idx_active_override_flag
		w.u(1, 0) // ref_pic_list_modification_flag_l0
		w.u(1, 0) // adaptive_ref_pic_marking_mode_flag
		if e.cabac {
			w.ue(0) // cabac_init_idc
		}
	}
	w.se(0) // slice_qp_delta
	w.ue(0) // disable_deblocking_filter_idc
	w.se(0) // slice_alpha_c0_offset_div2
	w.se(0) // slice_beta_offset_div2
	le32(e.sliceParam, vaoEHBits, uint32(w.flushPartial()))
	return e.queuePacked(e.sliceParam, w.out)
}

// vaPutEncPic writes a VAPictureH264 for a reconstructed surface.
func vaPutEncPic(b []byte, off int, surface uint32, frameNum uint32, flags uint32, poc int32) {
	le32(b, off+vaoPID, surface)
	le32(b, off+vaoPFrameIdx, frameNum)
	le32(b, off+vaoPFlags, flags)
	le32(b, off+vaoPTop, uint32(poc))
	le32(b, off+vaoPBottom, uint32(poc))
}

// upload copies f into the input surface.
func (e *vaH264Encoder) upload(f *VideoFrame) error {
	return e.s.uploadFrame(e.input, f, e.w16, e.h16)
}

// uploadFrame copies f into an encoder input surface of w by h and extends its
// edges over the padding, so the padding costs few bits.
func (s *vaSession) uploadFrame(surface uint32, f *VideoFrame, w, h int) error {
	if !s.derive(surface) {
		return s.uploadViaImage(surface, f, w, h)
	}
	img := s.image
	id, buf := rd32(img, vaoImgID), rd32(img, vaoImgBuf)
	addr, err := s.mapBuf(buf, vaMapWrite)
	if err == nil {
		err = vaFillSurface(addr, img, f, w, h)
		s.unmapBuf(buf)
	}
	s.c.C2(s.a.destroyImage, s.dpy, uintptr(id))
	return err
}

func vaFillSurface(addr uintptr, img []byte, f *VideoFrame, surfW, surfH int) error {
	size := int(rd32(img, vaoImgSize))
	yp, uvp := int(rd32(img, vaoImgPitches)), int(rd32(img, vaoImgPitches+4))
	yo, uvo := int(rd32(img, vaoImgOffsets)), int(rd32(img, vaoImgOffsets+4))
	if yo+(surfH-1)*yp+surfW > size || uvo+(surfH/2-1)*uvp+surfW > size {
		return errVAImageLayout
	}
	mem := cMem(addr, size)
	y, uv := mem[yo:], mem[uvo:]
	w, h := f.Width, f.Height
	copyPlane(y, yp, f.Y, f.YStride, w, h)
	if f.Format == NV12 {
		copyPlane(uv, uvp, f.U, f.UVStride, w, h/2)
	} else {
		for r := 0; r < h/2; r++ {
			row := uv[r*uvp : r*uvp+w]
			u := f.U[r*f.UVStride : r*f.UVStride+w/2]
			v := f.V[r*f.UVStride : r*f.UVStride+w/2]
			interleaveUV(row, u, v)
		}
	}
	if surfW != w {
		for r := 0; r < h; r++ {
			row := y[r*yp : r*yp+surfW]
			for x := w; x < surfW; x++ {
				row[x] = row[w-1]
			}
		}
		for r := 0; r < h/2; r++ {
			row := uv[r*uvp : r*uvp+surfW]
			for x := w; x < surfW; x += 2 {
				row[x], row[x+1] = row[w-2], row[w-1]
			}
		}
	}
	for r := h; r < surfH; r++ {
		copy(y[r*yp:r*yp+surfW], y[(h-1)*yp:(h-1)*yp+surfW])
	}
	for r := h / 2; r < surfH/2; r++ {
		copy(uv[r*uvp:r*uvp+surfW], uv[(h/2-1)*uvp:(h/2-1)*uvp+surfW])
	}
	return nil
}

// uploadViaImage serves drivers that cannot map surfaces. vaPutImage takes more
// arguments than dl.Caller supports, so this path allocates.
func (s *vaSession) uploadViaImage(surface uint32, f *VideoFrame, w, h int) error {
	if s.readImage == nil {
		img := make([]byte, vaSizeImage)
		if err := s.check("create image", s.c.C5(s.a.createImage, s.dpy, uintptr(unsafe.Pointer(&s.imageFmt[0])),
			uintptr(w), uintptr(h), uintptr(unsafe.Pointer(&img[0])))); err != nil {
			return err
		}
		s.readImage = img
	}
	img := s.readImage
	buf := rd32(img, vaoImgBuf)
	addr, err := s.mapBuf(buf, vaMapWrite)
	if err != nil {
		return err
	}
	err = vaFillSurface(addr, img, f, w, h)
	s.unmapBuf(buf)
	if err != nil {
		return err
	}
	return vaPutImage(s, surface, rd32(img, vaoImgID), w, h)
}

func (e *vaH264Encoder) Send(f *VideoFrame) error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	if err := e.upload(f); err != nil {
		return err
	}
	idr := !e.started || f.ForceKeyframe || e.sinceIDR >= e.cfg.KeyframeEvery
	if idr {
		e.frameNum, e.poc, e.sinceIDR = 0, 0, 0
	}
	if err := e.queue(idr); err != nil {
		e.s.dropBuffers()
		return err
	}
	s := e.s
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
	e.frameNum = (e.frameNum + 1) & (1<<vaEncLog2FrameNum - 1)
	e.poc += 2
	e.sinceIDR++
	if idr {
		e.idrID = (e.idrID + 1) & 0xffff
	}
	return nil
}

func (e *vaH264Encoder) queue(idr bool) error {
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
		if e.packed {
			if err := e.queuePacked(e.spsParam, e.spsNAL); err != nil {
				return err
			}
			if err := e.queuePacked(e.ppsParam, e.ppsNAL); err != nil {
				return err
			}
		}
	}
	if err := e.rate.queue(s, idr); err != nil {
		return err
	}
	p := e.pic
	clear(p)
	vaPutEncPic(p, vaoEPCurr, e.recon[0], e.frameNum, 0, e.poc)
	first := 0
	if !idr {
		vaPutEncPic(p, vaoEPRefs, e.recon[1], (e.frameNum-1)&(1<<vaEncLog2FrameNum-1), vaPicShortRef, e.poc-2)
		first = 1
	}
	for i := first; i < 16; i++ {
		vaPutInvalid(p, vaoEPRefs+i*vaSizePicH264)
	}
	le32(p, vaoEPCoded, e.coded)
	vaPut16(p, vaoEPFrameNum, uint16(e.frameNum))
	p[vaoEPQP] = vaEncInitQP
	fields := uint32(1<<vaBitEPRef | 1<<vaBitEPDeblock)
	if idr {
		fields |= 1 << vaBitEPIDR
	}
	if e.cabac {
		fields |= 1 << vaBitEPEntropy
	}
	le32(p, vaoEPFields, fields)
	if err := s.addBuffer(vaBufEncPic, unsafe.Pointer(&p[0]), len(p)); err != nil {
		return err
	}
	if e.packedSlice {
		if err := e.queueSliceHeader(idr); err != nil {
			return err
		}
	}
	l := e.slice
	clear(l)
	le32(l, vaoELNum, uint32(e.mbW*e.mbH))
	le32(l, vaoELInfo, vaInvalidID)
	if idr {
		l[vaoELType] = 2
		vaPut16(l, vaoELIDR, uint16(e.idrID))
	}
	for i := 0; i < 32; i++ {
		vaPutInvalid(l, vaoELList0+i*vaSizePicH264)
		vaPutInvalid(l, vaoELList1+i*vaSizePicH264)
	}
	if !idr {
		vaPutEncPic(l, vaoELList0, e.recon[1], (e.frameNum-1)&(1<<vaEncLog2FrameNum-1), vaPicShortRef, e.poc-2)
	}
	return s.addBuffer(vaBufEncSlice, unsafe.Pointer(&l[0]), len(l))
}

func (e *vaH264Encoder) queuePacked(param, nal []byte) error {
	if err := e.s.addBuffer(vaBufEncPackedParam, unsafe.Pointer(&param[0]), len(param)); err != nil {
		return err
	}
	return e.s.addBuffer(vaBufEncPackedData, unsafe.Pointer(&nal[0]), len(nal))
}

// vaHasSPS reports whether an SPS comes before the first slice.
func vaHasSPS(b []byte) bool {
	for nal, rest := vaNextNAL(b); nal != nil || rest != nil; nal, rest = vaNextNAL(rest) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 31 {
		case 7:
			return true
		case 1, 5:
			return false
		}
	}
	return false
}

// collect copies the coded segments into a pooled packet.
func (e *vaH264Encoder) collect(f *VideoFrame, idr bool) error {
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
		// Drivers without packed headers normally write their own. If this one
		// did not, the parameter sets are added here.
		h := cMem(head, vaSizeCodedSeg)
		first := cMem(rdPtr(h, vaoCSBuf), int(rd32(h, vaoCSSize)))
		if !vaHasSPS(first) {
			prefix = len(e.spsNAL) + len(e.ppsNAL)
		}
	}
	pkt := e.packets.get(prefix + n)
	at := 0
	if prefix != 0 {
		at = copy(pkt.Data, e.spsNAL)
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

func (e *vaH264Encoder) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *vaH264Encoder) Flush() error {
	e.flushed = true
	return nil
}

func (e *vaH264Encoder) Backend() BackendInfo { return e.info }

func (e *vaH264Encoder) Close() error {
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
