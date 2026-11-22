//go:build !windows && (amd64 || arm64)

package gaal

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// libvpx for VP8, loaded from the system. This is a software codec. Struct
// layouts depend on the library ABI, so a library with a different ABI version
// is refused at init rather than used with the wrong layout.

// vpxSonames are the libvpx versions whose layouts match vpx_layout.go, newest
// first: 1.16 and later (12), 1.15 (11) and 1.14 (9). Only the encoder ABI
// version differs between them, which counts changes to the external rate
// control interface gaal does not use.
var vpxSonames = []string{"12", "11", "9"}

// vpxEncABIs are the encoder ABI versions of those libraries: 1.16, 1.15.1,
// 1.15.0 and 1.14.
var vpxEncABIs = []int32{vpxEncABI, 37, 36, 34}

type vpxAPI struct {
	vp8cx, vp8dx, vp9cx, vp9dx func() uintptr
	cfgDefault                 func(iface uintptr, cfg unsafe.Pointer, usage uint32) int32
	encInit                    func(ctx unsafe.Pointer, iface uintptr, cfg unsafe.Pointer, flags int64, ver int32) int32
	decInit                    func(ctx unsafe.Pointer, iface uintptr, cfg unsafe.Pointer, flags int64, ver int32) int32
	destroy                    func(ctx unsafe.Pointer) int32
	control                    uintptr
	errStr                     func(ctx unsafe.Pointer) string
	encode                     uintptr
	getCX, decode              uintptr
	getFrame                   uintptr
	configSet                  uintptr
	encABI                     int32 // the one the library accepts
}

var loadVPX = sync.OnceValues(func() (*vpxAPI, error) {
	var names []string
	for _, v := range vpxSonames {
		names = append(names, libNames("vpx", v)...)
	}
	h, err := dl.Open(names...)
	if err != nil {
		return nil, err
	}
	a := &vpxAPI{}
	err = dl.BindAll(h, dl.Table{
		"vpx_codec_vp8_cx": &a.vp8cx, "vpx_codec_vp8_dx": &a.vp8dx,
		"vpx_codec_vp9_cx": &a.vp9cx, "vpx_codec_vp9_dx": &a.vp9dx,
		"vpx_codec_enc_config_default": &a.cfgDefault, "vpx_codec_enc_init_ver": &a.encInit,
		"vpx_codec_dec_init_ver": &a.decInit, "vpx_codec_destroy": &a.destroy,
		"vpx_codec_control_": &a.control, "vpx_codec_error": &a.errStr,
		"vpx_codec_encode": &a.encode, "vpx_codec_get_cx_data": &a.getCX,
		"vpx_codec_decode": &a.decode, "vpx_codec_get_frame": &a.getFrame,
		"vpx_codec_enc_config_set": &a.configSet,
	})
	if err != nil {
		return nil, err
	}
	return a, a.probe()
})

// probe rejects a library whose ABI differs from the generated layouts.
func (a *vpxAPI) probe() error {
	ctx := make([]byte, vpxCtxSize)
	p := unsafe.Pointer(&ctx[0])
	if rc := a.decInit(p, a.vp8dx(), nil, 0, vpxDecABI); rc != 0 {
		return fmt.Errorf("libvpx: incompatible decoder ABI (code %d)", rc)
	}
	a.destroy(p)
	cfg := make([]byte, vpxCfgSize)
	iface := a.vp8cx()
	if rc := a.cfgDefault(iface, unsafe.Pointer(&cfg[0]), 0); rc != 0 {
		return fmt.Errorf("libvpx: default config failed (code %d)", rc)
	}
	le32(cfg, vpxCW, 16)
	le32(cfg, vpxCH, 16)
	var rc int32
	for _, a.encABI = range vpxEncABIs {
		if rc = a.encInit(p, iface, unsafe.Pointer(&cfg[0]), 0, a.encABI); rc == 0 {
			a.destroy(p)
			return nil
		}
	}
	return fmt.Errorf("libvpx: incompatible encoder ABI (code %d)", rc)
}

func (a *vpxAPI) ifaces(c Codec) (encoder, decoder uintptr) {
	if c == VP9 {
		return a.vp9cx(), a.vp9dx()
	}
	return a.vp8cx(), a.vp8dx()
}

func vpxBackends() []backend {
	avail := func(*Library) error { _, err := loadVPX(); return err }
	var out []backend
	for _, c := range []Codec{VP8, VP9} {
		dec := BackendInfo{"libvpx", c, Decode, false}
		enc := BackendInfo{"libvpx", c, Encode, false}
		out = append(out,
			backend{BackendInfo: dec, avail: avail, vdec: func(l *Library, cfg VideoConfig) (VideoDecoder, error) { return newVPXDecoder(l, cfg, dec) }},
			backend{BackendInfo: enc, avail: avail, venc: func(l *Library, cfg VideoConfig) (VideoEncoder, error) { return newVPXEncoder(l, cfg, enc) }})
	}
	return out
}

// libvpx error codes that describe the bitstream.
const (
	vpxErrUnsupBitstream = 5
	vpxErrCorruptFrame   = 7
)

const (
	vp8TokenPartitions = 18 // VP8E_SET_TOKEN_PARTITIONS
	vpxMaxIntraPct     = 26 // VP8E_SET_MAX_INTRA_BITRATE_PCT
	vp9AQMode          = 36 // VP9E_SET_AQ_MODE

	// maxIntraPct caps a keyframe at three average frames, as WebRTC does, so
	// it does not burst past what the link takes.
	maxIntraPct = 300
	vp9AQCyclic = 3 // refreshes part of each frame, for realtime

	// Realtime speeds, against 8 on a 1080p clip. VP9 speed 9 is 15% faster for
	// under 0.1 dB. VP8 speed 10 is 10-25% faster for no loss up to 8 Mbit/s and
	// 0.3 dB at 20 Mbit/s. VP8 speed 12 loses 3.6 dB at 20 Mbit/s.
	vp8Speed = 10
	vp9Speed = 9
)

// vpxEncoderThreads picks the encoder threads. VP9 row multithreading keeps
// scaling to 8 threads, but only half the cores are used so it does not starve
// the rest of a pipeline. VP8 gains little past 4.
func vpxEncoderThreads(c Codec) int {
	n := runtime.NumCPU()
	if c == VP9 {
		return max(min(4, n), min(8, n/2))
	}
	return min(4, n)
}

// vp8PartitionBits returns log2 of the token partitions for a thread count.
// libvpx decodes VP8 on one thread unless the stream has several partitions.
func vp8PartitionBits(threads int) uintptr {
	bits := uintptr(0)
	for bits < 3 && 1<<bits < threads {
		bits++
	}
	return bits
}

type vpxStream struct {
	info BackendInfo
	c    dl.Caller
	api  *vpxAPI
	ctx  []byte
	iter uintptr
	open bool
}

func (s *vpxStream) ctxPtr() unsafe.Pointer { return unsafe.Pointer(&s.ctx[0]) }
func (s *vpxStream) fail(what string) error {
	return fmt.Errorf("libvpx: %s: %s", what, s.api.errStr(s.ctxPtr()))
}
func (s *vpxStream) Close() error {
	if s.open {
		s.api.destroy(s.ctxPtr())
		s.open = false
	}
	return nil
}

type vpxDecoder struct {
	vpxStream
	frames  FramePool
	out     fifo[*VideoFrame]
	flushed bool
}

func newVPXDecoder(_ *Library, _ VideoConfig, info BackendInfo) (VideoDecoder, error) {
	a, err := loadVPX()
	if err != nil {
		return nil, err
	}
	d := &vpxDecoder{vpxStream: vpxStream{info: info, api: a, ctx: make([]byte, vpxCtxSize)}}
	_, iface := a.ifaces(info.Codec)
	decCfg := make([]byte, vpxDecCfgSize)
	le32(decCfg, 0, uint32(min(4, runtime.NumCPU()))) // threads
	if rc := a.decInit(d.ctxPtr(), iface, unsafe.Pointer(&decCfg[0]), 0, vpxDecABI); rc != 0 {
		return nil, fmt.Errorf("libvpx: decoder init failed with code %d", rc)
	}
	d.open = true
	return d, nil
}

func (d *vpxDecoder) collect(pts time.Duration) {
	d.iter = 0
	for {
		r := d.c.C2(d.api.getFrame, uintptr(d.ctxPtr()), uintptr(unsafe.Pointer(&d.iter)))
		if r == 0 {
			return
		}
		img := cMem(r, vpxImgSize)
		w, h := int(rd32(img, vpxIDW)), int(rd32(img, vpxIDH))
		f := d.frames.Get(w, h, I420)
		f.PTS = pts
		cw, ch := (w+1)/2, (h+1)/2
		for i, dst := range [3][]byte{f.Y, f.U, f.V} {
			pw, ph, ds := cw, ch, f.UVStride
			if i == 0 {
				pw, ph, ds = w, h, f.YStride
			}
			ss := int(int32(rd32(img, vpxIStride+4*i)))
			copyPlane(dst, ds, cMem(rdPtr(img, vpxIPlanes+8*i), ss*ph), ss, pw, ph)
		}
		d.out.push(f)
	}
}

func (d *vpxDecoder) Send(p Packet) error {
	if len(p.Data) == 0 {
		return fmt.Errorf("%w: empty packet", ErrInvalid)
	}
	rc := d.c.C5(d.api.decode, uintptr(d.ctxPtr()), uintptr(unsafe.Pointer(&p.Data[0])), uintptr(len(p.Data)), 0, 0)
	runtime.KeepAlive(p.Data)
	if code := int32(rc); code == vpxErrUnsupBitstream || code == vpxErrCorruptFrame {
		return fmt.Errorf("%w: libvpx: %s", ErrCorrupt, d.api.errStr(d.ctxPtr()))
	} else if code != 0 {
		return d.fail("decode")
	}
	d.collect(p.PTS)
	return nil
}

func (d *vpxDecoder) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}
func (d *vpxDecoder) Flush() error         { d.flushed = true; return nil }
func (d *vpxDecoder) Backend() BackendInfo { return d.info }

type vpxEncoder struct {
	vpxStream
	cfg      VideoConfig
	vcfg     []byte
	img      []byte
	scratch  *VideoFrame
	duration uintptr
	packets  packetPool
	out      fifo[*EncodedPacket]
	flushed  bool
}

func newVPXEncoder(_ *Library, cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	a, err := loadVPX()
	if err != nil {
		return nil, err
	}
	e := &vpxEncoder{vpxStream: vpxStream{info: info, api: a, ctx: make([]byte, vpxCtxSize)}, cfg: cfg,
		vcfg: make([]byte, vpxCfgSize), img: make([]byte, vpxImgSize), duration: uintptr(1_000_000 / cfg.FPS)}
	iface, _ := a.ifaces(info.Codec)
	threads := vpxEncoderThreads(info.Codec)
	c := e.vcfg
	if rc := a.cfgDefault(iface, unsafe.Pointer(&c[0]), 0); rc != 0 {
		return nil, fmt.Errorf("libvpx: default config failed with code %d", rc)
	}
	for _, f := range []struct {
		off int
		v   int
	}{{vpxCThreads, threads}, {vpxCW, cfg.Width}, {vpxCH, cfg.Height},
		{vpxCTBNum, 1}, {vpxCTBDen, 1_000_000}, {vpxCLag, 0}, {vpxCPass, 0}, {vpxCDrop, 0},
		{vpxCEnd, vpxCBR}, {vpxCRate, cfg.Bitrate / 1000}, {vpxCBuf, 300}, {vpxCBufInit, 100}, {vpxCBufOpt, 200},
		{vpxCKFMode, vpxKFAuto}, {vpxCKFMin, 0}, {vpxCKFMax, cfg.KeyframeEvery}} {
		le32(c, f.off, uint32(f.v))
	}
	if rc := a.encInit(e.ctxPtr(), iface, unsafe.Pointer(&c[0]), 0, a.encABI); rc != 0 {
		return nil, fmt.Errorf("libvpx: encoder init failed with code %d", rc)
	}
	e.open = true
	speed := uintptr(vp8Speed)
	if info.Codec == VP9 {
		speed = vp9Speed
	}
	if int32(e.c.Variadic(a.control, uintptr(e.ctxPtr()), vpxCPUUsed, speed)) != 0 {
		defer e.Close()
		return nil, e.fail("set speed")
	}
	e.c.Variadic(a.control, uintptr(e.ctxPtr()), vpxMaxIntraPct, maxIntraPct)
	if info.Codec == VP8 {
		e.c.Variadic(a.control, uintptr(e.ctxPtr()), vp8TokenPartitions, vp8PartitionBits(threads))
	}
	if info.Codec == VP9 {
		tiles := uintptr(0)
		if cfg.Width >= 1280 {
			tiles = 1
		}
		if cfg.Width >= 1920 {
			tiles = 2
		}
		e.c.Variadic(a.control, uintptr(e.ctxPtr()), vp9TileColumns, tiles)
		e.c.Variadic(a.control, uintptr(e.ctxPtr()), vp9RowMT, 1)
		e.c.Variadic(a.control, uintptr(e.ctxPtr()), vp9AQMode, vp9AQCyclic)
		if cfg.Range == RangeFull {
			e.c.Variadic(a.control, uintptr(e.ctxPtr()), vp9ColorRange, 1)
		}
	}
	i := e.img
	le32(i, 0, vpxFmtI420)
	for _, f := range []struct{ off, v int }{{vpxIW, cfg.Width}, {vpxIH, cfg.Height}, {vpxIDW, cfg.Width}, {vpxIDH, cfg.Height},
		{vpxIDepth, 8}, {vpxIBPS, 12}, {vpxIXShift, 1}, {vpxIYShift, 1}} {
		le32(i, f.off, uint32(f.v))
	}
	return e, nil
}

// SetBitrate implements BitrateSetter.
func (e *vpxEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if !e.open {
		return ErrClosed
	}
	old := rd32(e.vcfg, vpxCRate)
	le32(e.vcfg, vpxCRate, uint32(max(1, bps/1000)))
	if int32(e.c.C2(e.api.configSet, uintptr(e.ctxPtr()), uintptr(unsafe.Pointer(&e.vcfg[0])))) != 0 {
		le32(e.vcfg, vpxCRate, old)
		return e.fail("set bitrate")
	}
	e.cfg.Bitrate = bps
	return nil
}

func (e *vpxEncoder) collect() {
	e.iter = 0
	for {
		r := e.c.C2(e.api.getCX, uintptr(e.ctxPtr()), uintptr(unsafe.Pointer(&e.iter)))
		if r == 0 {
			return
		}
		pkt := cMem(r, vpxPFlags+4)
		if rd32(pkt, vpxPKind) != vpxPktFrame {
			continue
		}
		n := int(rd64(pkt, vpxPSize))
		p := e.packets.get(n)
		copy(p.Data, cMem(rdPtr(pkt, vpxPBuf), n))
		p.PTS = time.Duration(rd64(pkt, vpxPPTS)) * time.Microsecond
		p.Keyframe = rd32(pkt, vpxPFlags)&vpxFrameKey != 0
		e.out.push(p)
	}
}

func (e *vpxEncoder) Send(f *VideoFrame) error {
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
	i := e.img
	le64(i, vpxIPlanes, uint64(uintptr(unsafe.Pointer(&src.Y[0]))))
	le64(i, vpxIPlanes+8, uint64(uintptr(unsafe.Pointer(&src.U[0]))))
	le64(i, vpxIPlanes+16, uint64(uintptr(unsafe.Pointer(&src.V[0]))))
	le32(i, vpxIStride, uint32(src.YStride))
	le32(i, vpxIStride+4, uint32(src.UVStride))
	le32(i, vpxIStride+8, uint32(src.UVStride))
	var flags uintptr
	if f.ForceKeyframe {
		flags = vpxForceKF
	}
	rc := e.c.C6(e.api.encode, uintptr(e.ctxPtr()), uintptr(unsafe.Pointer(&i[0])),
		uintptr(f.PTS/time.Microsecond), e.duration, flags, vpxRealtime)
	runtime.KeepAlive(src)
	if int32(rc) != 0 {
		return e.fail("encode")
	}
	e.collect()
	return nil
}

func (e *vpxEncoder) Receive() (*EncodedPacket, error) {
	if p, ok := e.out.pop(); ok {
		return p, nil
	}
	return nil, endOfQueue(e.flushed)
}

func (e *vpxEncoder) Flush() error {
	if e.flushed {
		return nil
	}
	e.flushed = true
	if rc := e.c.C6(e.api.encode, uintptr(e.ctxPtr()), 0, 0, e.duration, 0, vpxRealtime); int32(rc) != 0 {
		return e.fail("flush")
	}
	e.collect()
	return nil
}
func (e *vpxEncoder) Backend() BackendInfo { return e.info }
