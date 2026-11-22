//go:build darwin && arm64

package gaal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
	"github.com/ebitengine/purego"
)

// VideoToolbox and AudioToolbox. Calls follow the arm64 ABI, where a CMTime is
// larger than 16 bytes and is therefore passed as a pointer to a copy. Output
// callbacks are assembly thunks that never enter the Go runtime.

const (
	cfStringUTF8 = 0x08000100
	cfNumInt32   = 3
	cfNumFloat64 = 6

	fourccH264 = 0x61766331 // avc1
	fourccHEVC = 0x68766331 // hvc1
	fourccVP9  = 0x76703039 // vp09
	fourccJPEG = 0x6a706567 // jpeg
	fourccOpus = 0x6f707573 // opus
	fourccPCM  = 0x6c70636d // lpcm

	pixel420v = 0x34323076 // NV12, video range
	pixel420f = 0x34323066 // NV12, full range

	cmTimeValid = 1
	vtTimescale = 1_000_000_000
	vtLockRead  = 1
	vtRingSize  = 64
	vtRingMask  = vtRingSize - 1
	vtBadData   = -12909
	// A decoder that stopped working, rather than data it cannot decode.
	vtInvalidSession = -12903
	vtMalfunction    = -12911
	cmBadData        = -8969
	vtFrameDamage    = 1 << 1 // kVTDecodeInfo_FrameDropped

	vtMaxParamSets = 16 // parameter sets of one H.265 format description

	vtHEVCRateHint = 240 // see newVTEncoder

	// vtH264MaxPixelRate is the highest ExpectedFrameRate in pixels per second
	// that H.264 low-latency rate control accepts: 1080p at 90, while 100 fails.
	vtH264MaxPixelRate = 1920 * 1080 * 90
)

// vtOutput is one queued callback. Its size is used by the assembly.
type vtOutput struct {
	status int32
	flags  uint32
	ref    uint64
	obj    uintptr // retained CMSampleBuffer or CVPixelBuffer
}

const vtOutputSize = unsafe.Sizeof(vtOutput{})

// vtShared is written by vtOutputThunk.
type vtShared struct {
	retain uintptr
	tail   atomic.Uint64
	ring   [vtRingSize]vtOutput
}

type cmTime struct {
	value     int64
	timescale int32
	flags     uint32
	epoch     int64
}

func vtThunks() (output, audio uintptr)

// The thunks are only called from C. These declarations satisfy go vet.
func vtOutputThunk()
func auInputThunk()

type appleAPI struct {
	stringCreate  func(alloc uintptr, s string, enc uint32) uintptr
	numberCreate  func(alloc uintptr, kind int, p unsafe.Pointer) uintptr
	dictCreate    func(alloc uintptr, capacity int, keys, values uintptr) uintptr
	dictSet       func(d, k, v uintptr)
	cfRelease     func(uintptr)
	hwDecode      func(codec uint32) bool
	formatCreate  func(alloc uintptr, codec uint32, w, h int32, ext uintptr, out *uintptr) int32
	h264Format    func(alloc uintptr, n uintptr, sets, sizes unsafe.Pointer, nalLen int32, out *uintptr) int32
	hevcFormat    func(alloc uintptr, n uintptr, sets, sizes unsafe.Pointer, nalLen int32, ext uintptr, out *uintptr) int32
	dataCreate    func(alloc uintptr, p unsafe.Pointer, n int) uintptr
	addVP9        func(codec uint32) // nil where the system has no such call
	encoderProps  uintptr
	compCreate    uintptr
	compInvalid   func(uintptr)
	compPool      func(uintptr) uintptr
	compPrepare   func(uintptr) int32
	setProperty   func(s, k, v uintptr) int32
	decCreate     func(alloc, desc, spec, attrs uintptr, rec unsafe.Pointer, out *uintptr) int32
	decInvalid    func(uintptr)
	audioNew      func(in, out unsafe.Pointer, conv *uintptr) int32
	audioDispose  func(uintptr) int32
	audioSetProp  func(conv uintptr, id, size uint32, p unsafe.Pointer) int32
	release       uintptr
	retain        uintptr
	dictGet       uintptr
	arrayCount    uintptr
	arrayAt       uintptr
	encodeFrame   uintptr
	complete      uintptr
	decodeFrame   uintptr
	decWait       uintptr
	poolCreate    uintptr
	lockBase      uintptr
	unlockBase    uintptr
	planeBase     uintptr
	planeStride   uintptr
	pixW, pixH    uintptr
	blockCreate   uintptr
	sampleCreate  uintptr
	sampleData    uintptr
	sampleFormat  uintptr
	sampleAttach  uintptr
	blockPointer  uintptr
	h264Param     uintptr
	hevcParam     uintptr
	audioFill     uintptr
	kTrue         uintptr
	kFalse        uintptr
	kAllocNull    uintptr
	keyCB, valCB  uintptr
	kNotSync      uintptr
	kForceKey     uintptr
	kPixelFormat  uintptr
	kIOSurface    uintptr
	kWidth        uintptr
	kHeight       uintptr
	kRealTime     uintptr
	kReorder      uintptr
	kFPS          uintptr
	kBitrate      uintptr
	kMaxKey       uintptr
	kProfile      uintptr
	kProfileValue uintptr
	kProfileHEVC  uintptr
	kQuality      uintptr
	kLowLatency   uintptr
	kRequireHWDec uintptr
	kEnableHWDec  uintptr
	kRequireHWEnc uintptr
	kMaxDelay     uintptr
	kSpeed        uintptr
	kAtoms        uintptr
	forceKeyProps uintptr
}

// constant reads a global CFTypeRef exported by a framework.
func constant(h uintptr, name string, out *uintptr) error {
	addr, err := dl.Sym(h, name)
	if err != nil {
		return err
	}
	*out = *(*uintptr)(unsafe.Pointer(&cMem(addr, 8)[0]))
	return nil
}

var loadApple = sync.OnceValues(func() (*appleAPI, error) {
	const fw = "/System/Library/Frameworks/"
	open := func(n string) (uintptr, error) { return dl.Open(fw + n + ".framework/" + n) }
	cf, err := open("CoreFoundation")
	if err != nil {
		return nil, err
	}
	cm, err := open("CoreMedia")
	if err != nil {
		return nil, err
	}
	cv, err := open("CoreVideo")
	if err != nil {
		return nil, err
	}
	vt, err := open("VideoToolbox")
	if err != nil {
		return nil, err
	}
	at, err := open("AudioToolbox")
	if err != nil {
		return nil, err
	}
	a := &appleAPI{}
	for _, t := range []struct {
		h uintptr
		t dl.Table
	}{
		{cf, dl.Table{"CFStringCreateWithCString": &a.stringCreate, "CFNumberCreate": &a.numberCreate,
			"CFDictionaryCreateMutable": &a.dictCreate, "CFDictionarySetValue": &a.dictSet, "CFRelease": &a.release,
			"CFRetain": &a.retain, "CFDictionaryGetValue": &a.dictGet, "CFArrayGetCount": &a.arrayCount,
			"CFArrayGetValueAtIndex": &a.arrayAt, "CFDataCreate": &a.dataCreate}},
		{cm, dl.Table{"CMVideoFormatDescriptionCreate": &a.formatCreate,
			"CMVideoFormatDescriptionCreateFromH264ParameterSets": &a.h264Format,
			"CMVideoFormatDescriptionCreateFromHEVCParameterSets": &a.hevcFormat,
			"CMVideoFormatDescriptionGetHEVCParameterSetAtIndex":  &a.hevcParam,
			"CMBlockBufferCreateWithMemoryBlock":                  &a.blockCreate, "CMSampleBufferCreateReady": &a.sampleCreate,
			"CMSampleBufferGetDataBuffer": &a.sampleData, "CMSampleBufferGetFormatDescription": &a.sampleFormat,
			"CMSampleBufferGetSampleAttachmentsArray": &a.sampleAttach, "CMBlockBufferGetDataPointer": &a.blockPointer,
			"CMVideoFormatDescriptionGetH264ParameterSetAtIndex": &a.h264Param}},
		{cv, dl.Table{"CVPixelBufferPoolCreatePixelBuffer": &a.poolCreate, "CVPixelBufferLockBaseAddress": &a.lockBase,
			"CVPixelBufferUnlockBaseAddress": &a.unlockBase, "CVPixelBufferGetBaseAddressOfPlane": &a.planeBase,
			"CVPixelBufferGetBytesPerRowOfPlane": &a.planeStride, "CVPixelBufferGetWidth": &a.pixW,
			"CVPixelBufferGetHeight": &a.pixH}},
		{vt, dl.Table{"VTCompressionSessionCreate": &a.compCreate, "VTCompressionSessionInvalidate": &a.compInvalid,
			"VTCompressionSessionGetPixelBufferPool": &a.compPool, "VTCompressionSessionPrepareToEncodeFrames": &a.compPrepare,
			"VTSessionSetProperty": &a.setProperty, "VTDecompressionSessionCreate": &a.decCreate,
			"VTDecompressionSessionInvalidate": &a.decInvalid, "VTCompressionSessionEncodeFrame": &a.encodeFrame,
			"VTCompressionSessionCompleteFrames": &a.complete, "VTDecompressionSessionDecodeFrame": &a.decodeFrame,
			"VTDecompressionSessionWaitForAsynchronousFrames": &a.decWait, "VTIsHardwareDecodeSupported": &a.hwDecode,
			"VTCopySupportedPropertyDictionaryForEncoder": &a.encoderProps}},
		{at, dl.Table{"AudioConverterNew": &a.audioNew, "AudioConverterDispose": &a.audioDispose,
			"AudioConverterSetProperty": &a.audioSetProp, "AudioConverterFillComplexBuffer": &a.audioFill}},
	} {
		if err := dl.BindAll(t.h, t.t); err != nil {
			return nil, err
		}
	}
	a.cfRelease = func(o uintptr) {
		var c dl.Caller
		c.C1(a.release, o)
	}
	if a.keyCB, err = dl.Sym(cf, "kCFTypeDictionaryKeyCallBacks"); err != nil {
		return nil, err
	}
	if a.valCB, err = dl.Sym(cf, "kCFTypeDictionaryValueCallBacks"); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		h    uintptr
		name string
		out  *uintptr
	}{
		{cf, "kCFBooleanTrue", &a.kTrue}, {cf, "kCFBooleanFalse", &a.kFalse}, {cf, "kCFAllocatorNull", &a.kAllocNull},
		{cm, "kCMSampleAttachmentKey_NotSync", &a.kNotSync},
		{cm, "kCMFormatDescriptionExtension_SampleDescriptionExtensionAtoms", &a.kAtoms},
		{cv, "kCVPixelBufferPixelFormatTypeKey", &a.kPixelFormat}, {cv, "kCVPixelBufferIOSurfacePropertiesKey", &a.kIOSurface},
		{cv, "kCVPixelBufferWidthKey", &a.kWidth}, {cv, "kCVPixelBufferHeightKey", &a.kHeight},
		{vt, "kVTEncodeFrameOptionKey_ForceKeyFrame", &a.kForceKey},
		{vt, "kVTCompressionPropertyKey_RealTime", &a.kRealTime},
		{vt, "kVTCompressionPropertyKey_AllowFrameReordering", &a.kReorder},
		{vt, "kVTCompressionPropertyKey_ExpectedFrameRate", &a.kFPS},
		{vt, "kVTCompressionPropertyKey_AverageBitRate", &a.kBitrate},
		{vt, "kVTCompressionPropertyKey_MaxKeyFrameInterval", &a.kMaxKey},
		{vt, "kVTCompressionPropertyKey_ProfileLevel", &a.kProfile},
		{vt, "kVTProfileLevel_H264_ConstrainedHigh_AutoLevel", &a.kProfileValue},
		{vt, "kVTProfileLevel_HEVC_Main_AutoLevel", &a.kProfileHEVC},
		{vt, "kVTCompressionPropertyKey_Quality", &a.kQuality},
		{vt, "kVTVideoEncoderSpecification_EnableLowLatencyRateControl", &a.kLowLatency},
	} {
		if err := constant(c.h, c.name, c.out); err != nil {
			return nil, err
		}
	}
	// Absent on iOS, where decoding is always hardware when available.
	constant(vt, "kVTVideoDecoderSpecification_RequireHardwareAcceleratedVideoDecoder", &a.kRequireHWDec)
	constant(vt, "kVTVideoDecoderSpecification_EnableHardwareAcceleratedVideoDecoder", &a.kEnableHWDec)
	// These depend on the system version. Encoding works without them.
	constant(vt, "kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder", &a.kRequireHWEnc)
	constant(vt, "kVTCompressionPropertyKey_MaxFrameDelayCount", &a.kMaxDelay)
	constant(vt, "kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality", &a.kSpeed)
	if addr, err := dl.Sym(vt, "VTRegisterSupplementalVideoDecoderIfAvailable"); err == nil {
		purego.RegisterFunc(&a.addVP9, addr)
	}
	a.forceKeyProps = a.dict()
	a.dictSet(a.forceKeyProps, a.kForceKey, a.kTrue)
	return a, nil
})

func (a *appleAPI) dict() uintptr { return a.dictCreate(0, 0, a.keyCB, a.valCB) }

func (a *appleAPI) i32(v int32) uintptr {
	return a.numberCreate(0, cfNumInt32, unsafe.Pointer(&v))
}

func (a *appleAPI) f64(v float64) uintptr {
	return a.numberCreate(0, cfNumFloat64, unsafe.Pointer(&v))
}

// setOwned stores v under k and drops the local reference to v.
func (a *appleAPI) setOwned(d, k, v uintptr) {
	a.dictSet(d, k, v)
	a.cfRelease(v)
}

func (a *appleAPI) pixelAttrs(format int32, w, h int) uintptr {
	d := a.dict()
	a.setOwned(d, a.kPixelFormat, a.i32(format))
	a.setOwned(d, a.kIOSurface, a.dict())
	if w > 0 {
		a.setOwned(d, a.kWidth, a.i32(int32(w)))
		a.setOwned(d, a.kHeight, a.i32(int32(h)))
	}
	return d
}

func vtError(what string, status int32) error {
	if status == vtBadData || status == cmBadData {
		return fmt.Errorf("%w: videotoolbox: %s: status %d", ErrCorrupt, what, status)
	}
	return fmt.Errorf("videotoolbox: %s: status %d", what, status)
}

// --- encoder ---

type vtEncoder struct {
	c       dl.Caller
	pool    uintptr
	api     *appleAPI
	info    BackendInfo
	cfg     VideoConfig
	session uintptr
	shared  *vtShared
	head    uint64
	packets packetPool
	out     fifo[*EncodedPacket]
	flushed bool

	pixel, infoFlags uintptr
	pts, dur, until  cmTime
	base, size       uintptr
	paramFn          uintptr // reads one parameter set of a format description
	paramPtr         uintptr
	paramSize        uintptr
	paramCount       int32
	paramNAL         int32
	spsIn, spsOut    []byte // the H.265 SPS as encoded and with its range stated
}

// hevcEncoderSpec asks for the hardware encoder where the system lets a caller
// require one. The result is 0 or a dictionary owned by the caller.
func (a *appleAPI) hevcEncoderSpec() uintptr {
	if a.kRequireHWEnc == 0 {
		return 0
	}
	d := a.dict()
	a.dictSet(d, a.kRequireHWEnc, a.kTrue)
	return d
}

// hevcEncoderAvailable checks for an H.265 encoder without opening a session.
var hevcEncoderAvailable = sync.OnceValue(func() error {
	a, err := loadApple()
	if err != nil {
		return err
	}
	var c dl.Caller
	spec := a.hevcEncoderSpec()
	if spec != 0 {
		defer a.cfRelease(spec)
	}
	out := new([2]uintptr)
	rc := c.C6(a.encoderProps, 1920, 1080, fourccHEVC, spec, uintptr(unsafe.Pointer(&out[0])), uintptr(unsafe.Pointer(&out[1])))
	for _, o := range out {
		if o != 0 {
			a.cfRelease(o)
		}
	}
	if int32(rc) != 0 {
		return vtError("no hardware H.265 encoder", int32(rc))
	}
	return nil
})

func newVTEncoder(_ *Library, cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	a, err := loadApple()
	if err != nil {
		return nil, err
	}
	e := &vtEncoder{api: a, info: info, cfg: cfg, shared: &vtShared{retain: a.retain}, paramFn: a.h264Param}
	codec, format := uintptr(fourccH264), int32(pixel420v)
	if cfg.Range == RangeFull {
		format = pixel420f
	}
	var spec uintptr
	switch cfg.Codec {
	case H264:
		spec = a.dict()
		defer a.cfRelease(spec)
		a.dictSet(spec, a.kLowLatency, a.kTrue)
	case H265:
		// Low-latency rate control is H.264 only.
		codec, e.paramFn = fourccHEVC, a.hevcParam
		if spec = a.hevcEncoderSpec(); spec != 0 {
			defer a.cfRelease(spec)
		}
	default:
		codec, format = fourccJPEG, pixel420f
	}
	attrs := a.pixelAttrs(format, cfg.Width, cfg.Height)
	defer a.cfRelease(attrs)
	thunk, _ := vtThunks()
	rc := e.c.C10(a.compCreate, 0, uintptr(cfg.Width), uintptr(cfg.Height), codec, spec, attrs, 0, thunk,
		uintptr(unsafe.Pointer(e.shared)), uintptr(unsafe.Pointer(&e.session)))
	if int32(rc) != 0 {
		return nil, vtError("create encoder", int32(rc))
	}
	set := func(k, v uintptr) { a.setProperty(e.session, k, v) }
	owned := func(k, v uintptr) { set(k, v); a.cfRelease(v) }
	set(a.kRealTime, a.kTrue)
	if cfg.Codec == JPEG {
		owned(a.kQuality, a.f64(float64(cfg.Quality)/100))
	} else {
		set(a.kReorder, a.kFalse)
		if cfg.Codec == H265 {
			set(a.kProfile, a.kProfileHEVC)
			if a.kMaxDelay != 0 {
				owned(a.kMaxDelay, a.i32(0))
			}
			if a.kSpeed != 0 {
				set(a.kSpeed, a.kTrue)
			}
		} else {
			set(a.kProfile, a.kProfileValue)
		}
		// The H.265 encoder picks its clocks from this hint. At 1080p60 a hint of
		// 240 cuts the encode time from 6.9 ms to 3.7 ms and p99 from 11.5 ms to
		// 4.6 ms, at the same bitrate and within 0.2 dB. Rate control follows the
		// timestamps, so the hint does not change the bitrate target.
		rate := int32(cfg.FPS)
		if cfg.Codec == H265 {
			rate = max(rate, vtHEVCRateHint)
		} else {
			// H.264 with low-latency rate control stops producing output when the
			// hint asks for more pixels per second than this. The encoder can
			// keep up with more, so only the hint is limited.
			rate = min(rate, max(int32(vtH264MaxPixelRate/(cfg.Width*cfg.Height)), 1))
		}
		owned(a.kFPS, a.i32(rate))
		owned(a.kBitrate, a.i32(int32(cfg.Bitrate)))
		owned(a.kMaxKey, a.i32(int32(cfg.KeyframeEvery)))
	}
	if st := a.compPrepare(e.session); st != 0 {
		e.Close()
		return nil, vtError("prepare encoder", st)
	}
	e.dur = cmTime{}
	return e, nil
}

func (e *vtEncoder) upload(f *VideoFrame) error {
	a := e.api
	if e.pool == 0 {
		if e.pool = a.compPool(e.session); e.pool == 0 {
			return errors.New("videotoolbox: encoder has no pixel buffer pool")
		}
	}
	if rc := e.c.C3(a.poolCreate, 0, e.pool, uintptr(unsafe.Pointer(&e.pixel))); int32(rc) != 0 {
		return vtError("allocate pixel buffer", int32(rc))
	}
	if rc := e.c.C2(a.lockBase, e.pixel, 0); int32(rc) != 0 {
		e.c.C1(a.release, e.pixel)
		return vtError("lock pixel buffer", int32(rc))
	}
	y := e.c.C2(a.planeBase, e.pixel, 0)
	uv := e.c.C2(a.planeBase, e.pixel, 1)
	ys := int(e.c.C2(a.planeStride, e.pixel, 0))
	us := int(e.c.C2(a.planeStride, e.pixel, 1))
	h := f.Height
	copyPlane(cMem(y, ys*h), ys, f.Y, f.YStride, f.Width, h)
	writeChroma(cMem(uv, us*h/2), us, f)
	e.c.C2(a.unlockBase, e.pixel, 0)
	return nil
}

func (e *vtEncoder) Send(f *VideoFrame) error {
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	if err := e.upload(f); err != nil {
		return err
	}
	a := e.api
	e.pts = cmTime{value: int64(f.PTS), timescale: vtTimescale, flags: cmTimeValid}
	var props uintptr
	if f.ForceKeyframe {
		props = a.forceKeyProps
	}
	rc := e.c.C7(a.encodeFrame, e.session, e.pixel, uintptr(unsafe.Pointer(&e.pts)), uintptr(unsafe.Pointer(&e.dur)),
		props, uintptr(f.PTS), uintptr(unsafe.Pointer(&e.infoFlags)))
	e.c.C1(a.release, e.pixel)
	if int32(rc) != 0 {
		return vtError("encode", int32(rc))
	}
	// Without reordering this returns as soon as the frame is encoded.
	if rc := e.c.C2(a.complete, e.session, uintptr(unsafe.Pointer(&e.until))); int32(rc) != 0 {
		return vtError("complete", int32(rc))
	}
	return e.collect()
}

func (e *vtEncoder) collect() error {
	s := e.shared
	for tail := s.tail.Load(); e.head != tail; e.head++ {
		o := &s.ring[e.head&vtRingMask]
		var err error
		switch {
		case o.status != 0:
			err = vtError("encoder output", o.status)
		case o.obj != 0:
			err = e.packet(o.obj, time.Duration(o.ref))
		}
		if o.obj != 0 {
			e.c.C1(e.api.release, o.obj)
			o.obj = 0
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *vtEncoder) isKeyframe(sample uintptr) bool {
	a := e.api
	arr := e.c.C2(a.sampleAttach, sample, 0)
	if arr == 0 || e.c.C1(a.arrayCount, arr) == 0 {
		return true
	}
	d := e.c.C2(a.arrayAt, arr, 0)
	v := e.c.C2(a.dictGet, d, a.kNotSync)
	return v == 0 || v == a.kFalse
}

func (e *vtEncoder) param(desc, i uintptr) (int, error) {
	rc := e.c.C6(e.paramFn, desc, i, uintptr(unsafe.Pointer(&e.paramPtr)), uintptr(unsafe.Pointer(&e.paramSize)),
		uintptr(unsafe.Pointer(&e.paramCount)), uintptr(unsafe.Pointer(&e.paramNAL)))
	if int32(rc) != 0 {
		return 0, vtError("read parameter set", int32(rc))
	}
	return int(e.paramCount), nil
}

// paramSet reads parameter set i. The H.265 encoder leaves video_signal_type
// out of the SPS for limited range, so a requested range is written into it.
// The result is valid until the next call.
func (e *vtEncoder) paramSet(desc, i uintptr) ([]byte, error) {
	if _, err := e.param(desc, i); err != nil {
		return nil, err
	}
	nal := cMem(e.paramPtr, int(e.paramSize))
	if e.cfg.Codec != H265 || e.cfg.Range == RangeUnspecified || len(nal) < 2 || nal[0]>>1&0x3F != 33 {
		return nal, nil
	}
	if !bytes.Equal(nal, e.spsIn) {
		e.spsIn = append(e.spsIn[:0], nal...)
		var ok bool
		if e.spsOut, ok = appendH265SignalRange(e.spsOut[:0], nal, e.cfg.Range); !ok {
			e.spsOut = append(e.spsOut[:0], nal...)
		}
	}
	return e.spsOut, nil
}

func (e *vtEncoder) packet(sample uintptr, pts time.Duration) error {
	a := e.api
	block := e.c.C1(a.sampleData, sample)
	if block == 0 {
		return nil // the encoder dropped this frame
	}
	if rc := e.c.C5(a.blockPointer, block, 0, 0, uintptr(unsafe.Pointer(&e.size)), uintptr(unsafe.Pointer(&e.base))); int32(rc) != 0 {
		return vtError("read sample", int32(rc))
	}
	data := cMem(e.base, int(e.size))
	key := e.isKeyframe(sample)
	if e.cfg.Codec == JPEG {
		p := e.packets.get(len(data))
		copy(p.Data, data)
		p.PTS, p.Keyframe = pts, true
		e.out.push(p)
		return nil
	}
	prefix := 0
	desc := e.c.C1(a.sampleFormat, sample)
	count := 0
	if key {
		var err error
		if count, err = e.param(desc, 0); err != nil {
			return err
		}
		for i := 0; i < count; i++ {
			nal, err := e.paramSet(desc, uintptr(i))
			if err != nil {
				return err
			}
			prefix += 4 + len(nal)
		}
	}
	p := e.packets.get(prefix + len(data))
	at := 0
	for i := 0; i < count; i++ {
		nal, err := e.paramSet(desc, uintptr(i))
		if err != nil {
			p.Release()
			return err
		}
		copy(p.Data[at:], annexBStart[:])
		copy(p.Data[at+4:], nal)
		at += 4 + len(nal)
	}
	body := p.Data[at:]
	copy(body, data)
	for i := 0; i+4 <= len(body); {
		n := int(body[i])<<24 | int(body[i+1])<<16 | int(body[i+2])<<8 | int(body[i+3])
		copy(body[i:], annexBStart[:])
		i += 4 + n
	}
	p.PTS, p.Keyframe = pts, key
	e.out.push(p)
	return nil
}

var annexBStart = [4]byte{0, 0, 0, 1}

func (e *vtEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *vtEncoder) Flush() error {
	if e.flushed {
		return nil
	}
	e.flushed = true
	if rc := e.c.C2(e.api.complete, e.session, uintptr(unsafe.Pointer(&e.until))); int32(rc) != 0 {
		return vtError("complete", int32(rc))
	}
	return e.collect()
}

func (e *vtEncoder) Backend() BackendInfo { return e.info }

// SetBitrate implements BitrateSetter.
func (e *vtEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.session == 0 {
		return ErrClosed
	}
	if e.cfg.Codec == JPEG {
		return errBitrateUnsupported(e.info.Name)
	}
	v := e.api.i32(int32(min(bps, math.MaxInt32)))
	st := e.api.setProperty(e.session, e.api.kBitrate, v)
	e.api.cfRelease(v)
	if st != 0 {
		return vtError("set bitrate", st)
	}
	e.cfg.Bitrate = bps
	return nil
}

func (e *vtEncoder) Close() error {
	if e.session != 0 {
		e.api.compInvalid(e.session)
		e.api.cfRelease(e.session)
		e.session = 0
		e.collect()
	}
	for {
		p, ok := e.out.pop()
		if !ok {
			return nil
		}
		p.Release()
	}
}

// pixelForRange picks the output format that leaves samples unconverted.
func pixelForRange(r ColorRange) int32 {
	if r == RangeFull {
		return pixel420f
	}
	return pixel420v
}

// --- decoder ---

type vtDecoder struct {
	c       dl.Caller
	api     *appleAPI
	info    BackendInfo
	session uintptr
	desc    uintptr
	format  int32 // pixel format of the session

	// A hardware decoder that fails mid-stream is replaced by a software one.
	// fault holds the failure until Send or Receive handles it; needKey drops
	// packets until a keyframe, as the new session has no references.
	fault    int32
	soft     bool
	needKey  bool
	hardware bool // the backend's own Hardware, before any fallback
	restarts int
	shared   *vtShared
	head     uint64
	frames   FramePool
	out      fifo[*VideoFrame]
	flushed  bool
	err      error

	record   [2]uintptr
	sps, pps []byte
	au       []byte
	jpegW    int
	jpegH    int

	// H.265 parameter sets, each with a 4-byte length: those of the session,
	// and those being collected from the current packet.
	hevcSets, hevcNext []byte

	vp9W, vp9H int
	vp9Full    bool
	// The picture in each VP9 reference slot, retained, for
	// show_existing_frame. nil where it is unknown, as for a hidden frame.
	vp9Slots [8]*VideoFrame
	lastPic  *VideoFrame // the latest picture output, while Send runs

	// VideoToolbox returns pictures in decode order. They wait here until the
	// stream's reorder depth proves that none can precede them. With lowDelay
	// they do not wait.
	lowDelay bool
	reorder  int
	held     []*VideoFrame

	sampleSize, block, sample, infoFlags uintptr
	setPtrs                              [vtMaxParamSets]unsafe.Pointer
	setSizes                             [vtMaxParamSets]uintptr
}

func newVTDecoder(_ *Library, cfg VideoConfig, info BackendInfo) (VideoDecoder, error) {
	a, err := loadApple()
	if err != nil {
		return nil, err
	}
	d := &vtDecoder{api: a, info: info, hardware: info.Hardware, shared: &vtShared{retain: a.retain}, lowDelay: cfg.LowDelay}
	d.record[0], _ = vtThunks()
	d.record[1] = uintptr(unsafe.Pointer(d.shared))
	return d, nil
}

// nextNAL finds the NAL unit at or after pos in an Annex B stream.
func nextNAL(b []byte, pos int) (start, end int, ok bool) {
	i := bytes.Index(b[pos:], annexBStart[1:])
	if i < 0 {
		return 0, 0, false
	}
	start = pos + i + 3
	j := bytes.Index(b[start:], annexBStart[1:])
	if j < 0 {
		return start, len(b), true
	}
	end = start + j
	for end > start && b[end-1] == 0 {
		end--
	}
	return start, end, true
}

func (d *vtDecoder) dropSession() {
	a := d.api
	if d.session != 0 {
		d.c.C2(a.decWait, d.session, 0)
		a.decInvalid(d.session)
		a.cfRelease(d.session)
		d.session = 0
	}
	if d.desc != 0 {
		a.cfRelease(d.desc)
		d.desc = 0
	}
}

func (d *vtDecoder) openSession(format int32) error {
	a := d.api
	d.format = format
	attrs := a.pixelAttrs(format, 0, 0)
	defer a.cfRelease(attrs)
	var spec uintptr
	switch {
	case d.soft && a.kEnableHWDec != 0:
		spec = a.dict()
		defer a.cfRelease(spec)
		a.dictSet(spec, a.kEnableHWDec, a.kFalse)
	case d.info.Hardware && a.kRequireHWDec != 0:
		spec = a.dict()
		defer a.cfRelease(spec)
		a.dictSet(spec, a.kRequireHWDec, a.kTrue)
	}
	if st := a.decCreate(0, d.desc, spec, attrs, unsafe.Pointer(&d.record[0]), &d.session); st != 0 {
		d.session = 0
		return vtError("create decoder", st)
	}
	return nil
}

func (d *vtDecoder) configureH264(sps, pps []byte) error {
	if d.session != 0 && bytes.Equal(sps, d.sps) && bytes.Equal(pps, d.pps) {
		return nil
	}
	d.drainSession()
	d.dropSession()
	d.sps, d.pps = append(d.sps[:0], sps...), append(d.pps[:0], pps...)
	d.setPtrs[0], d.setPtrs[1] = unsafe.Pointer(&d.sps[0]), unsafe.Pointer(&d.pps[0])
	d.setSizes[0], d.setSizes[1] = uintptr(len(d.sps)), uintptr(len(d.pps))
	d.reorder = h264NALReorder(d.sps)
	if st := d.api.h264Format(0, 2, unsafe.Pointer(&d.setPtrs), unsafe.Pointer(&d.setSizes), 4, &d.desc); st != 0 {
		d.desc = 0
		return fmt.Errorf("%w: videotoolbox: bad parameter sets: status %d", ErrCorrupt, st)
	}
	return d.openSession(pixelForRange(nalSPSRange(H264, sps)))
}

// configureH265 opens a session for sets, which holds count length-prefixed
// parameter sets. An unchanged stream keeps its session.
func (d *vtDecoder) configureH265(sets []byte, count int) error {
	if d.session != 0 && bytes.Equal(sets, d.hevcSets) {
		return nil
	}
	d.drainSession()
	d.dropSession()
	d.hevcSets = append(d.hevcSets[:0], sets...)
	b := d.hevcSets
	d.reorder = 0
	spsRange := RangeUnspecified
	for i, at := 0, 0; i < count; i++ {
		n := int(binary.BigEndian.Uint32(b[at:]))
		nal := b[at+4 : at+4+n]
		d.setPtrs[i], d.setSizes[i] = unsafe.Pointer(&nal[0]), uintptr(n)
		if nal[0]>>1&0x3F == 33 {
			d.reorder = max(d.reorder, hevcReorderDepth(nal))
			spsRange = nalSPSRange(H265, nal)
		}
		at += 4 + n
	}
	st := d.api.hevcFormat(0, uintptr(count), unsafe.Pointer(&d.setPtrs), unsafe.Pointer(&d.setSizes), 4, 0, &d.desc)
	if st != 0 {
		d.desc = 0
		return fmt.Errorf("%w: videotoolbox: bad H.265 parameter sets: status %d", ErrCorrupt, st)
	}
	return d.openSession(pixelForRange(spsRange))
}

// hevcReorderDepth reads sps_max_num_reorder_pics of the highest sub-layer from
// an SPS NAL unit: how many pictures can precede another in decode order and
// follow it in output order. It returns 0 for an SPS it cannot read.
func hevcReorderDepth(nal []byte) int {
	rbsp := make([]byte, 0, min(len(nal), 256))
	for i, zeros := 2, 0; i < len(nal) && len(rbsp) < 256; i++ {
		if zeros >= 2 && nal[i] == 3 { // emulation prevention
			zeros = 0
			continue
		}
		if nal[i] == 0 {
			zeros++
		} else {
			zeros = 0
		}
		rbsp = append(rbsp, nal[i])
	}
	pos, bad := 0, false
	u := func(n int) int {
		v := 0
		for ; n > 0; n-- {
			if pos>>3 >= len(rbsp) {
				bad = true
				return 0
			}
			v = v<<1 | int(rbsp[pos>>3]>>(7-pos&7))&1
			pos++
		}
		return v
	}
	ue := func() int {
		zeros := 0
		for !bad && u(1) == 0 {
			if zeros++; zeros > 31 {
				bad = true
			}
		}
		return 1<<zeros - 1 + u(zeros)
	}
	u(4)
	sub := u(3) // sps_max_sub_layers_minus1
	u(1)
	pos += 96 // general profile, tier and level
	var profile, level [8]bool
	for i := 0; i < sub; i++ {
		profile[i], level[i] = u(1) == 1, u(1) == 1
	}
	if sub > 0 {
		pos += 2 * (8 - sub)
	}
	for i := 0; i < sub; i++ {
		if profile[i] {
			pos += 88
		}
		if level[i] {
			pos += 8
		}
	}
	ue() // sps_seq_parameter_set_id
	if ue() == 3 {
		u(1)
	}
	ue() // width
	ue() // height
	if u(1) == 1 {
		ue()
		ue()
		ue()
		ue()
	}
	ue() // bit depth, luma
	ue() // bit depth, chroma
	ue() // log2_max_pic_order_cnt_lsb_minus4
	first := sub
	if u(1) == 1 { // sps_sub_layer_ordering_info_present_flag
		first = 0
	}
	depth := 0
	for i := first; i <= sub; i++ {
		ue()
		depth = ue()
		ue()
	}
	if bad || depth < 0 || depth > 16 {
		return 0
	}
	return depth
}

// prepareH265 rewrites an access unit as length-prefixed NAL units. A packet
// with a VPS, SPS and PPS configures the decoder. Other parameter sets stay in
// the sample, ahead of the pictures.
func (d *vtDecoder) prepareH265(data []byte) ([]byte, error) {
	au, sets := d.au[:0], d.hevcNext[:0]
	var seen uint8
	count, units := 0, 0
	for pos := 0; ; {
		s, e, ok := nextNAL(data, pos)
		if !ok {
			break
		}
		pos = e
		if e-s < 2 {
			continue
		}
		units++
		n := e - s
		switch t := data[s] >> 1 & 0x3F; t {
		case 32, 33, 34: // VPS, SPS, PPS
			seen |= 1 << (t - 32)
			count++
			sets = append(sets, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
			sets = append(sets, data[s:e]...)
		case 35, 36, 37, 38: // delimiter, end of sequence, end of stream, filler
		default:
			au = append(au, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
			au = append(au, data[s:e]...)
		}
	}
	d.au, d.hevcNext = au, sets
	if units == 0 {
		return nil, fmt.Errorf("%w: videotoolbox: no H.265 NAL unit in packet", ErrCorrupt)
	}
	switch {
	case seen == 7 && count <= vtMaxParamSets:
		if err := d.configureH265(sets, count); err != nil {
			return nil, err
		}
	case seen == 7:
		return nil, fmt.Errorf("%w: videotoolbox: %d H.265 parameter sets", ErrCorrupt, count)
	case seen != 0 && len(au) > 0:
		au = append(sets[:len(sets):len(sets)], au...)
	}
	if len(au) == 0 {
		return nil, nil
	}
	if d.session == 0 {
		return nil, fmt.Errorf("%w: videotoolbox: picture before parameter sets", ErrCorrupt)
	}
	return au, nil
}

// configureVP9 opens a session for the stream of keyframe f. VideoToolbox takes
// the stream properties from a vpcC box, as stored in an MP4 sample entry.
func (d *vtDecoder) configureVP9(f vp9Frame) error {
	if d.session != 0 && f.Width == d.vp9W && f.Height == d.vp9H && f.FullRange == d.vp9Full {
		return nil
	}
	d.drainSession()
	d.dropSession()
	a := d.api
	const unspecified = 2
	full := byte(0)
	if f.FullRange {
		full = 1
	}
	const colocated420 = 1
	box := [12]byte{1, 0, 0, 0, byte(f.Profile), 0, 8<<4 | colocated420<<1 | full, unspecified, unspecified, unspecified}
	blob := a.dataCreate(0, unsafe.Pointer(&box), len(box))
	key := a.stringCreate(0, "vpcC", cfStringUTF8)
	atoms, ext := a.dict(), a.dict()
	a.dictSet(atoms, key, blob)
	a.dictSet(ext, a.kAtoms, atoms)
	st := a.formatCreate(0, fourccVP9, int32(f.Width), int32(f.Height), ext, &d.desc)
	for _, o := range []uintptr{ext, atoms, key, blob} {
		a.cfRelease(o)
	}
	if st != 0 {
		d.desc = 0
		return vtError("vp9 format", st)
	}
	d.vp9W, d.vp9H, d.vp9Full = f.Width, f.Height, f.FullRange
	r := RangeLimited
	if f.FullRange {
		r = RangeFull
	}
	return d.openSession(pixelForRange(r))
}

// prepareVP9 checks a frame or superframe and configures the decoder at each
// keyframe. Only 8-bit 4:2:0 is requested, other profiles go to another backend.
func (d *vtDecoder) prepareVP9(data []byte) ([]byte, error) {
	f, ok := parseVP9Frame(data)
	if !ok {
		return nil, fmt.Errorf("%w: videotoolbox: malformed VP9 frame", ErrCorrupt)
	}
	if f.Key {
		if f.Profile != 0 {
			return nil, fmt.Errorf("videotoolbox: VP9 profile %d is not supported", f.Profile)
		}
		if err := d.configureVP9(f); err != nil {
			return nil, err
		}
	}
	if d.session == 0 {
		return nil, fmt.Errorf("%w: videotoolbox: VP9 stream must start with a keyframe", ErrCorrupt)
	}
	return data, nil
}

// jpegSize reads the frame size from a JPEG start-of-frame marker.
func jpegSize(b []byte) (w, h int, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, 0, false
	}
	for i := 2; i+9 < len(b); {
		if b[i] != 0xFF {
			return 0, 0, false
		}
		m := b[i+1]
		if m == 0xFF {
			i++
			continue
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			return int(b[i+7])<<8 | int(b[i+8]), int(b[i+5])<<8 | int(b[i+6]), true
		}
		i += 2 + (int(b[i+2])<<8 | int(b[i+3]))
	}
	return 0, 0, false
}

func (d *vtDecoder) configureJPEG(w, h int) error {
	if d.session != 0 && w == d.jpegW && h == d.jpegH {
		return nil
	}
	d.drainSession()
	d.dropSession()
	d.jpegW, d.jpegH = w, h
	if st := d.api.formatCreate(0, fourccJPEG, int32(w), int32(h), 0, &d.desc); st != 0 {
		d.desc = 0
		return vtError("jpeg format", st)
	}
	return d.openSession(pixel420f)
}

// prepareH264 rewrites an access unit as length-prefixed NAL units.
func (d *vtDecoder) prepareH264(data []byte) ([]byte, error) {
	au := d.au[:0]
	var sps, pps []byte
	for pos := 0; ; {
		s, e, ok := nextNAL(data, pos)
		if !ok {
			break
		}
		pos = e
		if s >= e {
			continue
		}
		switch data[s] & 0x1F {
		case 7:
			sps = data[s:e]
		case 8:
			pps = data[s:e]
		case 9: // access unit delimiter
		default:
			n := e - s
			au = append(au, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
			au = append(au, data[s:e]...)
		}
	}
	d.au = au
	if sps != nil && pps != nil {
		if err := d.configureH264(sps, pps); err != nil {
			return nil, err
		}
	}
	if len(au) == 0 {
		return nil, nil
	}
	if d.session == 0 {
		return nil, fmt.Errorf("%w: videotoolbox: picture before parameter sets", ErrCorrupt)
	}
	return au, nil
}

func (d *vtDecoder) Send(p Packet) error {
	if len(p.Data) == 0 {
		return fmt.Errorf("%w: empty packet", ErrInvalid)
	}
	var data []byte
	var err error
	switch d.info.Codec {
	case H264:
		data, err = d.prepareH264(p.Data)
	case H265:
		data, err = d.prepareH265(p.Data)
	case VP9:
		data, err = d.prepareVP9(p.Data)
	default:
		w, h, ok := jpegSize(p.Data)
		if !ok {
			return fmt.Errorf("%w: videotoolbox: not a JPEG image", ErrCorrupt)
		}
		data, err = p.Data, d.configureJPEG(w, h)
	}
	if err != nil || data == nil {
		return err
	}
	if d.info.Codec == VP9 {
		if f, ok := parseVP9Frame(p.Data); ok && f.ShowExisting {
			return d.showExisting(f.ShowIdx, p.PTS)
		}
		d.lastPic = nil
		defer d.noteVP9Refs(p.Data)
	}
	key := vtIsKey(d.info.Codec, p.Data)
	if d.needKey {
		if !key {
			return nil // no references for it in the new session
		}
		d.needKey = false
	}
	for retried := false; ; retried = true {
		rc, err := d.decode(data, p.PTS)
		runtime.KeepAlive(p.Data)
		if d.isFault(rc) && d.fault == 0 {
			d.fault = rc
		}
		if d.fault == 0 {
			if rc != 0 {
				return vtError("decode", rc)
			}
			return err
		}
		// A keyframe is decoded again by the software session, so nothing is
		// lost. Otherwise the stream resumes at the next keyframe.
		if ferr := d.takeFault(); !errors.Is(ferr, errHWFallback) || !key || retried {
			return ferr
		}
		d.needKey = false
	}
}

// decode submits one access unit and collects what the session returns.
func (d *vtDecoder) decode(data []byte, pts time.Duration) (int32, error) {
	a := d.api
	size := uintptr(len(data))
	ptr := uintptr(unsafe.Pointer(&data[0]))
	rc := d.c.C10(a.blockCreate, 0, ptr, size, a.kAllocNull, 0, 0, size, 0, uintptr(unsafe.Pointer(&d.block)), 0)
	if int32(rc) != 0 {
		return 0, vtError("wrap data", int32(rc))
	}
	d.sampleSize = size
	rc = d.c.C10(a.sampleCreate, 0, d.block, d.desc, 1, 0, 0, 1, uintptr(unsafe.Pointer(&d.sampleSize)), uintptr(unsafe.Pointer(&d.sample)), 0)
	d.c.C1(a.release, d.block)
	if int32(rc) != 0 {
		return 0, vtError("wrap sample", int32(rc))
	}
	rc = d.c.C5(a.decodeFrame, d.session, d.sample, 0, uintptr(pts), uintptr(unsafe.Pointer(&d.infoFlags)))
	d.c.C1(a.release, d.sample)
	runtime.KeepAlive(data)
	return int32(rc), d.collect()
}

// showExisting outputs the picture in a reference slot again. VideoToolbox
// does not do it everywhere: on some systems it returns nothing for such a
// frame, which carries no picture data.
func (d *vtDecoder) showExisting(idx int, pts time.Duration) error {
	src := d.vp9Slots[idx]
	if src == nil {
		return fmt.Errorf("%w: videotoolbox: VP9 show_existing_frame of slot %d, which holds no picture", ErrCorrupt, idx)
	}
	f := d.frames.Get(src.Width, src.Height, src.Format)
	src.CopyTo(f)
	f.PTS = pts
	d.out.push(f)
	return nil
}

// noteVP9Refs records which reference slots the frames of a packet replaced.
// A shown frame leaves the picture Send produced; a hidden one is unknown.
func (d *vtDecoder) noteVP9Refs(b []byte) {
	var subs [8][]byte
	for _, sub := range subs[:vp9Subframes(b, &subs)] {
		f, ok := parseVP9Frame(sub)
		if !ok || f.ShowExisting {
			continue
		}
		pic := d.lastPic
		if !f.Show {
			pic = nil
		}
		for i := range d.vp9Slots {
			if f.Refresh&(1<<i) == 0 || d.vp9Slots[i] == pic {
				continue
			}
			if d.vp9Slots[i] != nil {
				d.vp9Slots[i].Release()
			}
			if d.vp9Slots[i] = pic; pic != nil {
				pic.Retain()
			}
		}
	}
	d.lastPic = nil
}

// vtHardwareFault reports a status that means the decoder itself failed:
// IOKit errors from the media engine, a malfunction, or a session the system
// invalidated.
func vtHardwareFault(st int32) bool {
	return st == vtInvalidSession || st == vtMalfunction || uint32(st)>>12 == 0xE0000
}

// isFault reports a status that a software session may not share. For JPEG
// that is any failure but bad data: the hardware decoder of virtual Macs
// rejects small images, such as 33x17, with -12916, and every image stands
// alone, so it is decoded again at no loss.
func (d *vtDecoder) isFault(st int32) bool {
	if d.info.Codec == JPEG && !d.soft {
		return st != 0 && st != vtBadData && st != cmBadData
	}
	return vtHardwareFault(st)
}

// takeFault handles a recorded hardware fault. The first one moves the
// decoder to a software session and returns an error wrapping errHWFallback
// and ErrCorrupt; with no software decoder, or a fault in software, the
// stream fails as before.
func (d *vtDecoder) takeFault() error {
	st := d.fault
	d.fault = 0
	if d.restarts >= vtMaxRestarts {
		return vtError("decoder failed", st)
	}
	d.restarts++
	for len(d.held) > 0 {
		d.releaseEarliest()
	}
	a := d.api
	if d.session != 0 {
		a.decInvalid(d.session)
		a.cfRelease(d.session)
		d.session = 0
	}
	how := "restarted"
	if !d.soft && a.kEnableHWDec != 0 {
		d.soft, d.info.Hardware = true, false
		if d.desc == 0 || d.openSession(d.format) == nil {
			d.needKey = true
			return fmt.Errorf("%w (videotoolbox status %d)", errHWFallback, st)
		}
		// No software decoder for this codec, such as VP9: start the
		// hardware one afresh instead.
		d.soft, d.info.Hardware = false, d.hardware
		how = "restarted in hardware, there is no software decoder"
	}
	if d.desc != 0 {
		if err := d.openSession(d.format); err != nil {
			return fmt.Errorf("videotoolbox: decoder failed with status %d and cannot be %s: %w", st, how, err)
		}
	}
	d.needKey = true
	return fmt.Errorf("%w (videotoolbox status %d, %s)", errHWFallback, st, how)
}

// vtMaxRestarts bounds how often one decoder recovers from failures, so a
// decoder that keeps failing reports it.
const vtMaxRestarts = 4

// vtIsKey reports whether a packet starts a stream without references.
func vtIsKey(c Codec, b []byte) bool {
	switch c {
	case JPEG:
		return true
	case VP9:
		f, ok := parseVP9Frame(b)
		return ok && f.Key
	}
	for pos := 0; ; {
		start, end, ok := nextNAL(b, pos)
		if !ok {
			return false
		}
		if start < len(b) {
			if c == H264 && b[start]&0x1F == 5 {
				return true
			}
			if c == H265 {
				if t := b[start] >> 1 & 0x3F; t >= 16 && t <= 21 {
					return true
				}
			}
		}
		pos = max(end, start)
		if pos >= len(b) {
			return false
		}
	}
}

func (d *vtDecoder) drainSession() {
	if d.session != 0 {
		d.c.C2(d.api.decWait, d.session, 0)
		if err := d.collect(); err != nil && d.err == nil {
			d.err = err
		}
	}
	for len(d.held) > 0 {
		d.releaseEarliest()
	}
}

// releaseEarliest moves the held picture with the lowest timestamp to the output.
func (d *vtDecoder) releaseEarliest() {
	best := 0
	for i, f := range d.held {
		if f.PTS < d.held[best].PTS {
			best = i
		}
	}
	d.lastPic = d.held[best]
	d.out.push(d.held[best])
	n := copy(d.held[best:], d.held[best+1:])
	d.held[best+n] = nil
	d.held = d.held[:best+n]
}

func (d *vtDecoder) collect() error {
	s := d.shared
	var first error
	for tail := s.tail.Load(); d.head != tail; d.head++ {
		o := &s.ring[d.head&vtRingMask]
		var err error
		switch {
		case d.isFault(o.status):
			if d.fault == 0 {
				d.fault = o.status
			}
		case o.status != 0:
			err = vtError("decoder output", o.status)
		case o.obj != 0 && o.flags&vtFrameDamage == 0:
			d.picture(o.obj, time.Duration(o.ref))
		}
		if o.obj != 0 {
			d.c.C1(d.api.release, o.obj)
			o.obj = 0
		}
		if first == nil {
			first = err
		}
	}
	return first
}

func (d *vtDecoder) picture(pix uintptr, pts time.Duration) {
	a := d.api
	if int32(d.c.C2(a.lockBase, pix, vtLockRead)) != 0 {
		return
	}
	w, h := int(d.c.C1(a.pixW, pix)), int(d.c.C1(a.pixH, pix))
	f := d.frames.Get(w, h, NV12)
	f.PTS = pts
	ys := int(d.c.C2(a.planeStride, pix, 0))
	us := int(d.c.C2(a.planeStride, pix, 1))
	copyPlane(f.Y, f.YStride, cMem(d.c.C2(a.planeBase, pix, 0), ys*h), ys, w, h)
	ch := (h + 1) / 2
	copyPlane(f.U, f.UVStride, cMem(d.c.C2(a.planeBase, pix, 1), us*ch), us, f.UVStride, ch)
	d.c.C2(a.unlockBase, pix, vtLockRead)
	if d.reorder == 0 || d.lowDelay {
		d.lastPic = f
		d.out.push(f)
		return
	}
	d.held = append(d.held, f)
	if len(d.held) > d.reorder {
		d.releaseEarliest()
	}
}

func (d *vtDecoder) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	// A decoder may finish after Send has returned.
	if err := d.collect(); err != nil && d.err == nil {
		d.err = err
	}
	if d.fault != 0 {
		if err := d.takeFault(); d.err == nil {
			d.err = err
		}
	}
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	if d.err != nil {
		err := d.err
		d.err = nil
		return nil, err
	}
	if d.flushed {
		return nil, io.EOF
	}
	return nil, ErrAgain
}

func (d *vtDecoder) Flush() error {
	d.flushed = true
	d.drainSession()
	return nil
}

func (d *vtDecoder) Backend() BackendInfo { return d.info }

func (d *vtDecoder) Close() error {
	d.drainSession()
	d.dropSession()
	for i, f := range d.vp9Slots {
		if f != nil {
			f.Release()
			d.vp9Slots[i] = nil
		}
	}
	for {
		f, ok := d.out.pop()
		if !ok {
			return nil
		}
		f.Release()
	}
}

// --- Opus through AudioToolbox ---

const (
	auNoData      = 100 // returned by the input thunk when it has no packet
	auBitrateProp = 0x62726174
	auFlagInt     = 1<<2 | 1<<3 // signed integer, packed
	opusFrames20  = 50          // frames per second of 20 ms packets
)

type asbd struct {
	sampleRate                                     float64
	format, flags, bytesPerPacket, framesPerPacket uint32
	bytesPerFrame, channels, bits, reserved        uint32
}

type auPacketDesc struct {
	offset         int64
	variableFrames uint32
	dataByteSize   uint32
}

// auShared is read by auInputThunk. A nil-free single packet is supplied per
// fill call.
type auShared struct {
	avail, packets, channels, bytes uint32
	data                            uintptr
	desc                            auPacketDesc
}

type auBufferList struct {
	count, _        uint32
	channels, bytes uint32
	data            uintptr
}

type auConverter struct {
	c       dl.Caller
	api     *appleAPI
	conv    uintptr
	thunk   uintptr
	input   auShared
	list    auBufferList
	packets uint32
	desc    auPacketDesc
	cfg     AudioConfig
}

func (c *auConverter) open(a *appleAPI, cfg AudioConfig, in, out *asbd) error {
	c.api, c.cfg = a, cfg
	_, c.thunk = vtThunks()
	if st := a.audioNew(unsafe.Pointer(in), unsafe.Pointer(out), &c.conv); st != 0 {
		return fmt.Errorf("audiotoolbox: create converter: status %d", st)
	}
	return nil
}

// fill runs the converter once. A status of auNoData is the normal end of input.
func (c *auConverter) fill(dst []byte, descs uintptr) (uint32, error) {
	c.list = auBufferList{count: 1, channels: uint32(c.cfg.Channels), bytes: uint32(len(dst)), data: uintptr(unsafe.Pointer(&dst[0]))}
	r := c.c.C6(c.api.audioFill, c.conv, c.thunk, uintptr(unsafe.Pointer(&c.input)),
		uintptr(unsafe.Pointer(&c.packets)), uintptr(unsafe.Pointer(&c.list)), descs)
	runtime.KeepAlive(dst)
	if st := int32(r); st != 0 && st != auNoData {
		return 0, fmt.Errorf("audiotoolbox: convert: status %d", st)
	}
	return c.packets, nil
}

func (c *auConverter) Close() error {
	if c.conv != 0 {
		c.api.audioDispose(c.conv)
		c.conv = 0
	}
	return nil
}

func pcmFormat(cfg AudioConfig) asbd {
	size := uint32(2 * cfg.Channels)
	return asbd{sampleRate: float64(cfg.SampleRate), format: fourccPCM, flags: auFlagInt, bytesPerPacket: size,
		framesPerPacket: 1, bytesPerFrame: size, channels: uint32(cfg.Channels), bits: 16}
}

type auDecoder struct {
	auConverter
	info    BackendInfo
	frames  audioPool
	out     fifo[*AudioFrame]
	flushed bool
	last    int
	lost    [1]byte
}

func newAUDecoder(_ *Library, cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	a, err := loadApple()
	if err != nil {
		return nil, err
	}
	d := &auDecoder{info: info}
	in := asbd{sampleRate: float64(cfg.SampleRate), format: fourccOpus, framesPerPacket: uint32(cfg.SampleRate / opusFrames20), channels: uint32(cfg.Channels)}
	out := pcmFormat(cfg)
	if err := d.open(a, cfg, &in, &out); err != nil {
		return nil, err
	}
	d.lost[0] = 0xF8
	if cfg.Channels == 2 {
		d.lost[0] = 0xFC
	}
	return d, nil
}

func (d *auDecoder) Send(p Packet) error {
	data := p.Data
	if len(data) == 0 {
		data = d.lost[:] // a zero-length Opus frame makes the decoder conceal
	}
	s := &d.input
	s.avail, s.packets, s.channels, s.bytes = 1, 1, uint32(d.cfg.Channels), uint32(len(data))
	s.data = uintptr(unsafe.Pointer(&data[0]))
	s.desc = auPacketDesc{dataByteSize: uint32(len(data))}
	maxFrames := d.cfg.SampleRate * 120 / 1000
	f := d.frames.get(maxFrames * d.cfg.Channels)
	d.packets = uint32(maxFrames)
	dst := unsafe.Slice((*byte)(unsafe.Pointer(&f.Samples[0])), 2*len(f.Samples))
	n, err := d.fill(dst, 0)
	runtime.KeepAlive(data)
	lost := len(p.Data) == 0
	if err != nil && !lost {
		f.Release()
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if lost && (err != nil || n == 0) {
		// AudioToolbox has no concealment for this, so play silence.
		n = uint32(d.last)
		if n == 0 {
			n = uint32(d.cfg.SampleRate / opusFrames20)
		}
		clear(f.Samples[:int(n)*d.cfg.Channels])
	}
	if n == 0 {
		f.Release()
		return nil
	}
	d.last = int(n)
	f.Samples = f.Samples[:int(n)*d.cfg.Channels]
	f.SampleRate, f.Channels, f.PTS = d.cfg.SampleRate, d.cfg.Channels, p.PTS
	d.out.push(f)
	return nil
}

func (d *auDecoder) Receive() (*AudioFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}
func (d *auDecoder) Flush() error         { d.flushed = true; return nil }
func (d *auDecoder) Backend() BackendInfo { return d.info }

type auEncoder struct {
	auConverter
	info     BackendInfo
	packetsP packetPool
	out      fifo[*EncodedPacket]
	pending  []int16
	pts      time.Duration
	flushed  bool
}

func newAUEncoder(_ *Library, cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	a, err := loadApple()
	if err != nil {
		return nil, err
	}
	e := &auEncoder{info: info, pending: make([]int16, 0, cfg.SampleRate/opusFrames20*cfg.Channels)}
	in := pcmFormat(cfg)
	out := asbd{sampleRate: float64(cfg.SampleRate), format: fourccOpus, framesPerPacket: uint32(cfg.SampleRate / opusFrames20), channels: uint32(cfg.Channels)}
	if err := e.open(a, cfg, &in, &out); err != nil {
		return nil, err
	}
	if cfg.Bitrate > 0 {
		v := uint32(cfg.Bitrate)
		if st := a.audioSetProp(e.conv, auBitrateProp, 4, unsafe.Pointer(&v)); st != 0 {
			e.Close()
			return nil, fmt.Errorf("audiotoolbox: set bitrate: status %d", st)
		}
	}
	return e, nil
}

func (e *auEncoder) emit(pcm []int16) error {
	s := &e.input
	s.avail, s.packets, s.channels = 1, uint32(len(pcm)/e.cfg.Channels), uint32(e.cfg.Channels)
	s.bytes, s.data = uint32(2*len(pcm)), uintptr(unsafe.Pointer(&pcm[0]))
	s.desc = auPacketDesc{}
	p := e.packetsP.get(opusMaxPacket)
	e.packets = 1
	n, err := e.fill(p.Data, uintptr(unsafe.Pointer(&e.desc)))
	runtime.KeepAlive(pcm)
	if err != nil {
		p.Release()
		return err
	}
	if n == 0 {
		p.Release()
		e.pts += 20 * time.Millisecond
		return nil
	}
	p.Data = p.Data[:e.desc.dataByteSize]
	p.PTS, p.Keyframe = e.pts, true
	e.out.push(p)
	e.pts += 20 * time.Millisecond
	return nil
}

func (e *auEncoder) Send(f *AudioFrame) error {
	if f.SampleRate != e.cfg.SampleRate || f.Channels != e.cfg.Channels || len(f.Samples)%e.cfg.Channels != 0 {
		return fmt.Errorf("%w: audio frame does not match encoder format", ErrInvalid)
	}
	size, samples := cap(e.pending), f.Samples
	if len(e.pending) == 0 {
		e.pts = f.PTS
	}
	for len(samples) > 0 {
		n := min(size-len(e.pending), len(samples))
		e.pending = append(e.pending, samples[:n]...)
		samples = samples[n:]
		if len(e.pending) == size {
			if err := e.emit(e.pending); err != nil {
				return err
			}
			e.pending = e.pending[:0]
		}
	}
	return nil
}

func (e *auEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *auEncoder) Flush() error {
	e.flushed = true
	if len(e.pending) == 0 {
		return nil
	}
	n := len(e.pending)
	e.pending = e.pending[:cap(e.pending)]
	clear(e.pending[n:])
	err := e.emit(e.pending)
	e.pending = e.pending[:0]
	return err
}
func (e *auEncoder) Backend() BackendInfo { return e.info }

func appleBackends() []backend {
	avail := func(*Library) error { _, err := loadApple(); return err }
	hwJPEG, hwHEVC, hwVP9 := false, false, false
	if a, err := loadApple(); err == nil {
		hwJPEG = a.hwDecode(fourccJPEG)
		hwHEVC = a.hwDecode(fourccHEVC)
		if a.addVP9 != nil {
			a.addVP9(fourccVP9)
			hwVP9 = a.hwDecode(fourccVP9)
		}
	}
	video := func(c Codec, hwDec, hwEnc bool) []backend {
		dec := BackendInfo{"videotoolbox", c, Decode, hwDec}
		enc := BackendInfo{"videotoolbox", c, Encode, hwEnc}
		return []backend{
			{BackendInfo: dec, avail: avail, vdec: func(l *Library, cfg VideoConfig) (VideoDecoder, error) { return newVTDecoder(l, cfg, dec) }},
			{BackendInfo: enc, avail: avail, venc: func(l *Library, cfg VideoConfig) (VideoEncoder, error) { return newVTEncoder(l, cfg, enc) }},
		}
	}
	out := append(video(H264, true, true), video(JPEG, hwJPEG, false)...)
	h265 := video(H265, hwHEVC, true)
	h265[1].avail = func(*Library) error { return hevcEncoderAvailable() }
	out = append(out, h265...)
	if hwVP9 { // decode only, and only in hardware: libvpx covers the rest
		out = append(out, video(VP9, true, false)[0])
	}
	dec := BackendInfo{"audiotoolbox", Opus, Decode, false}
	enc := BackendInfo{"audiotoolbox", Opus, Encode, false}
	return append(out,
		backend{BackendInfo: dec, avail: avail, adec: func(l *Library, cfg AudioConfig) (AudioDecoder, error) { return newAUDecoder(l, cfg, dec) }},
		backend{BackendInfo: enc, avail: avail, aenc: func(l *Library, cfg AudioConfig) (AudioEncoder, error) { return newAUEncoder(l, cfg, enc) }})
}
