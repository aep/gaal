//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/aep/gaal/internal/dl"
)

// Direct NVENC and NVDEC. All layouts come from nv_layout_linux.go.

func cBytes(addr uintptr, n int) []byte { return cMem(addr, n) }

// cudaAPI holds one process-wide context, so opening a codec is cheap.
type cudaAPI struct {
	ctx                 uintptr
	push, pop, memcpy2D uintptr
}

var loadCUDA = sync.OnceValues(func() (*cudaAPI, error) {
	h, err := dl.Open("libcuda.so.1")
	if err != nil {
		return nil, err
	}
	var cuInit func(uint32) int32
	var deviceGet func(dev *int32, ordinal int32) int32
	var ctxCreate func(ctx *uintptr, flags uint32, dev int32) int32
	a := &cudaAPI{}
	if err := dl.BindAll(h, dl.Table{
		"cuInit": &cuInit, "cuDeviceGet": &deviceGet, "cuCtxCreate_v2": &ctxCreate,
		"cuCtxPushCurrent_v2": &a.push, "cuCtxPopCurrent_v2": &a.pop, "cuMemcpy2D_v2": &a.memcpy2D,
	}); err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if rc := cuInit(0); rc != 0 {
		return nil, fmt.Errorf("cuInit: error %d", rc)
	}
	var dev int32
	if rc := deviceGet(&dev, 0); rc != 0 {
		return nil, fmt.Errorf("cuDeviceGet: error %d", rc)
	}
	if rc := ctxCreate(&a.ctx, 0, dev); rc != 0 {
		return nil, fmt.Errorf("cuCtxCreate: error %d", rc)
	}
	var old uintptr
	purego.SyscallN(a.pop, uintptr(unsafe.Pointer(&old)))
	return a, nil
})

// enter makes the context current on a locked thread.
func (a *cudaAPI) enter(c *dl.Caller) error {
	runtime.LockOSThread()
	if r := c.C1(a.push, a.ctx); int32(r) != 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("cuCtxPushCurrent: error %d", int32(r))
	}
	return nil
}

func (a *cudaAPI) leave(c *dl.Caller, scratch *uintptr) {
	c.C1(a.pop, uintptr(unsafe.Pointer(scratch)))
	runtime.UnlockOSThread()
}

// --- NVENC ---

type nvencFuncs struct {
	open, presetConfig, initialize, createInput, destroyInput, lockInput, unlockInput,
	createBS, destroyBS, encode, lockBS, unlockBS, destroy, lastError, reconfigure uintptr
}

var loadNVENC = sync.OnceValues(func() (*nvencFuncs, error) {
	h, err := dl.Open("libnvidia-encode.so.1")
	if err != nil {
		return nil, err
	}
	var create func(list unsafe.Pointer) int32
	if err := dl.BindAll(h, dl.Table{"NvEncodeAPICreateInstance": &create}); err != nil {
		return nil, err
	}
	list := make([]byte, nvencFuncListSize)
	le32(list, 0, nvencFuncListVer)
	if rc := create(unsafe.Pointer(&list[0])); rc != 0 {
		return nil, fmt.Errorf("NvEncodeAPICreateInstance: status %d", rc)
	}
	return &nvencFuncs{
		open: rdPtr(list, nvf_nvEncOpenEncodeSessionEx), presetConfig: rdPtr(list, nvf_nvEncGetEncodePresetConfigEx),
		initialize: rdPtr(list, nvf_nvEncInitializeEncoder), createInput: rdPtr(list, nvf_nvEncCreateInputBuffer),
		destroyInput: rdPtr(list, nvf_nvEncDestroyInputBuffer), lockInput: rdPtr(list, nvf_nvEncLockInputBuffer),
		unlockInput: rdPtr(list, nvf_nvEncUnlockInputBuffer), createBS: rdPtr(list, nvf_nvEncCreateBitstreamBuffer),
		destroyBS: rdPtr(list, nvf_nvEncDestroyBitstreamBuffer), encode: rdPtr(list, nvf_nvEncEncodePicture),
		lockBS: rdPtr(list, nvf_nvEncLockBitstream), unlockBS: rdPtr(list, nvf_nvEncUnlockBitstream),
		destroy: rdPtr(list, nvf_nvEncDestroyEncoder), lastError: rdPtr(list, nvf_nvEncGetLastErrorString),
		reconfigure: rdPtr(list, nvf_nvEncReconfigureEncoder),
	}, nil
})

// nvEncoder encodes synchronously: each Send yields its packet immediately,
// because B-frames and lookahead are disabled.
type nvEncoder struct {
	c       dl.Caller
	fn      *nvencFuncs
	cfg     VideoConfig
	info    BackendInfo
	enc     uintptr
	input   uintptr
	bs      uintptr
	config  []byte
	init    []byte
	pic     []byte
	lockIn  []byte
	lockOut []byte
	reinit  []byte // NV_ENC_RECONFIGURE_PARAMS, built on first use
	packets packetPool
	out     fifo[*EncodedPacket]
	flushed bool
}

// NV_ENC_RECONFIGURE_PARAMS from the same nvEncodeAPI.h as nv_layout_linux.go.
const (
	nvReconfigVer   = 4043440141
	nvReconfigSize  = 1816
	nvoReconfigInit = 8
)

// SetBitrate implements BitrateSetter. It reconfigures the rate control
// without resetting the encoder or forcing an IDR picture.
func (e *nvEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.enc == 0 {
		return ErrClosed
	}
	c := e.config
	oldAvg, oldMax := rd32(c, nvoCfgAvg), rd32(c, nvoCfgMax)
	oldVBV, oldInit := rd32(c, nvoCfgVBV), rd32(c, nvoCfgVBVInit)
	vbv := uint32(bps / e.cfg.FPS)
	le32(c, nvoCfgAvg, uint32(bps))
	le32(c, nvoCfgMax, uint32(bps))
	le32(c, nvoCfgVBV, vbv)
	le32(c, nvoCfgVBVInit, vbv)
	if e.reinit == nil {
		e.reinit = make([]byte, nvReconfigSize)
	}
	r := e.reinit
	clear(r)
	le32(r, 0, nvReconfigVer)
	copy(r[nvoReconfigInit:], e.init) // points at e.config; resetEncoder and forceIDR stay 0
	if err := e.call("reconfigure", e.fn.reconfigure, r); err != nil {
		le32(c, nvoCfgAvg, oldAvg)
		le32(c, nvoCfgMax, oldMax)
		le32(c, nvoCfgVBV, oldVBV)
		le32(c, nvoCfgVBVInit, oldInit)
		return err
	}
	e.cfg.Bitrate = bps
	return nil
}

func (e *nvEncoder) fail(what string, status uintptr) error {
	msg := ""
	if e.enc != 0 {
		r, _, _ := purego.SyscallN(e.fn.lastError, e.enc)
		msg = dl.GoString(r)
	}
	return fmt.Errorf("nvenc: %s: status %d: %s", what, int32(status), msg)
}

func (e *nvEncoder) call(what string, fn uintptr, arg []byte) error {
	if r := e.c.C2(fn, e.enc, uintptr(unsafe.Pointer(&arg[0]))); int32(r) != 0 {
		return e.fail(what, r)
	}
	return nil
}

func guidWords(g *[16]byte) (uintptr, uintptr) {
	return *(*uintptr)(unsafe.Pointer(&g[0])), *(*uintptr)(unsafe.Pointer(&g[8]))
}

func newNVEncoder(_ *Library, cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	cu, err := loadCUDA()
	if err != nil {
		return nil, err
	}
	fn, err := loadNVENC()
	if err != nil {
		return nil, err
	}
	e := &nvEncoder{fn: fn, cfg: cfg, info: info,
		pic: make([]byte, nvPicSize), lockIn: make([]byte, nvLockInputSize), lockOut: make([]byte, nvLockBSSize)}
	if err := e.setup(cu); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

func (e *nvEncoder) setup(cu *cudaAPI) error {
	fn, cfg := e.fn, e.cfg
	open := make([]byte, nvOpenSize)
	le32(open, 0, nvOpenVer)
	le32(open, nvoOpenType, nvDevCUDA)
	le64(open, nvoOpenDevice, uint64(cu.ctx))
	le32(open, nvoOpenAPI, nvencAPIVersion)
	if r, _, _ := purego.SyscallN(fn.open, uintptr(unsafe.Pointer(&open[0])), uintptr(unsafe.Pointer(&e.enc))); int32(r) != 0 {
		e.enc = 0
		return e.fail("open session", r)
	}
	preset := make([]byte, nvPresetSize)
	le32(preset, 0, nvPresetVer)
	le32(preset, nvoPresetCfg, nvConfigVer)
	codecGUID := &nvGUIDH264
	idrOff, repeatByte, repeatBit := nvoCfgIDR, nvCfgRepeatSPSByte, nvCfgRepeatSPSBit
	if cfg.Codec == H265 {
		codecGUID = &nvGUIDHEVC
		idrOff, repeatByte, repeatBit = nvoCfgHEVCIDR, nvCfgHEVCRepeatByte, nvCfgHEVCRepeatBit
	}
	c0, c1 := guidWords(codecGUID)
	p0, p1 := guidWords(&nvGUIDP1)
	if r, _, _ := purego.SyscallN(fn.presetConfig, e.enc, c0, c1, p0, p1, nvTuneLL, uintptr(unsafe.Pointer(&preset[0]))); int32(r) != 0 {
		return e.fail("preset config", r)
	}
	e.config = make([]byte, nvConfigSize)
	copy(e.config, preset[nvoPresetCfg:])
	c := e.config
	le32(c, 0, nvConfigVer)
	le32(c, nvoCfgGop, uint32(cfg.KeyframeEvery))
	le32(c, nvoCfgP, 1) // no B-frames
	le32(c, idrOff, uint32(cfg.KeyframeEvery))
	le32(c, nvoCfgRCMode, nvRCCBR)
	le32(c, nvoCfgAvg, uint32(cfg.Bitrate))
	le32(c, nvoCfgMax, uint32(cfg.Bitrate))
	// A one-frame rate-control buffer keeps every frame near its budget.
	vbv := uint32(cfg.Bitrate / cfg.FPS)
	le32(c, nvoCfgVBV, vbv)
	le32(c, nvoCfgVBVInit, vbv)
	c[repeatByte] |= 1 << repeatBit
	if cfg.Codec == H264 {
		// Without bitstream_restriction a decoder may assume pictures are
		// reordered as far as the level allows, and browsers then hold each
		// one back a frame. With no B-frames NVENC writes max_num_reorder_frames
		// 0. H.265 states it in sps_max_num_reorder_pics already.
		le32(c, nvoCfgVUIRestrict, 1)
	}
	if cfg.Range != RangeUnspecified {
		signal, format, fullFlag := nvoCfgVUISignal, nvoCfgVUIFormat, nvoCfgVUIFull
		if cfg.Codec == H265 {
			signal, format, fullFlag = nvoCfgHVUISignal, nvoCfgHVUIFormat, nvoCfgHVUIFull
		}
		const videoFormatUnspecified = 5
		le32(c, signal, 1)
		le32(c, format, videoFormatUnspecified)
		if cfg.Range == RangeFull {
			le32(c, fullFlag, 1)
		}
	}

	e.init = make([]byte, nvInitSize)
	in := e.init
	le32(in, 0, nvInitVer)
	copy(in[nvoInitCodec:], codecGUID[:])
	copy(in[nvoInitPresetGUID:], nvGUIDP1[:])
	for _, f := range []struct {
		off int
		v   int
	}{{nvoInitW, cfg.Width}, {nvoInitH, cfg.Height}, {nvoInitDarW, cfg.Width}, {nvoInitDarH, cfg.Height},
		{nvoInitMaxW, cfg.Width}, {nvoInitMaxH, cfg.Height}, {nvoInitFpsN, cfg.FPS}, {nvoInitFpsD, 1},
		{nvoInitPTD, 1}, {nvoInitTuning, nvTuneLL}} {
		le32(in, f.off, uint32(f.v))
	}
	le64(in, nvoInitConfig, uint64(uintptr(unsafe.Pointer(&c[0]))))
	if err := e.call("initialize", fn.initialize, in); err != nil {
		return err
	}
	ci := make([]byte, nvCreateInputSize)
	le32(ci, 0, nvCreateInputVer)
	le32(ci, nvoCIW, uint32(cfg.Width))
	le32(ci, nvoCIH, uint32(cfg.Height))
	le32(ci, nvoCIFmt, nvFmtNV12)
	if err := e.call("create input", fn.createInput, ci); err != nil {
		return err
	}
	e.input = rdPtr(ci, nvoCIBuf)
	cb := make([]byte, nvCreateBSSize)
	le32(cb, 0, nvCreateBSVer)
	if err := e.call("create bitstream", fn.createBS, cb); err != nil {
		return err
	}
	e.bs = rdPtr(cb, nvoCBBuf)
	return nil
}

// upload copies a frame into the locked NV12 input surface.
func (e *nvEncoder) upload(f *VideoFrame) (pitch int, err error) {
	clear(e.lockIn)
	le32(e.lockIn, 0, nvLockInputVer)
	le64(e.lockIn, nvoLIBuf, uint64(e.input))
	if err := e.call("lock input", e.fn.lockInput, e.lockIn); err != nil {
		return 0, err
	}
	pitch = int(rd32(e.lockIn, nvoLIPitch))
	h := f.Height
	dst := cBytes(rdPtr(e.lockIn, nvoLIPtr), pitch*h+pitch*h/2)
	writeNV12(dst, dst[pitch*h:], pitch, f)
	if r := e.c.C2(e.fn.unlockInput, e.enc, e.input); int32(r) != 0 {
		return 0, e.fail("unlock input", r)
	}
	return pitch, nil
}

func (e *nvEncoder) Send(f *VideoFrame) error {
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	pitch, err := e.upload(f)
	if err != nil {
		return err
	}
	p := e.pic
	clear(p)
	le32(p, 0, nvPicVer)
	le32(p, nvoPicW, uint32(f.Width))
	le32(p, nvoPicH, uint32(f.Height))
	le32(p, nvoPicPitch, uint32(pitch))
	le64(p, nvoPicIn, uint64(e.input))
	le64(p, nvoPicOut, uint64(e.bs))
	le32(p, nvoPicFmt, nvFmtNV12)
	le32(p, nvoPicStruct, nvFrame)
	le64(p, nvoPicTS, uint64(f.PTS))
	if f.ForceKeyframe {
		le32(p, nvoPicFlags, nvFlagForceIDR|nvFlagSPS)
	}
	r := e.c.C2(e.fn.encode, e.enc, uintptr(unsafe.Pointer(&p[0])))
	if int32(r) == nvErrNeedMore {
		return nil
	}
	if int32(r) != 0 {
		return e.fail("encode", r)
	}
	return e.collect()
}

func (e *nvEncoder) collect() error {
	l := e.lockOut
	clear(l)
	le32(l, 0, nvLockBSVer)
	le64(l, nvoLBOut, uint64(e.bs))
	if err := e.call("lock bitstream", e.fn.lockBS, l); err != nil {
		return err
	}
	n := int(rd32(l, nvoLBSize))
	pkt := e.packets.get(n)
	copy(pkt.Data, cBytes(rdPtr(l, nvoLBPtr), n))
	t := rd32(l, nvoLBType)
	pkt.Keyframe = t == nvPicIDR || t == nvPicI
	pkt.PTS = timeFrom(rd64(l, nvoLBTS))
	e.out.push(pkt)
	if r := e.c.C2(e.fn.unlockBS, e.enc, e.bs); int32(r) != 0 {
		return e.fail("unlock bitstream", r)
	}
	return nil
}

func (e *nvEncoder) sendEOS() error {
	clear(e.pic)
	le32(e.pic, 0, nvPicVer)
	le32(e.pic, nvoPicFlags, nvFlagEOS)
	return e.call("end of stream", e.fn.encode, e.pic)
}

func (e *nvEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *nvEncoder) Flush() error {
	if e.flushed {
		return nil
	}
	e.flushed = true
	return e.sendEOS()
}
func (e *nvEncoder) Backend() BackendInfo { return e.info }

func (e *nvEncoder) Close() error {
	if e.enc == 0 {
		return nil
	}
	if !e.flushed && e.bs != 0 && e.input != 0 {
		e.flushed = true
		e.sendEOS()
	}
	if e.input != 0 {
		purego.SyscallN(e.fn.destroyInput, e.enc, e.input)
	}
	if e.bs != 0 {
		purego.SyscallN(e.fn.destroyBS, e.enc, e.bs)
	}
	purego.SyscallN(e.fn.destroy, e.enc)
	e.enc, e.input, e.bs = 0, 0, 0
	return nil
}

func nvidiaBackends() []backend {
	decAvail := func(*Library) error {
		if _, err := loadCUDA(); err != nil {
			return err
		}
		_, err := loadCUVID()
		return err
	}
	encAvail := func(*Library) error {
		if _, err := loadCUDA(); err != nil {
			return err
		}
		_, err := loadNVENC()
		return err
	}
	var out []backend
	for _, c := range []Codec{H264, H265, VP8, VP9, JPEG} {
		info := BackendInfo{"nvdec", c, Decode, true}
		out = append(out, backend{BackendInfo: info, avail: decAvail,
			vdec: func(l *Library, cfg VideoConfig) (VideoDecoder, error) { return newNVDecoder(l, cfg, info) }})
	}
	for _, c := range []Codec{H264, H265} {
		info := BackendInfo{"nvenc", c, Encode, true}
		out = append(out, backend{BackendInfo: info, avail: encAvail,
			venc: func(l *Library, cfg VideoConfig) (VideoEncoder, error) { return newNVEncoder(l, cfg, info) }})
	}
	return out
}
