//go:build windows && (amd64 || arm64)

package gaal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	"unsafe"
)

var (
	errMFFlushed  = fmt.Errorf("%w: stream is flushed", ErrInvalid)
	errMFEmpty    = fmt.Errorf("%w: empty packet", ErrInvalid)
	errMFBadH264  = fmt.Errorf("%w: no H.264 NAL unit in packet", ErrCorrupt)
	errMFBadH265  = fmt.Errorf("%w: no H.265 NAL unit in packet", ErrCorrupt)
	errMFBadVP9   = fmt.Errorf("%w: malformed VP9 frame", ErrCorrupt)
	errMFNeedVP9  = fmt.Errorf("%w: VP9 stream must start with a keyframe", ErrCorrupt)
	errMFBadJPEG  = fmt.Errorf("%w: not a JPEG image", ErrCorrupt)
	errMFBadVP8   = fmt.Errorf("%w: malformed VP8 frame", ErrCorrupt)
	errMFNeedKey  = fmt.Errorf("%w: VP8 stream must start with a keyframe", ErrCorrupt)
	mfStartCode3  = []byte{0, 0, 1}
	errMFNoFormat = errors.New("mediafoundation: transform offers no usable output format")
)

// --- Bitstream helpers ---

// mfNALMask returns a bit for each NAL unit type in an Annex B buffer.
func mfNALMask(b []byte) uint32 {
	var mask uint32
	for i := 0; ; {
		n := bytes.Index(b[i:], mfStartCode3)
		if n < 0 || i+n+3 >= len(b) {
			return mask
		}
		i += n + 3
		mask |= 1 << (b[i] & 31)
	}
}

func mfHasStartCode(b []byte) bool {
	return bytes.HasPrefix(b, mfStartCode3) || bytes.HasPrefix(b, []byte{0, 0, 0, 1})
}

// mfValidH264 reports whether b holds at least one plausible NAL unit.
func mfValidH264(b []byte) bool {
	for i := 0; ; {
		n := bytes.Index(b[i:], mfStartCode3)
		if n < 0 || i+n+3 >= len(b) {
			return false
		}
		i += n + 3
		if t := b[i] & 31; b[i]&0x80 == 0 && t >= 1 && t <= 23 {
			return true
		}
	}
}

// mfLengthToStartCodes rewrites 4-byte length prefixes in place.
func mfLengthToStartCodes(b []byte) bool {
	for i := 0; i < len(b); {
		if len(b)-i < 4 {
			return false
		}
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n < 0 || n > len(b)-i-4 {
			return false
		}
		copy(b[i:], []byte{0, 0, 0, 1})
		i += 4 + n
	}
	return true
}

// mfParamSets converts MF_MT_MPEG_SEQUENCE_HEADER, which is Annex B or avcC, to
// Annex B.
func mfParamSets(b []byte) []byte {
	if mfHasStartCode(b) {
		return b
	}
	if len(b) < 7 || b[0] != 1 {
		return nil
	}
	var out []byte
	i, count, ppsDone := 6, int(b[5]&31), false
	for {
		for ; count > 0; count-- {
			if i+2 > len(b) {
				return nil
			}
			n := int(binary.BigEndian.Uint16(b[i:]))
			i += 2
			if i+n > len(b) {
				return nil
			}
			out = append(append(out, 0, 0, 0, 1), b[i:i+n]...)
			i += n
		}
		if ppsDone || i >= len(b) {
			return out
		}
		count, ppsDone = int(b[i]), true
		i++
	}
}

// mfExtractParamSets copies the SPS and PPS NAL units of an Annex B buffer.
func mfExtractParamSets(b []byte) []byte {
	return mfExtractNALs(b, func(h byte) bool { t := h & 31; return t == 7 || t == 8 })
}

// mfExtractNALs copies the NAL units whose first byte satisfies want.
func mfExtractNALs(b []byte, want func(byte) bool) []byte {
	var out []byte
	for i := 0; ; {
		n := bytes.Index(b[i:], mfStartCode3)
		if n < 0 || i+n+3 >= len(b) {
			return out
		}
		start := i + n + 3
		end := len(b)
		if e := bytes.Index(b[start:], mfStartCode3); e >= 0 {
			end = start + e
			for end > start && b[end-1] == 0 {
				end--
			}
		}
		if want(b[start]) {
			out = append(append(out, 0, 0, 0, 1), b[start:end]...)
		}
		i = start
	}
}

// H.265 NAL unit types, as bits of the mask from mfH265Mask.
const (
	mfH265Slices = 1<<10 - 1  // non-IRAP pictures, types 0 to 9
	mfH265IRAP   = 0xff << 16 // types 16 to 23
	mfH265Params = 7 << 32    // VPS, SPS and PPS
)

func mfH265Type(h byte) int { return int(h >> 1 & 0x3f) }

// mfH265Mask returns a bit for each NAL unit type in an Annex B buffer.
func mfH265Mask(b []byte) uint64 {
	var mask uint64
	for i := 0; ; {
		n := bytes.Index(b[i:], mfStartCode3)
		if n < 0 || i+n+3 >= len(b) {
			return mask
		}
		i += n + 3
		mask |= 1 << mfH265Type(b[i])
	}
}

// mfValidH265 reports whether b holds at least one plausible NAL unit.
func mfValidH265(b []byte) bool {
	for i := 0; ; {
		n := bytes.Index(b[i:], mfStartCode3)
		if n < 0 || i+n+4 >= len(b) {
			return false
		}
		i += n + 3
		t := mfH265Type(b[i])
		if b[i]&0x80 == 0 && b[i+1]&7 != 0 && (t <= 9 || (t >= 16 && t <= 21) || (t >= 32 && t <= 40)) {
			return true
		}
	}
}

func mfIsH265Param(h byte) bool { t := mfH265Type(h); return t >= 32 && t <= 34 }

// mfH265ParamSets converts MF_MT_MPEG_SEQUENCE_HEADER, which is Annex B or
// hvcC, to VPS, SPS and PPS in Annex B form.
func mfH265ParamSets(b []byte) []byte {
	if mfHasStartCode(b) {
		return mfExtractNALs(b, mfIsH265Param)
	}
	if len(b) < 23 || b[0] != 1 {
		return nil
	}
	var out []byte
	i, arrays := 23, int(b[22])
	for ; arrays > 0; arrays-- {
		if i+3 > len(b) {
			return nil
		}
		count := int(binary.BigEndian.Uint16(b[i+1:]))
		i += 3
		for ; count > 0; count-- {
			if i+2 > len(b) {
				return nil
			}
			n := int(binary.BigEndian.Uint16(b[i:]))
			i += 2
			if n == 0 || i+n > len(b) {
				return nil
			}
			if mfIsH265Param(b[i]) {
				out = append(append(out, 0, 0, 0, 1), b[i:i+n]...)
			}
			i += n
		}
	}
	return out
}

// mfJPEGSize finds the dimensions in a JPEG frame header.
func mfJPEGSize(b []byte) (w, h int, ok bool) {
	f, ok := mfJPEGFrame(b)
	return f.w, f.h, ok
}

// mfJPEGHeader is what a JPEG frame header says.
type mfJPEGHeader struct {
	w, h       int
	at         int // offset of the height, which the width follows
	mcuW, mcuH int // the size of one MCU, which the coded data covers whole
}

// padded is the size rounded up to whole MCUs if it is odd, which the
// Windows decoder refuses. An even size is decoded as it is.
func (f mfJPEGHeader) padded() (w, h int) {
	if f.w%2 == 0 && f.h%2 == 0 {
		return f.w, f.h
	}
	return (f.w + f.mcuW - 1) / f.mcuW * f.mcuW, (f.h + f.mcuH - 1) / f.mcuH * f.mcuH
}

func mfJPEGFrame(b []byte) (f mfJPEGHeader, ok bool) {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return f, false
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xff {
			return f, false
		}
		m := b[i+1]
		switch {
		case m == 0xff: // fill byte
			i++
			continue
		case m == 0xd8 || m == 0x01 || (m >= 0xd0 && m <= 0xd7):
			i += 2
			continue
		case m == 0xd9 || m == 0xda:
			return f, false
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 {
			return f, false
		}
		if m >= 0xc0 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc {
			if n < 8 || i+2+n > len(b) {
				return f, false
			}
			f.at = i + 5
			f.h, f.w = int(binary.BigEndian.Uint16(b[i+5:])), int(binary.BigEndian.Uint16(b[i+7:]))
			maxH, maxV := 1, 1
			for c, comps := 0, int(b[i+9]); c < comps && i+12+3*c <= i+2+n; c++ {
				hv := b[i+11+3*c]
				maxH, maxV = max(maxH, int(hv>>4)), max(maxV, int(hv&15))
			}
			f.mcuW, f.mcuH = 8*maxH, 8*maxV
			return f, f.w > 0 && f.h > 0
		}
		i += 2 + n
	}
	return f, false
}

// mfVP8Size checks a frame tag. Only keyframes carry dimensions.
func mfVP8Size(b []byte) (w, h int, key, ok bool) {
	if len(b) < 3 {
		return 0, 0, false, false
	}
	if b[0]&1 != 0 {
		return 0, 0, false, true
	}
	if len(b) < 10 || b[3] != 0x9d || b[4] != 0x01 || b[5] != 0x2a {
		return 0, 0, true, false
	}
	w = int(binary.LittleEndian.Uint16(b[6:]) & 0x3fff)
	h = int(binary.LittleEndian.Uint16(b[8:]) & 0x3fff)
	return w, h, true, w > 0 && h > 0
}

// --- Video decoder ---

type mfPixels int

const (
	mfNV12 mfPixels = iota
	mfI420
	mfYV12
	mfYUY2
)

func mfPixelsOf(g mfGUID) (mfPixels, int, bool) {
	switch g {
	case fmtNV12:
		return mfNV12, 0, true
	case fmtI420, fmtIYUV:
		return mfI420, 1, true
	case fmtYV12:
		return mfYV12, 2, true
	case fmtYUY2:
		return mfYUY2, 3, true
	}
	return 0, 0, false
}

// mfConvert writes one picture into the NV12 frame f. src starts at the first
// row of the coded picture.
func mfConvert(f *VideoFrame, src []byte, pix mfPixels, stride, codedH, cropX, cropY int) {
	w, h := f.Width, f.Height
	cw, ch := (w+1)/2, (h+1)/2
	if pix == mfYUY2 {
		for y := 0; y < h; y++ {
			row := src[(y+cropY)*stride+cropX*2&^3:]
			dst := f.Y[y*f.YStride : y*f.YStride+w]
			odd := cropX & 1
			for x := range dst {
				dst[x] = row[(x+odd)*2]
			}
			if y&1 == 0 {
				uv := f.U[y/2*f.UVStride : y/2*f.UVStride+2*cw]
				for x := 0; x < cw; x++ {
					uv[2*x], uv[2*x+1] = row[4*x+1], row[4*x+3]
				}
			}
		}
		return
	}
	copyPlane(f.Y, f.YStride, src[cropY*stride+cropX:], stride, w, h)
	base := stride * codedH
	cx, cy := cropX/2, cropY/2
	if pix == mfNV12 {
		copyPlane(f.U, f.UVStride, src[base+cy*stride+cx*2:], stride, 2*cw, ch)
		return
	}
	cs := stride / 2
	second := base + cs*((codedH+1)/2)
	up, vp := base, second
	if pix == mfYV12 {
		up, vp = second, base
	}
	for y := 0; y < ch; y++ {
		u := src[up+(y+cy)*cs+cx:]
		v := src[vp+(y+cy)*cs+cx:]
		row := f.U[y*f.UVStride : y*f.UVStride+2*cw]
		for x := 0; x < cw; x++ {
			row[2*x], row[2*x+1] = u[x], v[x]
		}
	}
}

func mfPictureSize(pix mfPixels, stride, codedH int) int {
	switch pix {
	case mfNV12:
		return stride*codedH + stride*((codedH+1)/2)
	case mfYUY2:
		return stride * codedH
	}
	return stride*codedH + 2*(stride/2)*((codedH+1)/2)
}

type mfVideoDecoder struct {
	m     *mfTransform
	info  BackendInfo
	spec  mfSpec
	sub   *mfGUID
	sized bool // the input type needs the picture size
	// A JPEG image of a size that is not whole MCUs is decoded at the padded
	// size its data covers anyway, and cropped back: the decoder refuses odd
	// sizes. jpegW and jpegH are that size, jpegBuf the image as given to it.
	jpegW, jpegH int
	jpegBuf      []byte
	inW          int
	inH          int
	frames       FramePool
	out          fifo[*VideoFrame]
	flushed      bool
	closed       bool

	ready          bool // an output type is set
	pix            mfPixels
	codedH, stride int
	cropX, cropY   int
	w, h           int
}

func newMFVideoDecoder(cfg VideoConfig, info BackendInfo) (VideoDecoder, error) {
	spec, ok := mfSpecFor(cfg.Codec, Decode)
	if !ok {
		return nil, errMFNoTransform
	}
	d := &mfVideoDecoder{info: info, spec: spec, sized: cfg.Codec != H264 && cfg.Codec != H265}
	d.sub = &fmtH264
	switch cfg.Codec {
	case H265:
		d.sub = &fmtHEVC
	case JPEG:
		d.sub = &fmtMJPG
	case VP8:
		d.sub = &fmtVP80
	case VP9:
		d.sub = &fmtVP90
	}
	m, err := mfOpen(spec, info.Hardware, d, d.setup)
	if err != nil {
		return nil, err
	}
	d.m = m
	return d, nil
}

func (d *mfVideoDecoder) inputType(m *mfTransform, w, h int) (uintptr, error) {
	t, err := m.api.newMediaType(&m.c, &mediaVideo, d.sub)
	if err != nil {
		return 0, err
	}
	if w > 0 {
		if d.info.Codec == JPEG && d.jpegW > 0 {
			w, h = d.jpegW, d.jpegH // see padJPEG
		}
		m.c.setU64(t, &attrFrameSize, mfPair(w, h))
	}
	return t, nil
}

// setup configures a new MFT. When the input type needs the picture size and
// it is not yet known, it only checks that the MFT accepts the format.
func (d *mfVideoDecoder) setup(m *mfTransform) error {
	if m.codec != 0 {
		m.c.setCodecValue(m.codec, &attrLowLatency, vtUI4, 1) // decoders take VT_UI4
	}
	if d.sized && d.inW == 0 {
		t, err := d.inputType(m, 640, 480)
		if err != nil {
			return err
		}
		if hr := m.setType(slotSetInputType, t, mftSetTestOnly); hr == hrInvalidType {
			return mfError("input type", hr)
		}
		return nil
	}
	t, err := d.inputType(m, d.inW, d.inH)
	if err != nil {
		return err
	}
	if hr := m.setType(slotSetInputType, t, 0); mfFailed(hr) {
		return mfError("input type", hr)
	}
	d.ready = false
	if err := d.negotiate(m); err != nil {
		return err
	}
	return m.start()
}

// negotiate picks the best output format. MFTs that need to see the stream
// first report no types yet and raise a stream change later.
func (d *mfVideoDecoder) negotiate(m *mfTransform) error {
	c := &m.c
	best, bestRank := uintptr(0), 1<<30
	offered := false
	for i := 0; ; i++ {
		c.p = 0
		hr := c.m3(m.xf, slotOutAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&c.p)))
		if hr == hrTypeNotSet {
			return nil
		}
		if mfFailed(hr) {
			break
		}
		offered = true
		t := c.p
		sub, _ := c.getGUID(t, &attrSubtype)
		if _, rank, ok := mfPixelsOf(sub); ok && rank < bestRank {
			c.release(best)
			best, bestRank = t, rank
		} else {
			c.release(t)
		}
	}
	if best == 0 {
		if !offered {
			return nil
		}
		return errMFNoFormat
	}
	if hr := m.c.m3(m.xf, slotSetOutputType, 0, best, 0); mfFailed(hr) {
		c.release(best)
		return mfError("output type", hr)
	}
	err := d.readLayout(m, best)
	c.release(best)
	return err
}

func (d *mfVideoDecoder) readLayout(m *mfTransform, fallback uintptr) error {
	c := &m.c
	t := fallback
	c.p = 0
	cur := uintptr(0)
	if !mfFailed(c.m2(m.xf, slotOutCurrent, 0, uintptr(unsafe.Pointer(&c.p)))) {
		cur = c.p
		t = cur
	}
	defer c.release(cur)
	sub, _ := c.getGUID(t, &attrSubtype)
	pix, _, ok := mfPixelsOf(sub)
	if !ok {
		return errMFNoFormat
	}
	size, _ := c.getU64(t, &attrFrameSize)
	codedW, codedH := int(size>>32), int(uint32(size))
	if codedW == 0 || codedH == 0 {
		codedW, codedH = d.inW, d.inH
	}
	if codedW == 0 || codedH == 0 {
		return errors.New("mediafoundation: output type has no frame size")
	}
	stride := 0
	if v, ok := c.getU32(t, &attrStride); ok {
		stride = int(int32(v))
		if stride < 0 {
			stride = -stride
		}
	}
	if stride == 0 {
		stride = codedW
		if pix == mfYUY2 {
			stride = 2 * ((codedW + 1) &^ 1)
		}
	}
	x, y, w, h := 0, 0, codedW, codedH
	c.aperture = [4]uint32{}
	c.u = 0
	if !mfFailed(c.m4(t, slotGetBlob, gp(&attrAperture), uintptr(unsafe.Pointer(&c.aperture)), 16, uintptr(unsafe.Pointer(&c.u)))) && c.u >= 16 {
		a := c.aperture
		if cx, cy := int(a[2]), int(a[3]); cx > 0 && cy > 0 {
			x, y, w, h = int(int16(a[0]>>16)), int(int16(a[1]>>16)), cx, cy
		}
	} else if d.sized && d.inW > 0 && d.inW <= codedW && d.inH <= codedH {
		w, h = d.inW, d.inH
	}
	if d.info.Codec == JPEG && d.inW > 0 && d.inW <= codedW && d.inH <= codedH {
		x, y, w, h = 0, 0, d.inW, d.inH // the header states a padded size, see padJPEG
	}
	if x < 0 || y < 0 || x+w > codedW || y+h > codedH {
		return fmt.Errorf("mediafoundation: bad aperture %d,%d %dx%d in %dx%d", x, y, w, h, codedW, codedH)
	}
	d.pix, d.codedH, d.stride, d.cropX, d.cropY, d.w, d.h = pix, codedH, stride, x, y, w, h
	d.ready = true
	return nil
}

func (d *mfVideoDecoder) onFormat() error { return d.negotiate(d.m) }

func (d *mfVideoDecoder) onSample(sample uintptr) error {
	m := d.m
	if !d.ready {
		return errors.New("mediafoundation: output before format negotiation")
	}
	pts := m.samplePTS(sample)
	addr, pitch, size, err := m.lockSample(sample)
	if err != nil {
		return err
	}
	defer m.unlockSample()
	if size == 0 {
		return nil
	}
	stride := d.stride
	if pitch > 0 {
		stride = pitch
	}
	need := mfPictureSize(d.pix, stride, d.codedH)
	if size < need {
		return fmt.Errorf("mediafoundation: output of %d bytes, need %d", size, need)
	}
	f := d.frames.Get(d.w, d.h, NV12)
	f.PTS = pts
	mfConvert(f, cMem(addr, need), d.pix, stride, d.codedH, d.cropX, d.cropY)
	d.out.push(f)
	return nil
}

// prepare validates a packet and, where needed, configures the input size.
func (d *mfVideoDecoder) prepare(p Packet) error {
	w, h := 0, 0
	switch d.info.Codec {
	case H264:
		if !mfValidH264(p.Data) {
			return errMFBadH264
		}
		return nil
	case H265:
		if !mfValidH265(p.Data) {
			return errMFBadH265
		}
		return nil
	case VP9:
		f, ok := parseVP9Frame(p.Data)
		if !ok {
			return errMFBadVP9
		}
		if !f.Key {
			if d.inW == 0 {
				return errMFNeedVP9
			}
			return nil
		}
		w, h = f.Width, f.Height
	case JPEG:
		f, ok := mfJPEGFrame(p.Data)
		if !ok {
			return errMFBadJPEG
		}
		w, h = f.w, f.h
		if pw, ph := f.padded(); pw != d.jpegW || ph != d.jpegH {
			d.jpegW, d.jpegH = pw, ph
			if d.m.started {
				return d.resize(w, h)
			}
		}
	case VP8:
		var key, ok bool
		if w, h, key, ok = mfVP8Size(p.Data); !ok {
			return errMFBadVP8
		}
		if !key {
			if d.inW == 0 {
				return errMFNeedKey
			}
			return nil
		}
	}
	if w == d.inW && h == d.inH && d.m.started {
		return nil
	}
	return d.resize(w, h)
}

// resize moves to a new input size. The first size configures the MFT opened by
// newMFVideoDecoder. Later ones use a fresh MFT, since most decoders cannot
// change it while streaming.
func (d *mfVideoDecoder) resize(w, h int) error {
	d.inW, d.inH = w, h
	if d.m.xf != 0 && !d.m.started {
		return d.setup(d.m)
	}
	if d.m.started {
		if err := d.m.drain(); err != nil {
			return err
		}
	}
	d.m.close()
	m, err := mfOpen(d.spec, d.info.Hardware, d, func(m *mfTransform) error {
		d.m = m // the sink callbacks use d.m
		return d.setup(m)
	})
	if err != nil {
		d.m = &mfTransform{}
		d.inW, d.inH = 0, 0
		return err
	}
	d.m = m
	return nil
}

func (d *mfVideoDecoder) Send(p Packet) error {
	switch {
	case d.closed:
		return ErrClosed
	case d.flushed:
		return errMFFlushed
	case len(p.Data) == 0:
		return errMFEmpty
	}
	if err := d.prepare(p); err != nil {
		return err
	}
	data := p.Data
	if d.info.Codec == JPEG {
		data = d.padJPEG(data)
	}
	in, dst, err := d.m.begin(len(data))
	if err != nil {
		return err
	}
	copy(dst, data)
	return d.m.commit(in, len(data), p.PTS, 0, p.Keyframe)
}

// padJPEG states the padded size in the frame header of an image whose size
// is not whole MCUs. The coded data covers the padded size already, so only
// those four bytes change.
func (d *mfVideoDecoder) padJPEG(b []byte) []byte {
	f, ok := mfJPEGFrame(b)
	if !ok || (f.w == d.jpegW && f.h == d.jpegH) {
		return b
	}
	d.jpegBuf = append(d.jpegBuf[:0], b...)
	binary.BigEndian.PutUint16(d.jpegBuf[f.at:], uint16(d.jpegH))
	binary.BigEndian.PutUint16(d.jpegBuf[f.at+2:], uint16(d.jpegW))
	return d.jpegBuf
}

func (d *mfVideoDecoder) Receive() (*VideoFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if !d.flushed && d.m.started {
		if err := d.m.service(); err != nil {
			return nil, err
		}
		if f, ok := d.out.pop(); ok {
			return f, nil
		}
	}
	return nil, endOfQueue(d.flushed)
}

func (d *mfVideoDecoder) Flush() error {
	if d.closed {
		return ErrClosed
	}
	if d.flushed {
		return nil
	}
	d.flushed = true
	if !d.m.started {
		return nil
	}
	return d.m.drain()
}

func (d *mfVideoDecoder) Backend() BackendInfo { return d.info }

func (d *mfVideoDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.m.close()
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}

// --- H.264 and H.265 encoder ---

type mfEncoder struct {
	m       *mfTransform
	cfg     VideoConfig
	info    BackendInfo
	packets packetPool
	out     fifo[*EncodedPacket]
	pitch   int
	rows    int
	dur     time.Duration
	params  []byte // parameter sets in Annex B form
	flushed bool
	closed  bool
}

func newMFEncoder(cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	spec, ok := mfSpecFor(cfg.Codec, Encode)
	if !ok {
		return nil, errMFNoTransform
	}
	e := &mfEncoder{cfg: cfg, info: info, dur: time.Second / time.Duration(cfg.FPS)}
	m, err := mfOpen(spec, info.Hardware, e, e.setup)
	if err != nil {
		return nil, err
	}
	e.m = m
	return e, nil
}

// applyCodecAPI sets the rate control and latency properties. Encoders differ
// in which they support and when, so failures are not fatal.
func (e *mfEncoder) applyCodecAPI(m *mfTransform) {
	if m.codec == 0 {
		return
	}
	c, api := &m.c, m.codec
	c.setCodecValue(api, &apiRateControl, vtUI4, mfRateControlCBR)
	c.setCodecValue(api, &apiMeanBitrate, vtUI4, uint64(e.cfg.Bitrate))
	c.setCodecValue(api, &apiMaxBitrate, vtUI4, uint64(e.cfg.Bitrate))
	c.setCodecValue(api, &apiGOPSize, vtUI4, uint64(e.cfg.KeyframeEvery))
	c.setCodecValue(api, &apiBFrames, vtUI4, 0)
	c.setCodecValue(api, &attrLowLatency, vtBool, 0xffff) // VARIANT_TRUE
}

// SetBitrate implements BitrateSetter through ICodecAPI, which encoders
// accept during encoding for CBR.
func (e *mfEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.closed || e.m == nil {
		return ErrClosed
	}
	m := e.m
	if m.codec == 0 {
		return errBitrateUnsupported(e.info.Name)
	}
	c, api := &m.c, m.codec
	// Some encoders take the value and ignore it. Those that say so through
	// IsModifiable (S_FALSE) cannot change the bitrate while encoding. The
	// HEVC Video Extensions encoder does not implement IsModifiable and keeps
	// its first bitrate (measured on Windows 11 on Arm).
	const sFalse = 1
	if c.m1(api, slotCodecIsModifiable, gp(&apiMeanBitrate)) == sFalse || m.name == "HEVCVideoExtensionEncoder" {
		return errBitrateUnsupported(e.info.Name)
	}
	if hr := c.setCodecValue(api, &apiMeanBitrate, vtUI4, uint64(bps)); int32(hr) < 0 {
		return fmt.Errorf("mediafoundation: bitrate %d refused with HRESULT %#x: %w", bps, hr, errors.ErrUnsupported)
	}
	c.setCodecValue(api, &apiMaxBitrate, vtUI4, uint64(bps))
	e.cfg.Bitrate = bps
	return nil
}

func (e *mfEncoder) videoAttrs(c *mfCall, t uintptr) {
	cfg := e.cfg
	c.setU64(t, &attrFrameSize, mfPair(cfg.Width, cfg.Height))
	c.setU64(t, &attrFrameRate, mfPair(cfg.FPS, 1))
	c.setU64(t, &attrPAR, mfPair(1, 1))
	c.setU32(t, &attrInterlace, mfProgressive)
	const nominalFull, nominalLimited = 1, 2
	switch cfg.Range {
	case RangeFull:
		c.setU32(t, &attrNominalRange, nominalFull)
	case RangeLimited:
		c.setU32(t, &attrNominalRange, nominalLimited)
	}
}

func (e *mfEncoder) setup(m *mfTransform) error {
	e.m = m
	m.orderedPTS = true // no B-frames
	c, cfg := &m.c, e.cfg
	e.applyCodecAPI(m) // some encoders only read these before the types are set

	sub, profile := &fmtH264, uint32(mfProfileBase)
	if cfg.Codec == H265 {
		sub, profile = &fmtHEVC, mfProfileHEVC
	}
	t, err := m.api.newMediaType(c, &mediaVideo, sub)
	if err != nil {
		return err
	}
	e.videoAttrs(c, t)
	c.setU32(t, &attrAvgBitrate, uint32(cfg.Bitrate))
	c.setU32(t, &attrProfile, profile)
	c.setU32(t, &attrKeySpacing, uint32(cfg.KeyframeEvery))
	if hr := m.setType(slotSetOutputType, t, 0); mfFailed(hr) {
		return mfError("encoder output type", hr)
	}
	if err := e.setInputType(m); err != nil {
		return err
	}
	e.applyCodecAPI(m) // and some only after
	if err := m.start(); err != nil {
		return err
	}
	e.readInputLayout(m)
	e.readParams(m)
	return nil
}

func (e *mfEncoder) setInputType(m *mfTransform) error {
	c, cfg := &m.c, e.cfg
	t, err := m.api.newMediaType(c, &mediaVideo, &fmtNV12)
	if err != nil {
		return err
	}
	e.videoAttrs(c, t)
	c.setU32(t, &attrStride, uint32(cfg.Width))
	hr := m.setType(slotSetInputType, t, 0)
	if !mfFailed(hr) {
		return nil
	}
	// Fall back to the NV12 type the encoder itself offers.
	for i := 0; ; i++ {
		c.p = 0
		if mfFailed(c.m3(m.xf, slotInAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&c.p)))) {
			return mfError("encoder input type", hr)
		}
		t := c.p
		if sub, _ := c.getGUID(t, &attrSubtype); sub != fmtNV12 {
			c.release(t)
			continue
		}
		e.videoAttrs(c, t)
		if h := m.setType(slotSetInputType, t, 0); !mfFailed(h) {
			return nil
		}
	}
}

// readInputLayout finds the pitch and the rows before the chroma plane, which
// some encoders align beyond the picture height.
func (e *mfEncoder) readInputLayout(m *mfTransform) {
	c, cfg := &m.c, e.cfg
	e.pitch, e.rows = cfg.Width, cfg.Height
	c.p = 0
	if !mfFailed(c.m2(m.xf, slotInCurrent, 0, uintptr(unsafe.Pointer(&c.p)))) {
		t := c.p
		if v, ok := c.getU32(t, &attrStride); ok && int(int32(v)) >= cfg.Width {
			e.pitch = int(int32(v))
		}
		c.release(t)
	}
	if rows := int(m.inInfo.size) * 2 / (3 * e.pitch); rows > e.rows && rows-e.rows < 64 {
		e.rows = rows
	}
}

func (e *mfEncoder) readParams(m *mfTransform) {
	c := &m.c
	c.p = 0
	if mfFailed(c.m2(m.xf, slotOutCurrent, 0, uintptr(unsafe.Pointer(&c.p)))) {
		return
	}
	t := c.p
	blob := c.getBlob(t, &attrSeqHeader)
	b := mfParamSets(blob)
	if e.cfg.Codec == H265 {
		b = mfH265ParamSets(blob)
	}
	if len(b) > 0 {
		e.params = b
	}
	c.release(t)
}

func (e *mfEncoder) onFormat() error {
	m := e.m
	c := &m.c
	c.p = 0
	if hr := c.m3(m.xf, slotOutAvailable, 0, 0, uintptr(unsafe.Pointer(&c.p))); mfFailed(hr) {
		return mfError("encoder output type", hr)
	}
	if hr := m.setType(slotSetOutputType, c.p, 0); mfFailed(hr) {
		return mfError("encoder output type", hr)
	}
	e.readParams(m)
	return nil
}

func (e *mfEncoder) onSample(sample uintptr) error {
	m := e.m
	pts := m.samplePTS(sample)
	clean, _ := m.c.getU32(sample, &attrCleanPoint)
	addr, _, size, err := m.lockSample(sample)
	if err != nil {
		return err
	}
	defer m.unlockSample()
	if size == 0 {
		return nil
	}
	src := cMem(addr, size)
	pkt := e.packets.get(size)
	copy(pkt.Data, src)
	if !mfHasStartCode(pkt.Data) && !mfLengthToStartCodes(pkt.Data) {
		pkt.Release()
		return errors.New("mediafoundation: encoder output is neither Annex B nor length-prefixed")
	}
	var key, hasParams bool
	if e.cfg.Codec == H265 {
		mask := mfH265Mask(pkt.Data)
		key = mask&mfH265IRAP != 0 || (clean != 0 && mask&mfH265Slices == 0)
		hasParams = mask&mfH265Params == mfH265Params
		if hasParams && len(e.params) == 0 {
			e.params = mfExtractNALs(pkt.Data, mfIsH265Param)
		}
	} else {
		mask := mfNALMask(pkt.Data)
		const idr, sps, pps = 1 << 5, 1 << 7, 1 << 8
		key = mask&idr != 0 || (clean != 0 && mask&(1<<1) == 0)
		hasParams = mask&(sps|pps) == sps|pps
		if hasParams && len(e.params) == 0 {
			e.params = mfExtractParamSets(pkt.Data)
		}
	}
	if key && !hasParams {
		if len(e.params) == 0 {
			e.readParams(m)
		}
		if n := len(e.params); n > 0 {
			with := e.packets.get(n + len(pkt.Data))
			copy(with.Data, e.params)
			copy(with.Data[n:], pkt.Data)
			pkt.Release()
			pkt = with
		}
	}
	pkt.PTS, pkt.Keyframe = pts, key
	e.out.push(pkt)
	return nil
}

// forceKeyframe makes the next frame a keyframe. An encoder without the codec
// property is drained and replaced, as a new encoder starts with a keyframe.
func (e *mfEncoder) forceKeyframe() error {
	m := e.m
	if m.codec != 0 && !mfFailed(m.c.setCodecValue(m.codec, &apiForceKey, vtUI4, 1)) {
		return nil
	}
	if err := m.drain(); err != nil {
		return err
	}
	m.close()
	spec, _ := mfSpecFor(e.cfg.Codec, Encode)
	next, err := mfOpen(spec, e.info.Hardware, e, e.setup)
	if err != nil {
		e.m = &mfTransform{}
		return err
	}
	e.m = next
	return nil
}

func (e *mfEncoder) Send(f *VideoFrame) error {
	switch {
	case e.closed:
		return ErrClosed
	case e.flushed:
		return errMFFlushed
	}
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	if f.ForceKeyframe {
		if err := e.forceKeyframe(); err != nil {
			return err
		}
	}
	m := e.m
	n := e.pitch*e.rows + e.pitch*(f.Height+1)/2
	in, dst, err := m.begin(n)
	if err != nil {
		return err
	}
	writeNV12(dst, dst[e.pitch*e.rows:], e.pitch, f)
	return m.commit(in, max(n, int(m.inInfo.size)), f.PTS, e.dur, false)
}

func (e *mfEncoder) Receive() (*EncodedPacket, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	if !e.flushed {
		if err := e.m.service(); err != nil {
			return nil, err
		}
		if p, ok := e.out.pop(); ok {
			return p, nil
		}
	}
	return nil, endOfQueue(e.flushed)
}

func (e *mfEncoder) Flush() error {
	if e.closed {
		return ErrClosed
	}
	if e.flushed {
		return nil
	}
	e.flushed = true
	return e.m.drain()
}

func (e *mfEncoder) Backend() BackendInfo { return e.info }

func (e *mfEncoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	e.m.close()
	for {
		p, ok := e.out.pop()
		if !ok {
			return nil
		}
		p.Release()
	}
}

// --- Opus decoder ---

type mfAudioDecoder struct {
	m        *mfTransform
	cfg      AudioConfig
	info     BackendInfo
	pool     audioPool
	out      fifo[*AudioFrame]
	float    bool
	head     [19]byte
	lastSize int // samples per channel of the latest output
	flushed  bool
	closed   bool
}

func newMFAudioDecoder(cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	spec, ok := mfSpecFor(cfg.Codec, Decode)
	if !ok {
		return nil, errMFNoTransform
	}
	d := &mfAudioDecoder{cfg: cfg, info: info, lastSize: cfg.SampleRate / 50}
	copy(d.head[:], "OpusHead")
	d.head[8], d.head[9] = 1, byte(cfg.Channels)
	binary.LittleEndian.PutUint32(d.head[12:], uint32(cfg.SampleRate))
	m, err := mfOpen(spec, info.Hardware, d, d.setup)
	if err != nil {
		return nil, err
	}
	d.m = m
	return d, nil
}

func (d *mfAudioDecoder) setup(m *mfTransform) error {
	d.m = m
	c, cfg := &m.c, d.cfg
	var hr uint32
	for _, withHead := range []bool{true, false} {
		t, err := m.api.newMediaType(c, &mediaAudio, &fmtOpus)
		if err != nil {
			return err
		}
		c.setU32(t, &attrRate, uint32(cfg.SampleRate))
		c.setU32(t, &attrChannels, uint32(cfg.Channels))
		if withHead {
			c.m3(t, slotSetBlob, gp(&attrUserData), uintptr(unsafe.Pointer(&d.head)), uintptr(len(d.head)))
		}
		if hr = m.setType(slotSetInputType, t, 0); !mfFailed(hr) {
			break
		}
	}
	if mfFailed(hr) {
		return mfError("opus input type", hr)
	}
	if err := d.onFormat(); err != nil {
		return err
	}
	return m.start()
}

// onFormat selects 16-bit or float PCM at the configured rate and layout.
func (d *mfAudioDecoder) onFormat() error {
	m := d.m
	c, cfg := &m.c, d.cfg
	best, bestFloat := uintptr(0), false
	for i := 0; ; i++ {
		c.p = 0
		if mfFailed(c.m3(m.xf, slotOutAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&c.p)))) {
			break
		}
		t := c.p
		sub, _ := c.getGUID(t, &attrSubtype)
		bits, _ := c.getU32(t, &attrBits)
		rate, _ := c.getU32(t, &attrRate)
		ch, _ := c.getU32(t, &attrChannels)
		pcm := sub == fmtPCM && bits == 16
		flt := sub == fmtFloat && bits == 32
		if int(rate) == cfg.SampleRate && int(ch) == cfg.Channels && (pcm || (flt && best == 0)) {
			c.release(best)
			best, bestFloat = t, flt
			if pcm {
				break
			}
			continue
		}
		c.release(t)
	}
	if best == 0 {
		return fmt.Errorf("mediafoundation: no PCM output at %d Hz, %d channels", cfg.SampleRate, cfg.Channels)
	}
	hr := m.setType(slotSetOutputType, best, 0)
	if mfFailed(hr) {
		return mfError("opus output type", hr)
	}
	d.float = bestFloat
	return nil
}

func (d *mfAudioDecoder) onSample(sample uintptr) error {
	m := d.m
	pts := m.samplePTS(sample)
	addr, _, size, err := m.lockSample(sample)
	if err != nil {
		return err
	}
	defer m.unlockSample()
	width := 2
	if d.float {
		width = 4
	}
	n := size / width
	if n == 0 {
		return nil
	}
	f := d.pool.get(n)
	f.SampleRate, f.Channels, f.PTS = d.cfg.SampleRate, d.cfg.Channels, pts
	if d.float {
		for i, v := range unsafe.Slice((*float32)(mfPtr(addr)), n) {
			f.Samples[i] = int16(math.Max(-32768, math.Min(32767, float64(v)*32768)))
		}
	} else {
		copy(f.Samples, unsafe.Slice((*int16)(mfPtr(addr)), n))
	}
	d.lastSize = n / d.cfg.Channels
	d.out.push(f)
	return nil
}

func (d *mfAudioDecoder) Send(p Packet) error {
	switch {
	case d.closed:
		return ErrClosed
	case d.flushed:
		return errMFFlushed
	}
	if len(p.Data) == 0 {
		// MFTs have no packet-loss interface, so a lost packet becomes silence
		// of the same length as the previous output.
		f := d.pool.get(d.lastSize * d.cfg.Channels)
		clear(f.Samples)
		f.SampleRate, f.Channels, f.PTS = d.cfg.SampleRate, d.cfg.Channels, p.PTS
		d.out.push(f)
		return nil
	}
	in, dst, err := d.m.begin(len(p.Data))
	if err != nil {
		return err
	}
	copy(dst, p.Data)
	return d.m.commit(in, len(p.Data), p.PTS, 0, true)
}

func (d *mfAudioDecoder) Receive() (*AudioFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if d.flushed {
		return nil, io.EOF
	}
	if err := d.m.service(); err != nil {
		return nil, err
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, ErrAgain
}

func (d *mfAudioDecoder) Flush() error {
	if d.closed {
		return ErrClosed
	}
	if d.flushed {
		return nil
	}
	d.flushed = true
	return d.m.drain()
}

func (d *mfAudioDecoder) Backend() BackendInfo { return d.info }

func (d *mfAudioDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.m.close()
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}
