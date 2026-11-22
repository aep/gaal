//go:build windows && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Windows Media Foundation, reached through raw COM vtables. This file holds
// the COM layer and MFT discovery, backend_mf_windows_transform.go drives an
// IMFTransform, and backend_mf_windows_codecs.go implements the streams.
//
// Pointers given to COM must stay put while a call runs. Go moves stacks, so
// every out-parameter lives in an mfCall, which is always on the heap.

const mfBackendName = "mediafoundation"

type mfGUID struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

func mfFourCC(s string) mfGUID {
	return mfGUID{uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24, 0, 0x10,
		[8]byte{0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}}
}

func mfWaveFormat(tag uint32) mfGUID {
	return mfGUID{tag, 0, 0x10, [8]byte{0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}}
}

var (
	iidTransform     = mfGUID{0xbf94c121, 0x5b05, 0x4e6f, [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}
	iidEventGen      = mfGUID{0x2cd0bd52, 0xbcd5, 0x4b89, [8]byte{0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d}}
	iid2DBuffer      = mfGUID{0x7dc9d5f9, 0x9ed9, 0x44ec, [8]byte{0x9b, 0xbf, 0x06, 0x00, 0xbb, 0x58, 0x9f, 0xbb}}
	iidCodecAPI      = mfGUID{0x901db4c7, 0x31ce, 0x41a2, [8]byte{0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}}
	iidMFShutdown    = mfGUID{0x97ec2ea4, 0x0e42, 0x4937, [8]byte{0x97, 0xac, 0x9d, 0x6d, 0x32, 0x88, 0x24, 0xe1}}
	catVideoDec      = mfGUID{0xd6c02d4b, 0x6833, 0x45b4, [8]byte{0x97, 0x1a, 0x05, 0xa4, 0xb0, 0x4b, 0xab, 0x91}}
	catVideoEnc      = mfGUID{0xf79eac7d, 0xe545, 0x4387, [8]byte{0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a}}
	catAudioDec      = mfGUID{0x9ea73fb4, 0xef7a, 0x4559, [8]byte{0x8d, 0x5d, 0x71, 0x9d, 0x8f, 0x04, 0x26, 0xc7}}
	mediaVideo       = mfGUID{0x73646976, 0, 0x10, [8]byte{0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}}
	mediaAudio       = mfGUID{0x73647561, 0, 0x10, [8]byte{0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}}
	fmtH264          = mfFourCC("H264")
	fmtHEVC          = mfFourCC("HEVC")
	fmtVP90          = mfFourCC("VP90")
	fmtMJPG          = mfFourCC("MJPG")
	fmtVP80          = mfFourCC("VP80")
	fmtNV12          = mfFourCC("NV12")
	fmtI420          = mfFourCC("I420")
	fmtIYUV          = mfFourCC("IYUV")
	fmtYV12          = mfFourCC("YV12")
	fmtYUY2          = mfFourCC("YUY2")
	fmtOpus          = mfWaveFormat(0x704f)
	fmtPCM           = mfWaveFormat(1)
	fmtFloat         = mfWaveFormat(3)
	attrMajor        = mfGUID{0x48eba18e, 0xf8c9, 0x4687, [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	attrSubtype      = mfGUID{0xf7e34c9a, 0x42e8, 0x4714, [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	attrFrameSize    = mfGUID{0x1652c33d, 0xd6b2, 0x4012, [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	attrFrameRate    = mfGUID{0xc459a2e8, 0x3d2c, 0x4e44, [8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0}}
	attrPAR          = mfGUID{0xc6376a1e, 0x8d0a, 0x4027, [8]byte{0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6}}
	attrNominalRange = mfGUID{0xc21b8ee5, 0xb956, 0x4071, [8]byte{0x8d, 0xaf, 0x32, 0x5e, 0xdf, 0x5c, 0xab, 0x11}}
	attrInterlace    = mfGUID{0xe2724bb8, 0xe676, 0x4806, [8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd}}
	attrAvgBitrate   = mfGUID{0x20332624, 0xfb0d, 0x4d9e, [8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e}}
	attrStride       = mfGUID{0x644b4e48, 0x1e02, 0x4516, [8]byte{0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6}}
	attrProfile      = mfGUID{0xad76a80b, 0x2d5c, 0x4e0b, [8]byte{0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36}}
	attrSeqHeader    = mfGUID{0x3c036de7, 0x3ad0, 0x4c9e, [8]byte{0x92, 0x16, 0xee, 0x6d, 0x6a, 0xc2, 0x1c, 0xb3}}
	attrAperture     = mfGUID{0xd7388766, 0x18fe, 0x48c6, [8]byte{0xa1, 0x77, 0xee, 0x89, 0x48, 0x67, 0xc8, 0xc4}}
	attrKeySpacing   = mfGUID{0xc16eb52b, 0x73a1, 0x476f, [8]byte{0x8d, 0x62, 0x83, 0x9d, 0x6a, 0x02, 0x06, 0x52}}
	attrUserData     = mfGUID{0xb6bc765f, 0x4c3b, 0x40a4, [8]byte{0xbd, 0x51, 0x25, 0x35, 0xb6, 0x6f, 0xe0, 0x9d}}
	attrChannels     = mfGUID{0x37e48bf5, 0x645e, 0x4c5b, [8]byte{0x89, 0xde, 0xad, 0xa9, 0xe2, 0x9b, 0x69, 0x6a}}
	attrRate         = mfGUID{0x5faeeae7, 0x0290, 0x4c31, [8]byte{0x9e, 0x8a, 0xc5, 0x34, 0xf6, 0x8d, 0x9d, 0xba}}
	attrBits         = mfGUID{0xf2deb57f, 0x40fa, 0x4764, [8]byte{0xaa, 0x33, 0xed, 0x4f, 0x2d, 0x1f, 0xf6, 0x69}}
	attrBlockAlign   = mfGUID{0x322de230, 0x9eeb, 0x43bd, [8]byte{0xab, 0x7a, 0xff, 0x41, 0x22, 0x51, 0x54, 0x1d}}
	attrAvgBytes     = mfGUID{0x1aab75c8, 0xcfef, 0x451c, [8]byte{0xab, 0x95, 0xac, 0x03, 0x4b, 0x8e, 0x17, 0x31}}
	attrCleanPoint   = mfGUID{0x9cdf01d8, 0xa0f0, 0x43ba, [8]byte{0xb0, 0x77, 0xea, 0xa0, 0x6c, 0xbd, 0x72, 0x8a}}
	attrAsync        = mfGUID{0xf81a699a, 0x649a, 0x497d, [8]byte{0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a}}
	attrAsyncOK      = mfGUID{0xe5666d6b, 0x3422, 0x4eb6, [8]byte{0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07}}
	attrMFTFlags     = mfGUID{0x9359bb7e, 0x6275, 0x46c4, [8]byte{0xa0, 0x25, 0x1c, 0x01, 0xe4, 0x5f, 0x1a, 0x86}}
	attrHWURL        = mfGUID{0x2fb866ac, 0xb078, 0x4942, [8]byte{0xab, 0x6c, 0x00, 0x3d, 0x05, 0xcd, 0xa6, 0x74}}
	// MF_LOW_LATENCY and CODECAPI_AVLowLatencyMode are the same GUID.
	// MFT_FRIENDLY_NAME_Attribute
	attrFriendlyName = mfGUID{0x314ffbae, 0x5b41, 0x4c95, [8]byte{0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3}}
	attrLowLatency   = mfGUID{0x9c27891a, 0xed7a, 0x40e1, [8]byte{0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}}

	apiRateControl = mfGUID{0x1c0608e9, 0x370c, 0x4710, [8]byte{0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23}}
	apiMeanBitrate = mfGUID{0xf7222374, 0x2144, 0x4815, [8]byte{0xb5, 0x50, 0xa3, 0x7f, 0x8e, 0x12, 0xee, 0x52}}
	apiMaxBitrate  = mfGUID{0x9651eae4, 0x39b9, 0x4ebf, [8]byte{0x85, 0xef, 0xd7, 0xf4, 0x44, 0xec, 0x74, 0x65}}
	apiForceKey    = mfGUID{0x398c1b98, 0x8353, 0x475a, [8]byte{0x9e, 0xf2, 0x8f, 0x26, 0x5d, 0x26, 0x03, 0x45}}
	apiGOPSize     = mfGUID{0x95f31b26, 0x95a4, 0x41aa, [8]byte{0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1}}
	apiBFrames     = mfGUID{0x8d390aac, 0xdc5c, 0x4200, [8]byte{0xb5, 0x7f, 0x81, 0x4d, 0x04, 0xba, 0xba, 0xb2}}
)

// HRESULTs.
const (
	hrNotImpl        = 0x80004001
	hrInvalidData    = 0x8007000d
	hrBufferTooSmall = 0xc00d36b1
	hrInvalidType    = 0xc00d36b4
	hrNotAccepting   = 0xc00d36b5
	hrNoMoreTypes    = 0xc00d36b9
	hrBadFileFormat  = 0xc00d36be
	hrBadStreamData  = 0xc00d36cb
	hrNoEvents       = 0xc00d3e80
	hrTypeNotSet     = 0xc00d6d60
	hrStreamChange   = 0xc00d6d61
	hrNeedMoreInput  = 0xc00d6d72
)

func mfFailed(hr uint32) bool { return int32(hr) < 0 }

// Vtable slots. IMFMediaType, IMFSample, IMFActivate and IMFMediaEvent extend
// IMFAttributes, so their own methods start at slot 33.
const (
	slotQI      = 0
	slotAddRef  = 1
	slotRelease = 2

	slotGetUINT32   = 7
	slotGetUINT64   = 8
	slotGetGUID     = 10
	slotGetString   = 12
	slotGetBlobSize = 14
	slotGetBlob     = 15
	slotSetUINT32   = 21
	slotSetUINT64   = 22
	slotSetGUID     = 24
	slotSetBlob     = 26

	slotActivateObject = 33
	slotShutdownObject = 34

	slotEventType   = 33
	slotEventStatus = 35

	slotSampleSetTime     = 36
	slotSampleGetTime     = 35
	slotSampleSetDuration = 38
	slotSampleBufferCount = 39
	slotSampleBufferAt    = 40
	slotSampleContiguous  = 41
	slotSampleAddBuffer   = 42
	slotSampleRemoveAll   = 44
	slotSampleTotalLength = 45

	slotBufLock   = 3
	slotBufUnlock = 4
	slotBufGetLen = 5
	slotBufSetLen = 6
	slotBufGetMax = 7

	slot2DLock   = 3
	slot2DUnlock = 4

	slotGetEvent = 3

	slotCodecIsModifiable = 4
	slotCodecSetValue     = 9

	slotShutdown = 3

	slotInStreamInfo   = 6
	slotOutStreamInfo  = 7
	slotGetAttributes  = 8
	slotInAvailable    = 13
	slotOutAvailable   = 14
	slotSetInputType   = 15
	slotSetOutputType  = 16
	slotInCurrent      = 17
	slotOutCurrent     = 18
	slotProcessMessage = 23
	slotProcessInput   = 24
	slotProcessOutput  = 25
)

const (
	mftEnumHardware = 0x4
	mftEnumFlags    = 0x1 | 0x2 | mftEnumHardware | 0x40 // sync, async, hardware, sort and filter

	mftMsgFlush      = 0x0
	mftMsgDrain      = 0x1
	mftMsgBegin      = 0x10000000
	mftMsgEnd        = 0x10000001
	mftMsgEndOfStrm  = 0x10000002
	mftMsgStartStrm  = 0x10000003
	mftOutProvides   = 0x100
	mftOutCanProvide = 0x200
	mftSetTestOnly   = 1

	meError          = 1
	meNeedInput      = 601
	meHaveOutput     = 602
	meDrainComplete  = 603
	mfEventNoWait    = 1
	vtBool           = 11
	vtUI4            = 19
	mfProgressive    = 2
	mfRateControlCBR = 0
	mfProfileBase    = 66
	mfProfileHEVC    = 1 // eAVEncH265VProfile_Main_420_8
)

type mftInputInfo struct {
	maxLatency             int64
	flags, size, lookahead uint32
	alignment              uint32
}

type mftOutputInfo struct{ flags, size, alignment uint32 }

type mftOutputBuffer struct {
	id     uint32
	sample uintptr
	status uint32
	events uintptr
}

type mfTypeInfo struct{ major, sub mfGUID }

// mfCall makes COM calls without allocating, and holds their out-parameters
// where they cannot move. It is not safe for concurrent use. Always get one
// from newMFCall or embed it in a heap object.
type mfCall struct {
	a [10]uintptr

	p        uintptr
	u, u2    uint32
	i        int32
	u64      uint64
	i64      int64
	g        mfGUID
	variant  [3]uint64
	inInfo   mftInputInfo
	outInfo  mftOutputInfo
	output   mftOutputBuffer
	types    [2]mfTypeInfo
	aperture [4]uint32
}

//go:noinline
func newMFCall() *mfCall { return new(mfCall) }

func mfPtr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

func mfMethod(obj uintptr, slot int) uintptr {
	vt := *(*uintptr)(mfPtr(obj))
	return *(*uintptr)(mfPtr(vt + uintptr(slot)*unsafe.Sizeof(vt)))
}

func gp(g *mfGUID) uintptr { return uintptr(unsafe.Pointer(g)) }

func (c *mfCall) raw(fn uintptr, n int) uintptr {
	r, _, _ := syscall.SyscallN(fn, c.a[:n]...)
	return r
}

func (c *mfCall) m0(obj uintptr, slot int) uint32 {
	c.a[0] = obj
	return uint32(c.raw(mfMethod(obj, slot), 1))
}

func (c *mfCall) m1(obj uintptr, slot int, a uintptr) uint32 {
	c.a[0], c.a[1] = obj, a
	return uint32(c.raw(mfMethod(obj, slot), 2))
}

func (c *mfCall) m2(obj uintptr, slot int, a, b uintptr) uint32 {
	c.a[0], c.a[1], c.a[2] = obj, a, b
	return uint32(c.raw(mfMethod(obj, slot), 3))
}

func (c *mfCall) m3(obj uintptr, slot int, a, b, d uintptr) uint32 {
	c.a[0], c.a[1], c.a[2], c.a[3] = obj, a, b, d
	return uint32(c.raw(mfMethod(obj, slot), 4))
}

func (c *mfCall) m4(obj uintptr, slot int, a, b, d, e uintptr) uint32 {
	c.a[0], c.a[1], c.a[2], c.a[3], c.a[4] = obj, a, b, d, e
	return uint32(c.raw(mfMethod(obj, slot), 5))
}

func (c *mfCall) addRef(obj uintptr) uint32 { return c.m0(obj, slotAddRef) }

func (c *mfCall) release(obj uintptr) uint32 {
	if obj == 0 {
		return 0
	}
	return c.m0(obj, slotRelease)
}

// refs returns the reference count of obj.
func (c *mfCall) refs(obj uintptr) uint32 {
	c.addRef(obj)
	return c.release(obj)
}

func (c *mfCall) qi(obj uintptr, iid *mfGUID) (uintptr, uint32) {
	c.p = 0
	hr := c.m2(obj, slotQI, gp(iid), uintptr(unsafe.Pointer(&c.p)))
	if mfFailed(hr) {
		return 0, hr
	}
	return c.p, hr
}

func (c *mfCall) getU32(obj uintptr, key *mfGUID) (uint32, bool) {
	c.u = 0
	hr := c.m2(obj, slotGetUINT32, gp(key), uintptr(unsafe.Pointer(&c.u)))
	return c.u, !mfFailed(hr)
}

func (c *mfCall) getU64(obj uintptr, key *mfGUID) (uint64, bool) {
	c.u64 = 0
	hr := c.m2(obj, slotGetUINT64, gp(key), uintptr(unsafe.Pointer(&c.u64)))
	return c.u64, !mfFailed(hr)
}

func (c *mfCall) getGUID(obj uintptr, key *mfGUID) (mfGUID, bool) {
	c.g = mfGUID{}
	hr := c.m2(obj, slotGetGUID, gp(key), uintptr(unsafe.Pointer(&c.g)))
	return c.g, !mfFailed(hr)
}

// getBlob returns a copy of a blob attribute, or nil if it is absent.
func (c *mfCall) getBlob(obj uintptr, key *mfGUID) []byte {
	c.u = 0
	if mfFailed(c.m2(obj, slotGetBlobSize, gp(key), uintptr(unsafe.Pointer(&c.u)))) || c.u == 0 {
		return nil
	}
	b := make([]byte, int(c.u))
	hr := c.m4(obj, slotGetBlob, gp(key), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&c.u)))
	runtime.KeepAlive(b)
	if mfFailed(hr) {
		return nil
	}
	return b[:c.u]
}

func (c *mfCall) setU32(obj uintptr, key *mfGUID, v uint32) uint32 {
	return c.m2(obj, slotSetUINT32, gp(key), uintptr(v))
}

func (c *mfCall) setU64(obj uintptr, key *mfGUID, v uint64) uint32 {
	return c.m2(obj, slotSetUINT64, gp(key), uintptr(v))
}

func (c *mfCall) setGUID(obj uintptr, key, v *mfGUID) uint32 {
	return c.m2(obj, slotSetGUID, gp(key), gp(v))
}

// setCodecValue calls ICodecAPI::SetValue with a VARIANT of the given type.
func (c *mfCall) setCodecValue(api uintptr, key *mfGUID, vt uint16, v uint64) uint32 {
	c.variant = [3]uint64{uint64(vt), v, 0}
	return c.m2(api, slotCodecSetValue, gp(key), uintptr(unsafe.Pointer(&c.variant)))
}

func mfPair(hi, lo int) uint64 { return uint64(uint32(hi))<<32 | uint64(uint32(lo)) }

type mfAPI struct {
	enumEx, createSample, createAligned, createType, taskFree uintptr
}

func mfSystemDLL(load *syscall.LazyProc, name string) (uintptr, error) {
	const searchSystem32 = 0x800
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, _, cerr := load.Call(uintptr(unsafe.Pointer(p)), 0, searchSystem32)
	if h == 0 {
		return 0, fmt.Errorf("load %s: %w", name, cerr)
	}
	return h, nil
}

var loadMF = sync.OnceValues(func() (*mfAPI, error) {
	load := syscall.NewLazyDLL("kernel32.dll").NewProc("LoadLibraryExW")
	if err := load.Find(); err != nil {
		return nil, err
	}
	mfplat, err := mfSystemDLL(load, "mfplat.dll")
	if err != nil {
		return nil, err
	}
	ole, err := mfSystemDLL(load, "ole32.dll")
	if err != nil {
		return nil, err
	}
	a := &mfAPI{}
	var startup, mtaUsage uintptr
	for _, s := range []struct {
		h    uintptr
		name string
		dst  *uintptr
	}{
		{mfplat, "MFStartup", &startup}, {mfplat, "MFTEnumEx", &a.enumEx},
		{mfplat, "MFCreateSample", &a.createSample}, {mfplat, "MFCreateAlignedMemoryBuffer", &a.createAligned},
		{mfplat, "MFCreateMediaType", &a.createType},
		{ole, "CoTaskMemFree", &a.taskFree}, {ole, "CoIncrementMTAUsage", &mtaUsage},
	} {
		if *s.dst, err = syscall.GetProcAddress(syscall.Handle(s.h), s.name); err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
	}
	// Keeping the multithreaded apartment alive lets every OS thread, and so
	// every goroutine, use the free-threaded MF objects without CoInitialize.
	c := newMFCall()
	c.a[0] = uintptr(unsafe.Pointer(&c.p))
	if hr := uint32(c.raw(mtaUsage, 1)); mfFailed(hr) {
		return nil, mfError("CoIncrementMTAUsage", hr)
	}
	const mfVersion, startupLite = 0x00020070, 1
	c.a[0], c.a[1] = mfVersion, startupLite
	if hr := uint32(c.raw(startup, 2)); mfFailed(hr) {
		return nil, mfError("MFStartup", hr)
	}
	return a, nil
})

func mfError(what string, hr uint32) error {
	return fmt.Errorf("mediafoundation: %s: hresult 0x%08x", what, hr)
}

// mfIsCorrupt reports HRESULTs that blame the input data.
func mfIsCorrupt(hr uint32) bool {
	return hr == hrBadStreamData || hr == hrBadFileFormat || hr == hrInvalidData
}

// mfDataError classifies a failure to process media data.
func mfDataError(what string, hr uint32) error {
	if mfIsCorrupt(hr) {
		return fmt.Errorf("%w: mediafoundation: %s: hresult 0x%08x", ErrCorrupt, what, hr)
	}
	return mfError(what, hr)
}

func (a *mfAPI) newMediaType(c *mfCall, major, sub *mfGUID) (uintptr, error) {
	c.p = 0
	c.a[0] = uintptr(unsafe.Pointer(&c.p))
	if hr := uint32(c.raw(a.createType, 1)); mfFailed(hr) {
		return 0, mfError("MFCreateMediaType", hr)
	}
	t := c.p
	c.setGUID(t, &attrMajor, major)
	c.setGUID(t, &attrSubtype, sub)
	return t, nil
}

// mfSpec selects the MFTs for one codec and direction.
type mfSpec struct {
	category *mfGUID
	in, out  mfTypeInfo
	anyOut   bool // match any output type
}

func mfSpecFor(codec Codec, dir Direction) (mfSpec, bool) {
	dec := func(cat, major, sub *mfGUID) (mfSpec, bool) {
		return mfSpec{category: cat, in: mfTypeInfo{*major, *sub}, anyOut: true}, true
	}
	switch {
	case dir == Encode && codec == H264:
		return mfSpec{category: &catVideoEnc, in: mfTypeInfo{mediaVideo, fmtNV12}, out: mfTypeInfo{mediaVideo, fmtH264}}, true
	case dir == Encode && codec == H265:
		return mfSpec{category: &catVideoEnc, in: mfTypeInfo{mediaVideo, fmtNV12}, out: mfTypeInfo{mediaVideo, fmtHEVC}}, true
	case dir == Encode:
		return mfSpec{}, false
	case codec == H264:
		return dec(&catVideoDec, &mediaVideo, &fmtH264)
	case codec == H265:
		return dec(&catVideoDec, &mediaVideo, &fmtHEVC)
	case codec == VP8:
		return dec(&catVideoDec, &mediaVideo, &fmtVP80)
	case codec == VP9:
		return dec(&catVideoDec, &mediaVideo, &fmtVP90)
	case codec == JPEG:
		return dec(&catVideoDec, &mediaVideo, &fmtMJPG)
	case codec == Opus:
		return dec(&catAudioDec, &mediaAudio, &fmtOpus)
	}
	return mfSpec{}, false
}

// enumerate returns the IMFActivate objects for spec, best first. The caller
// releases each of them.
func (a *mfAPI) enumerate(c *mfCall, s mfSpec) ([]uintptr, error) {
	c.types[0], c.types[1] = s.in, s.out
	in := uintptr(unsafe.Pointer(&c.types[0]))
	out := uintptr(unsafe.Pointer(&c.types[1]))
	if s.anyOut {
		out = 0
	}
	c.p, c.u = 0, 0
	list, count := uintptr(unsafe.Pointer(&c.p)), uintptr(unsafe.Pointer(&c.u))
	n := 0
	// A GUID passed by value goes by reference on x64 and in two registers on
	// arm64.
	if runtime.GOARCH == "arm64" {
		w := (*[2]uintptr)(unsafe.Pointer(s.category))
		c.a = [10]uintptr{w[0], w[1], mftEnumFlags, in, out, list, count}
		n = 7
	} else {
		c.a = [10]uintptr{gp(s.category), mftEnumFlags, in, out, list, count}
		n = 6
	}
	if hr := uint32(c.raw(a.enumEx, n)); mfFailed(hr) {
		return nil, mfError("MFTEnumEx", hr)
	}
	if c.p == 0 {
		return nil, nil
	}
	arr, total := c.p, int(c.u)
	out2 := make([]uintptr, total)
	copy(out2, unsafe.Slice((*uintptr)(mfPtr(arr)), total))
	c.a[0] = arr
	c.raw(a.taskFree, 1)
	return out2, nil
}

// mfIsHardware reports whether an activation object is a hardware MFT.
func mfIsHardware(c *mfCall, activate uintptr) bool {
	if f, ok := c.getU32(activate, &attrMFTFlags); ok && f&mftEnumHardware != 0 {
		return true
	}
	c.u = 0
	return !mfFailed(c.m2(activate, 11, gp(&attrHWURL), uintptr(unsafe.Pointer(&c.u)))) // GetStringLength
}

var errMFNoTransform = errors.New("no matching transform is installed")

func mfBackends() []backend {
	type key struct {
		codec Codec
		dir   Direction
		hw    bool
	}
	var mu sync.Mutex
	known := map[key]error{}
	avail := func(k key) func(*Library) error {
		return func(*Library) error {
			mu.Lock()
			defer mu.Unlock()
			if err, ok := known[k]; ok {
				return err
			}
			err := mfProbe(k.codec, k.dir, k.hw)
			known[k] = err
			return err
		}
	}
	var out []backend
	add := func(codec Codec, dir Direction, hw bool) {
		b := backend{BackendInfo: BackendInfo{mfBackendName, codec, dir, hw}, avail: avail(key{codec, dir, hw})}
		info := b.BackendInfo
		switch {
		case dir == Encode:
			b.venc = func(_ *Library, cfg VideoConfig) (VideoEncoder, error) { return newMFEncoder(cfg, info) }
		case codec == Opus:
			b.adec = func(_ *Library, cfg AudioConfig) (AudioDecoder, error) { return newMFAudioDecoder(cfg, info) }
		default:
			b.vdec = func(_ *Library, cfg VideoConfig) (VideoDecoder, error) { return newMFVideoDecoder(cfg, info) }
		}
		out = append(out, b)
	}
	// Hardware and software are separate entries, so Options.Hardware applies
	// and a failing hardware MFT fails over to a software one.
	for _, hw := range []bool{true, false} {
		add(H264, Decode, hw)
		add(H265, Decode, hw)
		add(VP8, Decode, hw)
		add(VP9, Decode, hw)
		add(JPEG, Decode, hw)
		add(H264, Encode, hw)
		add(H265, Encode, hw)
		add(Opus, Decode, hw)
	}
	return out
}

func mfProbe(codec Codec, dir Direction, hw bool) error {
	api, err := loadMF()
	if err != nil {
		return err
	}
	spec, ok := mfSpecFor(codec, dir)
	if !ok {
		return errMFNoTransform
	}
	c := newMFCall()
	list, err := api.enumerate(c, spec)
	if err != nil {
		return err
	}
	found := false
	for _, a := range list {
		found = found || mfIsHardware(c, a) == hw
		c.release(a)
	}
	if !found {
		return errMFNoTransform
	}
	return nil
}
