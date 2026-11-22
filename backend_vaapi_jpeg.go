//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"bytes"
	"errors"
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Baseline JPEG decoding. Only headers are parsed, and the entropy-coded scan
// goes to the hardware as it is.

var (
	errJPEGData = fmt.Errorf("%w: malformed jpeg", ErrCorrupt)

	errJPEGProgressive = errors.New("vaapi: only baseline jpeg is supported")
	errJPEGScans       = errors.New("vaapi: jpeg with several scans is not supported")
	errJPEGSampling    = errors.New("vaapi: only 4:2:0 jpeg is supported")
	errJPEGSize        = errors.New("vaapi: jpeg too small for the hardware path")
)

type vaJPEGComp struct{ id, h, v, tq uint8 }
type vaJPEGScan struct{ sel, dc, ac uint8 }

type vaJPEGDecoder struct {
	s    *vaSession
	info BackendInfo

	pic, iq, huff, slice []byte

	w, h    int
	comps   [4]vaJPEGComp
	ncomp   int
	scan    [4]vaJPEGScan
	nscan   int
	restart int
	hasSOF  bool

	surfaceW, surfaceH int

	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
	closed  bool
}

func newVAJPEGDecoder(info BackendInfo) (VideoDecoder, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	ds, err := vaFind(vaJPEGProfiles, vaVLD)
	if err != nil {
		return nil, err
	}
	return &vaJPEGDecoder{s: newVASession(a, ds[0]), info: info, pic: make([]byte, vaSizeJPic),
		iq: make([]byte, vaSizeJIQ), huff: make([]byte, vaSizeJHuff), slice: make([]byte, vaSizeJSlice)}, nil
}

func (d *vaJPEGDecoder) dqt(b []byte) error {
	for len(b) > 0 {
		pq, tq := b[0]>>4, b[0]&15
		if pq != 0 {
			return errJPEGProgressive // 16-bit tables are not baseline
		}
		if tq > 3 || len(b) < 65 {
			return errJPEGData
		}
		d.iq[vaoJIQLoad+int(tq)] = 1
		copy(d.iq[vaoJIQTable+int(tq)*64:], b[1:65]) // both are in zigzag order
		b = b[65:]
	}
	return nil
}

func (d *vaJPEGDecoder) dht(b []byte) error {
	for len(b) > 0 {
		if len(b) < 17 {
			return errJPEGData
		}
		class, id := b[0]>>4, int(b[0]&15)
		if class > 1 || id > 3 {
			return errJPEGData
		}
		if id > 1 {
			return errJPEGProgressive // VA-API has two table slots, as baseline does
		}
		n := 0
		for _, c := range b[1:17] {
			n += int(c)
		}
		if len(b) < 17+n {
			return errJPEGData
		}
		t := d.huff[vaoJHTable+id*vaSizeJHTable:]
		if class == 0 {
			if n > 12 {
				return errJPEGData
			}
			copy(t[vaoJHNumDC-vaoJHTable:], b[1:17])
			copy(t[vaoJHDC-vaoJHTable:], b[17:17+n])
		} else {
			if n > 162 {
				return errJPEGData
			}
			copy(t[vaoJHNumAC-vaoJHTable:], b[1:17])
			copy(t[vaoJHAC-vaoJHTable:], b[17:17+n])
		}
		d.huff[vaoJHLoad+id] = 1
		b = b[17+n:]
	}
	return nil
}

func (d *vaJPEGDecoder) sof(b []byte) error {
	if len(b) < 6 || b[0] != 8 {
		return errJPEGData
	}
	d.h, d.w = int(b[1])<<8|int(b[2]), int(b[3])<<8|int(b[4])
	d.ncomp = int(b[5])
	if d.w == 0 || d.h == 0 || (d.ncomp != 1 && d.ncomp != 3) || len(b) < 6+3*d.ncomp {
		return errJPEGData
	}
	for i := 0; i < d.ncomp; i++ {
		c := b[6+3*i:]
		cp := vaJPEGComp{c[0], c[1] >> 4, c[1] & 15, c[2]}
		if cp.h == 0 || cp.h > 4 || cp.v == 0 || cp.v > 4 || cp.tq > 3 {
			return errJPEGData
		}
		d.comps[i] = cp
	}
	d.hasSOF = true
	return nil
}

func (d *vaJPEGDecoder) sos(b []byte) error {
	if !d.hasSOF || len(b) < 1 {
		return errJPEGData
	}
	d.nscan = int(b[0])
	if d.nscan == 0 || d.nscan > 4 || len(b) < 1+2*d.nscan+3 {
		return errJPEGData
	}
	if d.nscan != d.ncomp {
		return errJPEGScans
	}
	for i := 0; i < d.nscan; i++ {
		c, t := b[1+2*i], b[2+2*i]
		if t>>4 > 1 || t&15 > 1 {
			return errJPEGData
		}
		d.scan[i] = vaJPEGScan{c, t >> 4, t & 15}
	}
	return nil
}

// parse walks the markers and returns the entropy-coded data.
func (d *vaJPEGDecoder) parse(b []byte) ([]byte, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errJPEGData
	}
	clear(d.iq[:vaoJIQLoad+4])
	clear(d.huff[:vaoJHLoad+2])
	d.restart, d.hasSOF = 0, false
	i := 2
	for {
		for i < len(b) && b[i] != 0xFF {
			i++ // tolerate stray bytes between segments
		}
		for i < len(b) && b[i] == 0xFF {
			i++
		}
		if i >= len(b) {
			return nil, errJPEGData
		}
		m := b[i]
		i++
		if m == 0xD9 || m == 0 {
			return nil, errJPEGData
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD8) {
			continue
		}
		if i+2 > len(b) {
			return nil, errJPEGData
		}
		n := int(b[i])<<8 | int(b[i+1])
		if n < 2 || i+n > len(b) {
			return nil, errJPEGData
		}
		seg := b[i+2 : i+n]
		i += n
		var err error
		switch m {
		case 0xDB:
			err = d.dqt(seg)
		case 0xC4:
			err = d.dht(seg)
		case 0xC0, 0xC1:
			err = d.sof(seg)
		case 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
			err = errJPEGProgressive
		case 0xDD:
			if len(seg) < 2 {
				return nil, errJPEGData
			}
			d.restart = int(seg[0])<<8 | int(seg[1])
		case 0xDA:
			if err := d.sos(seg); err != nil {
				return nil, err
			}
			return vaJPEGScanData(b[i:])
		}
		if err != nil {
			return nil, err
		}
	}
}

// vaJPEGScanData cuts b at the first marker that is not stuffing or a restart.
func vaJPEGScanData(b []byte) ([]byte, error) {
	for at := 0; ; {
		i := bytes.IndexByte(b[at:], 0xFF)
		if i < 0 {
			if len(b) == 0 {
				return nil, errJPEGData
			}
			return b, nil // truncated: decode what is there
		}
		at += i
		if at+1 >= len(b) {
			return b[:at], nil
		}
		if m := b[at+1]; m != 0 && (m < 0xD0 || m > 0xD7) && m != 0xFF {
			if at == 0 {
				return nil, errJPEGData
			}
			return b[:at], nil
		}
		at += 2
	}
}

// layout checks the sampling and returns the MCU count. Only 4:2:0 maps directly
// onto an NV12 surface. This check protects the device as well as the output:
// radeonsi does not reject other samplings, and submitting 4:2:2 to a surface
// of another format made the JPEG engine fault and need a ring reset. Images
// below one MCU row are left to software for the same reason.
func (d *vaJPEGDecoder) layout() (mcus int, err error) {
	c := &d.comps
	if d.w < 16 || d.h < 16 {
		return 0, errJPEGSize
	}
	if d.ncomp != 3 || c[0].h != 2 || c[0].v != 2 || c[1].h != 1 || c[1].v != 1 || c[2].h != 1 || c[2].v != 1 {
		return 0, errJPEGSampling
	}
	return (d.w + 15) / 16 * ((d.h + 15) / 16), nil
}

// vaJPEGMinSurface is the smallest context radeonsi accepts. Smaller images are
// decoded into the top left corner of a surface of this size.
const vaJPEGMinSurface = 64

func (d *vaJPEGDecoder) configure() error {
	w, h := max(d.w, vaJPEGMinSurface), max(d.h, vaJPEGMinSurface)
	if d.s.hasCtx && d.surfaceW == w && d.surfaceH == h {
		return nil
	}
	s := d.s
	s.destroyVideo()
	if !s.hasConfig {
		if err := s.createConfig(vaEndpoint{vaProfileJPEG, vaEntryVLD}, vaAttrRTFormat, vaRTYUV420); err != nil {
			return err
		}
	}
	if err := s.createSurfaces(vaRTYUV420, vaFourccNV12, w, h, 1); err != nil {
		return err
	}
	if err := s.createContext(w, h, vaProgressive); err != nil {
		s.destroyVideo()
		return err
	}
	d.surfaceW, d.surfaceH = w, h
	return nil
}

func (d *vaJPEGDecoder) Send(p Packet) error {
	if d.closed {
		return ErrClosed
	}
	if d.flushed {
		return fmt.Errorf("%w: send after flush", ErrInvalid)
	}
	scan, err := d.parse(p.Data)
	if err != nil {
		return err
	}
	mcus, err := d.layout()
	if err != nil {
		return err
	}
	for i := 0; i < d.nscan; i++ {
		if d.huff[vaoJHLoad+int(d.scan[i].dc)] == 0 || d.huff[vaoJHLoad+int(d.scan[i].ac)] == 0 {
			return errJPEGData
		}
	}
	for i := 0; i < d.ncomp; i++ {
		if d.iq[vaoJIQLoad+int(d.comps[i].tq)] == 0 {
			return errJPEGData
		}
	}
	if err := d.configure(); err != nil {
		return err
	}
	b := d.pic
	clear(b)
	vaPut16(b, vaoJW, uint16(d.w))
	vaPut16(b, vaoJH, uint16(d.h))
	b[vaoJNumComps] = uint8(d.ncomp)
	for i := 0; i < d.ncomp; i++ {
		c := b[vaoJComps+i*vaSizeJComp:]
		cp := d.comps[i]
		c[vaoJCompID-vaoJComps], c[vaoJCompH-vaoJComps] = cp.id, cp.h
		c[vaoJCompV-vaoJComps], c[vaoJCompQ-vaoJComps] = cp.v, cp.tq
	}
	sl := d.slice
	clear(sl)
	le32(sl, vaoJSSize, uint32(len(scan)))
	le32(sl, vaoJSFlag, vaSliceDataAll)
	sl[vaoJSNum] = uint8(d.nscan)
	vaPut16(sl, vaoJSRestart, uint16(d.restart))
	le32(sl, vaoJSMCUs, uint32(mcus))
	for i := 0; i < d.nscan; i++ {
		c := sl[vaoJSComps+i*vaSizeJSComp:]
		sc := d.scan[i]
		c[vaoJSSel-vaoJSComps], c[vaoJSDC-vaoJSComps], c[vaoJSAC-vaoJSComps] = sc.sel, sc.dc, sc.ac
	}
	s := d.s
	for _, q := range [...]struct {
		typ uint32
		b   []byte
	}{{vaBufPicParam, b}, {vaBufIQ, d.iq}, {vaBufHuffman, d.huff}, {vaBufSliceParam, sl}, {vaBufSliceData, scan}} {
		if err := s.addBuffer(q.typ, unsafe.Pointer(&q.b[0]), len(q.b)); err != nil {
			s.dropBuffers()
			return err
		}
	}
	surface := s.surfaces[0]
	if err := s.submit(surface); err != nil {
		return err
	}
	if err := s.sync(surface); err != nil {
		return err
	}
	f := d.frames.Get(d.w, d.h, NV12)
	f.PTS = p.PTS
	if err := s.readSurface(surface, f, 0, 0, d.surfaceW, d.surfaceH); err != nil {
		f.Release()
		return err
	}
	d.out.push(f)
	return nil
}

func (d *vaJPEGDecoder) Receive() (*VideoFrame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *vaJPEGDecoder) Flush() error         { d.flushed = true; return nil }
func (d *vaJPEGDecoder) Backend() BackendInfo { return d.info }

func (d *vaJPEGDecoder) Close() error {
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

// vaPutImage has eleven arguments, one more than dl.Caller provides.
func vaPutImage(s *vaSession, surface, image uint32, w, h int) error {
	r, _, _ := purego.SyscallN(s.a.putImage, s.dpy, uintptr(surface), uintptr(image),
		0, 0, uintptr(w), uintptr(h), 0, 0, uintptr(w), uintptr(h))
	return s.check("put image", r)
}
