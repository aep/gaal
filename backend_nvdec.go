//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/aep/gaal/internal/dl"
)

// NVDEC. The parser's per-picture callbacks are assembly thunks that do not
// enter the Go runtime: reflection-based callbacks allocate on every call.
// Only a real format change reaches Go.

const (
	nvRingSize = 64
	nvRingMask = nvRingSize - 1
)

type nvDisplay struct {
	index       int32
	progressive int32
	pts         int64
}

// nvPicture is a picture handed to the decoder, in decoding order.
type nvPicture struct {
	index  int32
	field  int32
	second int32
	_      int32
}

// nvShared is read and written by the thunks. See nvthunk_linux_*.s.
type nvShared struct {
	dec                      uintptr
	decodeFn                 uintptr
	goSeq                    uintptr
	codedW, codedH, surfaces uint32
	tail                     uint64
	ring                     [nvRingSize]nvDisplay
	picTail                  uint64
	pics                     [nvRingSize]nvPicture
	d                        *nvDecoder
}

// nvThunks returns the C-callable addresses of the thunks.
func nvThunks() (seq, decode, display uintptr)

// The thunks are only called from C. These declarations satisfy go vet.
func nvSeqThunk()
func nvDecodeThunk()
func nvDisplayThunk()

type cuvidAPI struct {
	createParser, parse, destroyParser, createDecoder, destroyDecoder,
	decode, mapFrame, unmapFrame uintptr
	seqCB uintptr
}

var loadCUVID = sync.OnceValues(func() (*cuvidAPI, error) {
	h, err := dl.Open("libnvcuvid.so.1")
	if err != nil {
		return nil, err
	}
	a := &cuvidAPI{}
	if err := dl.BindAll(h, dl.Table{
		"cuvidCreateVideoParser": &a.createParser, "cuvidParseVideoData": &a.parse,
		"cuvidDestroyVideoParser": &a.destroyParser, "cuvidCreateDecoder": &a.createDecoder,
		"cuvidDestroyDecoder": &a.destroyDecoder, "cuvidDecodePicture": &a.decode,
		"cuvidMapVideoFrame64": &a.mapFrame, "cuvidUnmapVideoFrame64": &a.unmapFrame,
	}); err != nil {
		return nil, err
	}
	// Callbacks cannot be freed, so one serves every decoder.
	a.seqCB = purego.NewCallback(func(shared, format uintptr) uintptr {
		return (*(**nvShared)(unsafe.Pointer(&shared))).d.onSequence(format)
	})
	return a, nil
})

type nvDecoder struct {
	c       dl.Caller
	api     *cuvidAPI
	cu      *cudaAPI
	info    BackendInfo
	codec   uint32
	shared  *nvShared
	parser  uintptr
	head    uint64
	w, h    int
	surfH   int
	frames  FramePool
	out     fifo[*VideoFrame]
	err     error
	flushed bool

	// lowDelay presents pictures as they are decoded instead of when the
	// parser's display logic releases them, which for a stream without a
	// reorder limit is only when its DPB overflows.
	lowDelay bool
	picHead  uint64
	lastPTS  uint64 // of the latest packet

	pkt, proc, copyArgs []byte
	devPtr              uint64
	pitch               uint32
	popped              uintptr
}

func nvCodec(c Codec) uint32 {
	switch c {
	case H264:
		return cvH264
	case VP8:
		return cvVP8
	case H265:
		return cvHEVC
	case VP9:
		return cvVP9
	}
	return cvJPEG
}

func newNVDecoder(_ *Library, cfg VideoConfig, info BackendInfo) (VideoDecoder, error) {
	cu, err := loadCUDA()
	if err != nil {
		return nil, err
	}
	api, err := loadCUVID()
	if err != nil {
		return nil, err
	}
	d := &nvDecoder{api: api, cu: cu, info: info, codec: nvCodec(info.Codec),
		lowDelay: cfg.LowDelay && (info.Codec == H264 || info.Codec == H265),
		pkt:      make([]byte, cvPktSize), proc: make([]byte, cvProcSize), copyArgs: make([]byte, cuMSize)}
	d.shared = &nvShared{decodeFn: api.decode, goSeq: api.seqCB, d: d}
	seq, decode, display := nvThunks()
	p := make([]byte, cvParserSize)
	le32(p, cvPCodec, d.codec)
	le32(p, cvPSurfaces, 1)
	le32(p, cvPClock, 1_000_000_000)
	le32(p, cvPDelay, 0) // output each picture as soon as it can be displayed
	le64(p, cvPUser, uint64(uintptr(unsafe.Pointer(d.shared))))
	le64(p, cvPSeq, uint64(seq))
	le64(p, cvPDec, uint64(decode))
	le64(p, cvPDisp, uint64(display))
	if r, _, _ := purego.SyscallN(api.createParser, uintptr(unsafe.Pointer(&d.parser)), uintptr(unsafe.Pointer(&p[0]))); int32(r) != 0 {
		return nil, fmt.Errorf("nvdec: create parser: error %d", int32(r))
	}
	return d, nil
}

// cvFormatView covers every CUVIDEOFORMAT field read by onSequence.
const cvFormatView = max(cvFCW, cvFCH, cvFChroma, cvFDepth, cvFMinSurf, cvFL, cvFT, cvFR, cvFB) + 4

// onSequence runs when the coded size changes. It may be called from C only.
func (d *nvDecoder) onSequence(format uintptr) uintptr {
	s := d.shared
	f := cBytes(format, cvFormatView)
	codedW, codedH := rd32(f, cvFCW), rd32(f, cvFCH)
	surfaces := uint32(f[cvFMinSurf])
	if f[cvFDepth] != 0 {
		d.err = errors.New("nvdec: only 8-bit video is supported")
		return 0
	}
	if s.dec != 0 {
		purego.SyscallN(d.api.destroyDecoder, s.dec)
		s.dec, s.surfaces = 0, 0
	}
	left, top := int32(rd32(f, cvFL)), int32(rd32(f, cvFT))
	right, bottom := int32(rd32(f, cvFR)), int32(rd32(f, cvFB))
	d.w, d.h = int(right-left), int(bottom-top)
	d.surfH = rup2(d.h)
	// NV12 surfaces have even sizes. Decoding one more row or column of the
	// coded picture, where there is one, keeps NVDEC from scaling an odd
	// display area to fit.
	if int(left)+rup2(d.w) <= int(codedW) {
		right = left + int32(rup2(d.w))
	}
	if int(top)+d.surfH <= int(codedH) {
		bottom = top + int32(d.surfH)
	}
	c := make([]byte, cvCreateSize)
	le64(c, cvCW, uint64(codedW))
	le64(c, cvCH, uint64(codedH))
	le64(c, cvCSurf, uint64(surfaces))
	le32(c, cvCCodec, d.codec)
	le32(c, cvCChroma, rd32(f, cvFChroma))
	le64(c, cvCFlags, cvPreferCUVID)
	le32(c, cvCFmt, cvNV12)
	le32(c, cvCDeint, cvWeave)
	le64(c, cvCTW, uint64(rup2(d.w)))
	le64(c, cvCTH, uint64(d.surfH))
	le64(c, cvCOutSurf, 2)
	*(*[4]int16)(unsafe.Pointer(&c[cvCDisplay])) = [4]int16{int16(left), int16(top), int16(right), int16(bottom)}
	if r, _, _ := purego.SyscallN(d.api.createDecoder, uintptr(unsafe.Pointer(&s.dec)), uintptr(unsafe.Pointer(&c[0]))); int32(r) != 0 {
		s.dec = 0
		d.err = fmt.Errorf("nvdec: create decoder: error %d", int32(r))
		return 0
	}
	s.codedW, s.codedH, s.surfaces = codedW, codedH, surfaces
	return uintptr(surfaces)
}

func rup2(v int) int { return (v + 1) &^ 1 }

func (d *nvDecoder) copySurface(srcY int, dst []byte, stride, width, height int) error {
	m := d.copyArgs
	clear(m)
	le32(m, cuMSrcType, 2) // device
	le64(m, cuMSrcDev, d.devPtr)
	le64(m, cuMSrcPitch, uint64(d.pitch))
	le64(m, cuMSrcY, uint64(srcY))
	le32(m, cuMDstType, 1) // host
	le64(m, cuMDstHost, uint64(uintptr(unsafe.Pointer(&dst[0]))))
	le64(m, cuMDstPitch, uint64(stride))
	le64(m, cuMW, uint64(width))
	le64(m, cuMH, uint64(height))
	if r := d.c.C1(d.cu.memcpy2D, uintptr(unsafe.Pointer(&m[0]))); int32(r) != 0 {
		return fmt.Errorf("nvdec: copy: error %d", int32(r))
	}
	return nil
}

// present maps a decoded picture and copies it into a pooled frame.
func (d *nvDecoder) present(e *nvDisplay) error {
	clear(d.proc)
	le32(d.proc, cvRProg, uint32(e.progressive))
	if r := d.c.C5(d.api.mapFrame, d.shared.dec, uintptr(e.index), uintptr(unsafe.Pointer(&d.devPtr)),
		uintptr(unsafe.Pointer(&d.pitch)), uintptr(unsafe.Pointer(&d.proc[0]))); int32(r) != 0 {
		return fmt.Errorf("nvdec: map frame: error %d", int32(r))
	}
	f := d.frames.Get(d.w, d.h, NV12)
	f.PTS = timeFrom(uint64(e.pts))
	var err error
	if f.YStride == f.UVStride && f.Height == d.surfH {
		// The planes are contiguous on both sides, so one transfer moves them.
		err = d.copySurface(0, f.store, f.YStride, f.Width, f.Height+f.Height/2)
	} else {
		err = d.copySurface(0, f.Y, f.YStride, f.Width, f.Height)
		if err == nil {
			err = d.copySurface(d.surfH, f.U, f.UVStride, f.UVStride, (f.Height+1)/2)
		}
	}
	d.c.C2(d.api.unmapFrame, d.shared.dec, uintptr(d.devPtr))
	if err != nil {
		f.Release()
		return err
	}
	d.out.push(f)
	return nil
}

// parse feeds the parser, then collects the pictures it made displayable.
func (d *nvDecoder) parse(data unsafe.Pointer, size int, flags, pts uint64) error {
	if err := d.cu.enter(&d.c); err != nil {
		return err
	}
	defer d.cu.leave(&d.c, &d.popped)
	p := d.pkt
	le64(p, cvKFlags, flags)
	le64(p, cvKSize, uint64(size))
	*(*unsafe.Pointer)(unsafe.Pointer(&p[cvKData])) = data
	le64(p, cvKTS, pts)
	r := d.c.C2(d.api.parse, d.parser, uintptr(unsafe.Pointer(&p[0])))
	*(*unsafe.Pointer)(unsafe.Pointer(&p[cvKData])) = nil
	s := d.shared
	if d.err != nil || int32(r) != 0 {
		// Pictures of a failed packet must not get the next packet's timestamp.
		d.picHead = s.picTail
		if d.err != nil {
			return d.err
		}
		return fmt.Errorf("nvdec: parse: error %d", int32(r))
	}
	if d.lowDelay {
		d.head = s.tail // the parser's display order is not used
		return d.presentDecoded(int64(d.lastPTS))
	}
	for ; d.head != s.tail; d.head++ {
		if err := d.present(&s.ring[d.head&nvRingMask]); err != nil {
			return err
		}
	}
	return nil
}

// presentDecoded outputs the pictures decoded for the current packet, which
// all carry its timestamp. A field pair becomes one frame with its second field.
func (d *nvDecoder) presentDecoded(pts int64) error {
	s := d.shared
	for ; d.picHead != s.picTail; d.picHead++ {
		p := &s.pics[d.picHead&nvRingMask]
		if p.field != 0 && p.second == 0 {
			continue
		}
		e := nvDisplay{index: p.index, progressive: 1, pts: pts}
		if p.field != 0 {
			e.progressive = 0
		}
		if err := d.present(&e); err != nil {
			return err
		}
	}
	return nil
}

func (d *nvDecoder) Send(p Packet) error {
	if len(p.Data) == 0 {
		return fmt.Errorf("%w: empty packet", ErrInvalid)
	}
	// End-of-picture lets the parser decode now instead of waiting for the
	// start of the next access unit.
	d.lastPTS = uint64(p.PTS)
	err := d.parse(unsafe.Pointer(&p.Data[0]), len(p.Data), cvPktTS|cvPktEndPic, uint64(p.PTS))
	runtime.KeepAlive(p.Data)
	return err
}

func (d *nvDecoder) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if d.flushed {
		return nil, io.EOF
	}
	return nil, ErrAgain
}

func (d *nvDecoder) Flush() error {
	if d.flushed {
		return nil
	}
	d.flushed = true
	return d.parse(nil, 0, cvPktEOS, 0)
}

func (d *nvDecoder) Backend() BackendInfo { return d.info }

func (d *nvDecoder) Close() error {
	if d.parser != 0 {
		purego.SyscallN(d.api.destroyParser, d.parser)
		d.parser = 0
	}
	if s := d.shared; s.dec != 0 {
		if d.cu.enter(&d.c) == nil {
			purego.SyscallN(d.api.destroyDecoder, s.dec)
			d.cu.leave(&d.c, &d.popped)
		}
		s.dec = 0
	}
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}
