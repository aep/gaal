//go:build amd64 || arm64

package gaal

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// OpenH264 as a software fallback for H.264. This module contains no H.264 code.
// The library comes from the system or, if the application allows it, from the
// verified copy of Cisco's binary that Library.Prepare stored in the cache. Its
// structs and vtables depend on the release, so only releases whose headers
// match those the layouts were generated from are accepted.

type ohAPI struct {
	createDec, destroyDec uintptr
	createEnc, destroyEnc uintptr
	version               [4]uint32 // major, minor, revision, reserved
	cached                bool      // Cisco's binary from the cache
}

// One copy of the library serves the whole process. Two copies can share a
// soname, and the dynamic loader would then return the first for both.
var ohLib struct {
	sync.Mutex
	api *ohAPI
}

// ohSystemNames lists the system library names. Tests replace it.
var ohSystemNames = func() []string { return libNames("openh264", "8") }

// ohOldestMinor is the oldest release of ohMajor with the same ABI: the
// headers of 2.6 and 2.7 differ only in the version (Debian 13 ships 2.6),
// and 2.5 lacks the PSNR fields 2.6 added to the encoder's structs.
const ohOldestMinor = 6

func ohCheckVersion(v [4]uint32) error {
	if v[0] != ohMajor || v[1] < ohOldestMinor || v[1] > ohMinor {
		return fmt.Errorf("openh264: version %d.%d.%d does not match the supported ABI %d.%d-%d.%d", v[0], v[1], v[2], ohMajor, ohOldestMinor, ohMajor, ohMinor)
	}
	return nil
}

// ohBind loads one library file. A file that is refused is closed again, so a
// later attempt can load a different copy.
func ohBind(name string, cached bool) (*ohAPI, error) {
	h, err := ohOpen(name)
	if err != nil {
		return nil, err
	}
	a := &ohAPI{cached: cached}
	var versionEx uintptr
	err = dl.BindAll(h, dl.Table{
		"WelsCreateDecoder": &a.createDec, "WelsDestroyDecoder": &a.destroyDec,
		"WelsCreateSVCEncoder": &a.createEnc, "WelsDestroySVCEncoder": &a.destroyEnc,
		"WelsGetCodecVersionEx": &versionEx,
	})
	if err == nil {
		// WelsGetCodecVersion returns a struct by value, which has a different
		// convention on each platform. This variant fills a pointer instead.
		var c dl.Caller
		c.C1(versionEx, uintptr(unsafe.Pointer(&a.version)))
		err = ohCheckVersion(a.version)
	}
	if err != nil {
		ohClose(h)
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return a, nil
}

// ohLoad returns the library. It never uses the network. Cisco's cached binary
// is only considered if the application allows downloads, so clearing
// Options.AllowDownload disables it.
func ohLoad(l *Library) (*ohAPI, error) {
	ohLib.Lock()
	defer ohLib.Unlock()
	if a := ohLib.api; a != nil {
		if a.cached && !l.opts.AllowDownload {
			return nil, errors.New("openh264: Cisco's binary is loaded, but Options.AllowDownload is false")
		}
		return a, nil
	}
	var errs []error
	if l.opts.AllowDownload {
		path, err := ohCachedFile(l)
		if err == nil && path != "" {
			var a *ohAPI
			if a, err = ohBind(path, true); err == nil {
				ohLib.api = a
				return a, nil
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range ohSystemNames() {
		a, err := ohBind(name, false)
		if err == nil {
			ohLib.api = a
			return a, nil
		}
		errs = append(errs, err)
	}
	if l.opts.AllowDownload {
		errs = append(errs, errors.New("openh264: no prepared copy, call Library.Prepare"))
	} else if len(errs) == 0 {
		errs = append(errs, errors.New("openh264: no library to load"))
	}
	return nil, errors.Join(errs...)
}

var (
	ohDecInfo = BackendInfo{"openh264", H264, Decode, false}
	ohEncInfo = BackendInfo{"openh264", H264, Encode, false}
)

func openH264Backends() []backend {
	avail := func(l *Library) error { _, err := ohLoad(l); return err }
	return []backend{
		{BackendInfo: ohDecInfo, avail: avail, vdec: newOHDecoder},
		{BackendInfo: ohEncInfo, avail: avail, venc: newOHEncoder},
	}
}

// ohMethod reads a C++ virtual function pointer from an object.
func ohMethod(obj uintptr, off int) uintptr {
	vtable := rdPtr(cMem(obj, 8), 0)
	return rdPtr(cMem(vtable, off+8), off)
}

// ohDecodeDamaged lists the decoding states that mean damaged input.
const ohDecodeDamaged = ohDsRefLost | ohDsBitstream | ohDsDepLost | ohDsNoParam | ohDsConcealed | ohDsRefNull

// ohDecodeBroken lists the decoding states that are not caused by the input.
const ohDecodeBroken = ohDsInvalid | ohDsNoInit | ohDsNoMem | ohDsNeedExp

type ohDecoder struct {
	c       dl.Caller
	api     *ohAPI
	obj     uintptr
	decode  uintptr
	flushFn uintptr
	opt     int32
	dst     [3]uintptr
	info    []byte
	param   []byte
	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
	// baseline relabels the SPS for VideoConfig.LowDelay: OpenH264 holds back
	// one picture of every stream above baseline profile to reorder, and has
	// no setting to stop that.
	baseline *spsRewriter
}

func newOHDecoder(l *Library, cfg VideoConfig) (VideoDecoder, error) {
	a, err := ohLoad(l)
	if err != nil {
		return nil, err
	}
	d := &ohDecoder{api: a, info: make([]byte, ohBufSize), param: make([]byte, ohDecParamSize)}
	if cfg.LowDelay {
		d.baseline = &spsRewriter{rewrite: appendBaselineSPS}
	}
	if rc := d.c.C1(a.createDec, uintptr(unsafe.Pointer(&d.obj))); int32(rc) != 0 || d.obj == 0 {
		return nil, fmt.Errorf("openh264: cannot create decoder, code %d", int32(rc))
	}
	d.opt = ohLogQuiet
	d.c.C3(ohMethod(d.obj, ohDSetOpt), d.obj, ohDOptTrace, uintptr(unsafe.Pointer(&d.opt)))
	d.param[ohDDQ] = 0xff
	le32(d.param, ohDEC, ohECDisable)
	le32(d.param, ohDVCL, ohBSAVC)
	if rc := d.c.C2(ohMethod(d.obj, ohDInit), d.obj, uintptr(unsafe.Pointer(&d.param[0]))); int32(rc) != 0 {
		d.c.C1(a.destroyDec, d.obj)
		d.obj = 0
		return nil, fmt.Errorf("openh264: decoder init failed with code %d", int32(rc))
	}
	d.decode, d.flushFn = ohMethod(d.obj, ohDNoDelay), ohMethod(d.obj, ohDFlush)
	return d, nil
}

// collect copies a finished picture out of the decoder.
func (d *ohDecoder) collect() {
	i := d.info
	if rd32(i, ohBStatus) != 1 {
		return
	}
	w, h := int(int32(rd32(i, ohBW))), int(int32(rd32(i, ohBH)))
	ys, uvs := int(int32(rd32(i, ohBStride))), int(int32(rd32(i, ohBStride+4)))
	cw, ch := (w+1)/2, (h+1)/2
	if w <= 0 || h <= 0 || ys < w || uvs < cw || d.dst[0] == 0 || d.dst[1] == 0 || d.dst[2] == 0 {
		return
	}
	f := d.frames.Get(w, h, I420)
	f.PTS = time.Duration(rd64(i, ohBOutTS))
	copyPlane(f.Y, f.YStride, cMem(d.dst[0], ys*(h-1)+w), ys, w, h)
	copyPlane(f.U, f.UVStride, cMem(d.dst[1], uvs*(ch-1)+cw), uvs, cw, ch)
	copyPlane(f.V, f.UVStride, cMem(d.dst[2], uvs*(ch-1)+cw), uvs, cw, ch)
	d.out.push(f)
}

func (d *ohDecoder) resetInfo(pts time.Duration) {
	d.dst = [3]uintptr{}
	le32(d.info, ohBStatus, 0)
	le64(d.info, ohBInTS, uint64(pts))
	le64(d.info, ohBOutTS, 0)
}

func (d *ohDecoder) Send(p Packet) error {
	if d.obj == 0 {
		return ErrClosed
	}
	if d.flushed {
		return fmt.Errorf("%w: Send after Flush", ErrInvalid)
	}
	if len(p.Data) == 0 || len(p.Data) > math.MaxInt32 {
		return fmt.Errorf("%w: packet of %d bytes", ErrInvalid, len(p.Data))
	}
	if d.baseline != nil {
		p.Data = d.baseline.apply(p.Data)
	}
	d.resetInfo(p.PTS)
	rc := d.c.C5(d.decode, d.obj, uintptr(unsafe.Pointer(&p.Data[0])), uintptr(len(p.Data)),
		uintptr(unsafe.Pointer(&d.dst[0])), uintptr(unsafe.Pointer(&d.info[0])))
	runtime.KeepAlive(p.Data)
	state := int32(rc)
	d.collect()
	switch {
	case state&ohDecodeBroken != 0:
		return fmt.Errorf("openh264: decode failed with state %#x", state)
	case state&ohDecodeDamaged != 0:
		return fmt.Errorf("%w: openh264: decoding state %#x", ErrCorrupt, state)
	}
	return nil
}

func (d *ohDecoder) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if d.obj == 0 {
		return nil, ErrClosed
	}
	return nil, endOfQueue(d.flushed)
}

func (d *ohDecoder) Flush() error {
	if d.obj == 0 {
		return ErrClosed
	}
	if d.flushed {
		return nil
	}
	d.flushed = true
	d.opt = 1
	d.c.C3(ohMethod(d.obj, ohDSetOpt), d.obj, ohDOptEOS, uintptr(unsafe.Pointer(&d.opt)))
	// The reorder buffer of H.264 holds at most 16 frames, the bound is a guard.
	for range 32 {
		d.resetInfo(0)
		d.c.C3(d.flushFn, d.obj, uintptr(unsafe.Pointer(&d.dst[0])), uintptr(unsafe.Pointer(&d.info[0])))
		if rd32(d.info, ohBStatus) != 1 {
			break
		}
		d.collect()
	}
	return nil
}

func (d *ohDecoder) Close() error {
	if d.obj != 0 {
		d.c.C1(ohMethod(d.obj, ohDUninit), d.obj)
		d.c.C1(d.api.destroyDec, d.obj)
		d.obj = 0
	}
	return nil
}

func (d *ohDecoder) Backend() BackendInfo { return ohDecInfo }

type ohEncoder struct {
	c       dl.Caller
	api     *ohAPI
	cfg     VideoConfig
	obj     uintptr
	encode  uintptr
	force   uintptr
	opt     int32
	param   []byte
	pic     []byte
	info    []byte
	scratch *VideoFrame
	packets packetPool
	out     fifo[*EncodedPacket]
	flushed bool
	rate    [2]int32 // SBitrateInfo: layer, bits per second
}

// ohThreads picks the encoder threads. Each thread encodes its own slice in the
// same frame, so threads add no delay. Small pictures do not need them.
func ohThreads(cfg VideoConfig) int {
	if cfg.Width*cfg.Height < 640*480 {
		return 1
	}
	return min(4, runtime.NumCPU())
}

func newOHEncoder(l *Library, cfg VideoConfig) (VideoEncoder, error) {
	a, err := ohLoad(l)
	if err != nil {
		return nil, err
	}
	e := &ohEncoder{api: a, cfg: cfg, param: make([]byte, ohParamSize), pic: make([]byte, ohPicSize), info: make([]byte, ohInfoSize)}
	if rc := e.c.C1(a.createEnc, uintptr(unsafe.Pointer(&e.obj))); int32(rc) != 0 || e.obj == 0 {
		return nil, fmt.Errorf("openh264: cannot create encoder, code %d", int32(rc))
	}
	setOpt := ohMethod(e.obj, ohESetOpt)
	e.opt = ohLogQuiet
	e.c.C3(setOpt, e.obj, ohEOptTrace, uintptr(unsafe.Pointer(&e.opt)))
	p := e.param
	rc := e.c.C2(ohMethod(e.obj, ohEDefaults), e.obj, uintptr(unsafe.Pointer(&p[0])))
	if int32(rc) == 0 {
		threads := ohThreads(cfg)
		fps := math.Float32bits(float32(cfg.FPS))
		for _, f := range []struct {
			off int
			v   uint32
		}{{ohPUsage, ohRealtime}, {ohPW, uint32(cfg.Width)}, {ohPH, uint32(cfg.Height)},
			{ohPRate, uint32(cfg.Bitrate)}, {ohPRC, ohRCBitrate}, {ohPFPS, fps},
			{ohPTL, 1}, {ohPSL, 1}, {ohPIntra, uint32(cfg.KeyframeEvery)}, {ohPStrategy, ohConstant},
			{ohPLayer0 + ohLW, uint32(cfg.Width)}, {ohPLayer0 + ohLH, uint32(cfg.Height)},
			{ohPLayer0 + ohLFPS, fps}, {ohPLayer0 + ohLRate, uint32(cfg.Bitrate)}} {
			le32(p, f.off, f.v)
		}
		p[ohPSkip] = 0 // every input frame produces a packet
		if cfg.Range != RangeUnspecified {
			const videoFormatUnspecified = 5
			p[ohPLayer0+ohLSignal] = 1
			p[ohPLayer0+ohLVideoFormat] = videoFormatUnspecified
			if cfg.Range == RangeFull {
				p[ohPLayer0+ohLFullRange] = 1
			}
		}
		*(*uint16)(unsafe.Pointer(&p[ohPThreads])) = uint16(threads)
		if threads > 1 {
			le32(p, ohPLayer0+ohLSliceMode, ohSliceFixed)
			le32(p, ohPLayer0+ohLSliceNum, uint32(threads))
		} else {
			le32(p, ohPLayer0+ohLSliceMode, ohSliceSingle)
		}
		rc = e.c.C2(ohMethod(e.obj, ohEInitExt), e.obj, uintptr(unsafe.Pointer(&p[0])))
	}
	if int32(rc) != 0 {
		e.c.C1(a.destroyEnc, e.obj)
		e.obj = 0
		return nil, fmt.Errorf("openh264: encoder init failed with code %d", int32(rc))
	}
	e.c.C3(setOpt, e.obj, ohEOptTrace, uintptr(unsafe.Pointer(&e.opt)))
	e.encode, e.force = ohMethod(e.obj, ohEEncode), ohMethod(e.obj, ohEForce)
	le32(e.pic, ohSFmt, ohI420)
	le32(e.pic, ohSW, uint32(cfg.Width))
	le32(e.pic, ohSH, uint32(cfg.Height))
	return e, nil
}

func (e *ohEncoder) Send(f *VideoFrame) error {
	if e.obj == 0 {
		return ErrClosed
	}
	if e.flushed {
		return fmt.Errorf("%w: Send after Flush", ErrInvalid)
	}
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	src := f
	if f.Format != I420 {
		if e.scratch == nil {
			e.scratch = NewVideoFrame(f.Width, f.Height, I420)
		}
		if err := f.ToI420(e.scratch); err != nil {
			return err
		}
		src = e.scratch
	}
	if f.ForceKeyframe {
		// The C header omits the layer argument of the C++ method, which reads
		// it anyway. -1 selects all layers.
		if rc := e.c.C3(e.force, e.obj, 1, ^uintptr(0)); int32(rc) != 0 {
			return fmt.Errorf("openh264: force keyframe failed with code %d", int32(rc))
		}
	}
	pic := e.pic
	le32(pic, ohSStride, uint32(src.YStride))
	le32(pic, ohSStride+4, uint32(src.UVStride))
	le32(pic, ohSStride+8, uint32(src.UVStride))
	le64(pic, ohSData, uint64(uintptr(unsafe.Pointer(&src.Y[0]))))
	le64(pic, ohSData+8, uint64(uintptr(unsafe.Pointer(&src.U[0]))))
	le64(pic, ohSData+16, uint64(uintptr(unsafe.Pointer(&src.V[0]))))
	le64(pic, ohSTS, uint64(f.PTS/time.Millisecond))
	le32(e.info, ohILayers, 0)
	rc := e.c.C3(e.encode, e.obj, uintptr(unsafe.Pointer(&pic[0])), uintptr(unsafe.Pointer(&e.info[0])))
	runtime.KeepAlive(src)
	if int32(rc) != 0 {
		return fmt.Errorf("openh264: encode failed with code %d", int32(rc))
	}
	e.collect(f.PTS)
	return nil
}

// collect joins the layers of the encoded frame, which are Annex B already.
func (e *ohEncoder) collect(pts time.Duration) {
	info := e.info
	kind := rd32(info, ohIType)
	layers := int(int32(rd32(info, ohILayers)))
	if kind == ohSkipFrame || layers <= 0 {
		return
	}
	total := 0
	for i := range layers {
		total += e.layerSize(i)
	}
	if total == 0 {
		return
	}
	p := e.packets.get(total)
	off := 0
	for i := range layers {
		if n := e.layerSize(i); n > 0 {
			l := info[ohILayer+i*ohLayerSize:]
			copy(p.Data[off:], cMem(rdPtr(l, ohLBuf), n))
			off += n
		}
	}
	p.PTS, p.Keyframe = pts, kind == ohIDR
	e.out.push(p)
}

func (e *ohEncoder) layerSize(i int) int {
	l := e.info[ohILayer+i*ohLayerSize:]
	nals := int(int32(rd32(l, ohLNals)))
	if nals <= 0 {
		return 0
	}
	lens := cMem(rdPtr(l, ohLLens), 4*nals)
	n := 0
	for j := range nals {
		n += int(int32(rd32(lens, 4*j)))
	}
	return n
}

func (e *ohEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	if e.obj == 0 {
		return nil, ErrClosed
	}
	return nil, endOfQueue(e.flushed)
}

// Flush only ends the input, because the encoder holds no frames back.
func (e *ohEncoder) Flush() error {
	if e.obj == 0 {
		return ErrClosed
	}
	e.flushed = true
	return nil
}

// SetBitrate implements BitrateSetter. OpenH264 refuses a target above the
// maximum and a maximum below the target, so the maximum is set on both sides.
func (e *ohEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.obj == 0 {
		return ErrClosed
	}
	setOpt := ohMethod(e.obj, ohESetOpt)
	maxRate := int32(min(int64(bps)*3/2, math.MaxInt32))
	e.rate = [2]int32{ohSpatialAll, maxRate}
	e.c.C3(setOpt, e.obj, ohOptMaxBitrate, uintptr(unsafe.Pointer(&e.rate)))
	e.rate[1] = int32(min(bps, math.MaxInt32))
	if rc := e.c.C3(setOpt, e.obj, ohOptBitrate, uintptr(unsafe.Pointer(&e.rate))); int32(rc) != 0 {
		return fmt.Errorf("openh264: bitrate %d refused with code %d", bps, int32(rc))
	}
	e.rate[1] = maxRate
	e.c.C3(setOpt, e.obj, ohOptMaxBitrate, uintptr(unsafe.Pointer(&e.rate)))
	e.cfg.Bitrate = bps
	return nil
}

func (e *ohEncoder) Close() error {
	if e.obj != 0 {
		e.c.C1(ohMethod(e.obj, ohEUninit), e.obj)
		e.c.C1(e.api.destroyEnc, e.obj)
		e.obj = 0
	}
	return nil
}

func (e *ohEncoder) Backend() BackendInfo { return ohEncInfo }
