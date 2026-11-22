//go:build android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/aep/gaal/internal/dl"
)

// Android MediaCodec through the NDK, in synchronous buffer mode.

const (
	mcFlagKey    = 1
	mcFlagConfig = 2
	mcFlagEOS    = 4

	mcTryLater      = -1
	mcFormatChanged = -2
	mcBuffersChange = -3

	mcConfigureEncode = 1

	mcColorI420     = 19
	mcColorNV12     = 21
	mcColorFlexible = 0x7F420888

	mcInputWaitUs = 5_000
	mcInputTries  = 200
	mcFlushWaitUs = 10_000

	mcRangeFull    = 1 // MediaFormat.COLOR_RANGE_FULL
	mcRangeLimited = 2 // MediaFormat.COLOR_RANGE_LIMITED

	mcPollInterval = 100 * time.Microsecond

	// mcOperatingRate asks for the highest clocks. The Galaxy S25 encodes 1080p
	// in 2.9 ms this way and 6.9 ms at 240, with an identical bitstream, and its
	// memory runs faster too, so copies take 40% of the time. Codecs that refuse
	// it are opened again with mcOperatingRateFallback.
	mcOperatingRate         = 32767 // Short.MAX_VALUE
	mcOperatingRateFallback = 240
)

type mediaAPI struct {
	createDecoder, createEncoder func(mime string) uintptr
	configure                    func(codec, format, surface, crypto uintptr, flags uint32) int32
	start, stop                  func(codec uintptr) int32
	del                          func(codec uintptr) int32
	outputFormat                 func(codec uintptr) uintptr
	formatNew                    func() uintptr
	formatDelete                 func(format uintptr) int32
	setString                    func(format uintptr, key, value string)
	setInt32                     func(format uintptr, key string, value int32)
	getInt32                     func(format uintptr, key string, out *int32) bool
	setBuffer                    func(format uintptr, key string, data unsafe.Pointer, size uintptr)

	// Optional, depending on the API level.
	inputFormat   func(codec uintptr) uintptr
	getName       func(codec uintptr, out *uintptr) int32
	bufferFormat  func(codec uintptr, index uintptr) uintptr
	releaseName   func(codec, name uintptr)
	setParameters func(codec, format uintptr) int32

	setAsync uintptr

	dequeueInput, inputBuffer, queueInput uintptr
	dequeueOutput, outputBuffer, release  uintptr
}

var loadMedia = sync.OnceValues(func() (*mediaAPI, error) {
	h, err := dl.Open("libmediandk.so")
	if err != nil {
		return nil, err
	}
	a := &mediaAPI{}
	err = dl.BindAll(h, dl.Table{
		"AMediaCodec_createDecoderByType": &a.createDecoder, "AMediaCodec_createEncoderByType": &a.createEncoder,
		"AMediaCodec_configure": &a.configure, "AMediaCodec_start": &a.start, "AMediaCodec_stop": &a.stop,
		"AMediaCodec_delete": &a.del, "AMediaCodec_getOutputFormat": &a.outputFormat,
		"AMediaFormat_new": &a.formatNew, "AMediaFormat_delete": &a.formatDelete,
		"AMediaFormat_setString": &a.setString, "AMediaFormat_setInt32": &a.setInt32,
		"AMediaFormat_getInt32": &a.getInt32, "AMediaFormat_setBuffer": &a.setBuffer,
		"AMediaCodec_dequeueInputBuffer": &a.dequeueInput, "AMediaCodec_getInputBuffer": &a.inputBuffer,
		"AMediaCodec_queueInputBuffer": &a.queueInput, "AMediaCodec_dequeueOutputBuffer": &a.dequeueOutput,
		"AMediaCodec_getOutputBuffer": &a.outputBuffer, "AMediaCodec_releaseOutputBuffer": &a.release,
	})
	if err != nil {
		return nil, err
	}
	for name, dst := range map[string]any{
		"AMediaCodec_getInputFormat": &a.inputFormat, "AMediaCodec_getBufferFormat": &a.bufferFormat, "AMediaCodec_getName": &a.getName,
		"AMediaCodec_releaseName": &a.releaseName, "AMediaCodec_setParameters": &a.setParameters, "AMediaCodec_setAsyncNotifyCallback": &a.setAsync,
	} {
		_ = dl.BindAll(h, dl.Table{name: dst}) // absent on older Android
	}
	startBinderThreads()
	return a, nil
})

// startBinderThreads lets codec events arrive in processes that are not Android
// apps. App processes already run a pool, and changing it there aborts.
func startBinderThreads() {
	if exe, err := os.Readlink("/proc/self/exe"); err != nil || strings.Contains(exe, "app_process") {
		return
	}
	h, err := dl.Open("libbinder_ndk.so")
	if err != nil {
		return
	}
	var setMax func(uint32) bool
	var start func()
	if dl.BindAll(h, dl.Table{"ABinderProcess_setThreadPoolMaxThreadCount": &setMax, "ABinderProcess_startThreadPool": &start}) == nil {
		setMax(4)
		start()
	}
}

type mcBufferInfo struct {
	offset, size int32
	pts          int64
	flags        uint32
	_            uint32
}

// mcCodec is the state shared by all stream types.
type mcCodec struct {
	shared      *mcShared
	callbacks   [4]uintptr
	timeout     syscall.Timespec
	inHead      uint64
	outHead     uint64
	seenFormats uint32
	current     int // output buffer being delivered, or -1
	c           dl.Caller
	api         *mediaAPI
	h           uintptr
	info        BackendInfo
	buf         mcBufferInfo
	size        uintptr
	started     bool
	flushed     bool
	finished    bool
	rate        int32 // operating rate for setRate
	rateUsed    bool
}

func mcMime(c Codec) string {
	if m := mcAudioMime(c); m != "" {
		return m
	}
	switch c {
	case H264:
		return "video/avc"
	case VP8:
		return "video/x-vnd.on2.vp8"
	case H265:
		return "video/hevc"
	case VP9:
		return "video/x-vnd.on2.vp9"
	}
	return "audio/opus"
}

// setRate requests the operating rate of this open attempt.
func (m *mcCodec) setRate(a *mediaAPI, format uintptr) {
	a.setInt32(format, "operating-rate", m.rate)
	m.rateUsed = true
}

// open creates, configures and starts a codec. setup fills the format. A codec
// that fails with the highest operating rate gets a second try with a lower one.
func (m *mcCodec) open(l *Library, info BackendInfo, setup func(a *mediaAPI, format uintptr)) error {
	m.rate = mcOperatingRate
	err := m.tryOpen(l, info, setup)
	if err != nil && m.rateUsed {
		m.rate, m.rateUsed = mcOperatingRateFallback, false
		err = m.tryOpen(l, info, setup)
	}
	return err
}

func (m *mcCodec) tryOpen(l *Library, info BackendInfo, setup func(a *mediaAPI, format uintptr)) error {
	a, err := loadMedia()
	if err != nil {
		return err
	}
	m.api, m.info = a, info
	mime := mcMime(info.Codec)
	flags := uint32(0)
	if info.Dir == Encode {
		m.h, flags = a.createEncoder(mime), mcConfigureEncode
	} else {
		m.h = a.createDecoder(mime)
	}
	if m.h == 0 {
		return fmt.Errorf("mediacodec: no %s %s", mime, info.Dir)
	}
	m.detectHardware()
	m.enableAsync()
	if l.opts.Hardware == RequireHardware && !m.info.Hardware {
		m.Close()
		return errors.New("mediacodec: only a software codec is available")
	}
	format := a.formatNew()
	defer a.formatDelete(format)
	a.setString(format, "mime", mime)
	a.setInt32(format, "priority", 0) // realtime
	a.setInt32(format, "low-latency", 1)
	setup(a, format)
	if rc := a.configure(m.h, format, 0, 0, flags); rc != 0 {
		m.Close()
		return fmt.Errorf("mediacodec: configure failed with status %d", rc)
	}
	if rc := a.start(m.h); rc != 0 {
		m.Close()
		return fmt.Errorf("mediacodec: start failed with status %d", rc)
	}
	m.started = true
	return nil
}

func (m *mcCodec) detectHardware() {
	a := m.api
	m.info.Hardware = false
	if a.getName == nil {
		return
	}
	var name uintptr
	if a.getName(m.h, &name) != 0 {
		return
	}
	n := strings.ToLower(dl.GoString(name))
	if a.releaseName != nil {
		a.releaseName(m.h, name)
	}
	m.info.Name = "mediacodec:" + n
	m.info.Hardware = !strings.HasPrefix(n, "omx.google.") && !strings.HasPrefix(n, "c2.android.") &&
		!strings.Contains(n, ".sw.")
}

func (m *mcCodec) Backend() BackendInfo { return m.info }

func (m *mcCodec) Close() error {
	if m.h != 0 {
		if m.started {
			m.api.stop(m.h)
		}
		m.api.del(m.h)
		m.h, m.started = 0, false
	}
	return nil
}

func (m *mcCodec) queue(idx, n int, pts time.Duration, flags uintptr) error {
	r := m.c.C6(m.api.queueInput, m.h, uintptr(idx), 0, uintptr(n), uintptr(pts/time.Microsecond), flags)
	if int32(r) != 0 {
		return fmt.Errorf("mediacodec: queue input failed with status %d", int32(r))
	}
	return nil
}

// sendEOS queues the end-of-stream marker.
func (m *mcCodec) sendEOS(drain func() error) error {
	if m.flushed {
		return nil
	}
	m.flushed = true
	idx, _, err := m.acquireInput(drain)
	if err != nil {
		return err
	}
	return m.queue(idx, 0, 0, mcFlagEOS)
}

// receiveWait is the output wait: none while streaming, short when draining.
func (m *mcCodec) receiveWait() uintptr {
	if m.flushed {
		return mcFlushWaitUs
	}
	return 0
}

func noFormat() error { return nil }

// --- video decoder ---

type mcVideoDecoder struct {
	mcCodec
	frames                       FramePool
	out                          fifo[*VideoFrame]
	width, height, stride, slice int
	color                        int
	cropL, cropT                 int
	onFormat                     func() error
	onData                       func([]byte, *mcBufferInfo) error
	drain                        func() error
}

func newMCVideoDecoder(l *Library, cfg VideoConfig, info BackendInfo) (VideoDecoder, error) {
	d := &mcVideoDecoder{}
	w, h := cfg.Width, cfg.Height
	if w == 0 || h == 0 {
		w, h = 1920, 1080 // an upper hint, the stream carries the real size
	}
	if err := d.open(l, info, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "width", int32(w))
		a.setInt32(f, "height", int32(h))
		a.setInt32(f, "color-format", mcColorNV12)
		if d.info.Hardware {
			a.setInt32(f, "vendor.qti-ext-dec-low-latency.enable", 1)
			if cfg.LowDelay {
				// Qualcomm decoders output in decoding order with this key.
				a.setInt32(f, "vendor.qti-ext-dec-picture-order.enable", 1)
			}
			d.setRate(a, f)
		}
	}); err != nil {
		return nil, err
	}
	d.onFormat, d.onData = d.readFormat, d.frame
	d.drain = func() error { return d.pump(0, d.onFormat, d.onData) }
	return d, nil
}

func (d *mcVideoDecoder) readFormat() error {
	a := d.api
	var f uintptr
	if a.bufferFormat != nil && d.current >= 0 {
		f = a.bufferFormat(d.h, uintptr(d.current))
	}
	if f == 0 {
		f = a.outputFormat(d.h)
	}
	if f == 0 {
		return nil
	}
	defer a.formatDelete(f)
	get := func(key string, def int) int {
		var v int32
		if a.getInt32(f, key, &v) {
			return int(v)
		}
		return def
	}
	d.width, d.height = get("width", 0), get("height", 0)
	d.stride, d.slice = get("stride", d.width), get("slice-height", d.height)
	d.color = get("color-format", mcColorNV12)
	d.cropL, d.cropT = 0, 0
	// Android reports the crop rectangle through these keys.
	if r := get("crop-right", -1); r >= 0 {
		d.cropL, d.cropT = get("crop-left", 0), get("crop-top", 0)
		d.width, d.height = r-d.cropL+1, get("crop-bottom", d.height-1)-d.cropT+1
	}
	if d.stride < d.width {
		d.stride = d.width
	}
	if d.slice < d.height {
		d.slice = d.height
	}
	return nil
}

func (d *mcVideoDecoder) minSize() int {
	return d.stride*d.slice + d.stride*((d.height+1)/2-1) + d.width
}

func (d *mcVideoDecoder) frame(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 {
		return nil
	}
	// The format can lag behind the first buffers, so refresh it when it is
	// unknown or does not fit.
	if d.width == 0 || len(b) < d.minSize() {
		if err := d.readFormat(); err != nil {
			return err
		}
	}
	if d.width == 0 {
		return nil
	}
	s, sl := d.stride, d.slice
	cw, ch := (d.width+1)/2, (d.height+1)/2
	if len(b) < d.minSize() {
		return fmt.Errorf("mediacodec: short output buffer of %d bytes for %dx%d stride %d slice %d color %#x", len(b), d.width, d.height, s, sl, d.color)
	}
	y := b[d.cropT*s+d.cropL:]
	var f *VideoFrame
	if d.color == mcColorI420 {
		f = d.frames.Get(d.width, d.height, I420)
		us := s / 2
		off := (d.cropT/2)*us + d.cropL/2
		copyPlane(f.Y, f.YStride, y, s, d.width, d.height)
		copyPlane(f.U, f.UVStride, b[s*sl+off:], us, cw, ch)
		copyPlane(f.V, f.UVStride, b[s*sl+us*(sl/2)+off:], us, cw, ch)
	} else {
		f = d.frames.Get(d.width, d.height, NV12)
		copyPlane(f.Y, f.YStride, y, s, d.width, d.height)
		copyPlane(f.U, f.UVStride, b[s*sl+(d.cropT/2)*s+d.cropL&^1:], s, 2*cw, ch)
	}
	f.PTS = time.Duration(info.pts) * time.Microsecond
	d.out.push(f)
	return nil
}

func (d *mcVideoDecoder) Send(p Packet) error {
	idx, buf, err := d.acquireInput(d.drain)
	if err != nil {
		return err
	}
	if len(p.Data) > len(buf) {
		return fmt.Errorf("%w: packet of %d bytes exceeds codec buffer", ErrInvalid, len(p.Data))
	}
	copy(buf, p.Data)
	var flags uintptr
	if err := d.queue(idx, len(p.Data), p.PTS, flags); err != nil {
		return err
	}
	return d.drain()
}

func (d *mcVideoDecoder) Receive() (*VideoFrame, error) {
	for {
		if f, ok := d.out.pop(); ok {
			return f, nil
		}
		if d.finished {
			return nil, endOfQueue(true)
		}
		if err := d.pump(d.receiveWait(), d.onFormat, d.onData); err != nil {
			return nil, err
		}
		if d.out.head == len(d.out.items) && !d.flushed {
			return nil, ErrAgain
		}
	}
}

func (d *mcVideoDecoder) Flush() error { return d.sendEOS(d.drain) }

func (d *mcVideoDecoder) Close() error {
	for {
		f, ok := d.out.pop()
		if !ok {
			return d.mcCodec.Close()
		}
		f.Release()
	}
}

// --- video encoder ---

type mcVideoEncoder struct {
	mcCodec
	cfg           VideoConfig
	stride, slice int
	config        []byte // SPS and PPS, repeated before each keyframe
	packets       packetPool
	out           fifo[*EncodedPacket]
	onData        func([]byte, *mcBufferInfo) error
	drain         func() error
	syncParams    uintptr
	rateParams    uintptr // AMediaFormat holding video-bitrate, made on first use
}

func newMCVideoEncoder(l *Library, cfg VideoConfig, info BackendInfo) (VideoEncoder, error) {
	e := &mcVideoEncoder{cfg: cfg, stride: cfg.Width, slice: cfg.Height}
	const bitrateModeCBR = 2
	if err := e.open(l, info, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "width", int32(cfg.Width))
		a.setInt32(f, "height", int32(cfg.Height))
		a.setInt32(f, "color-format", mcColorNV12)
		a.setInt32(f, "bitrate", int32(cfg.Bitrate))
		a.setInt32(f, "bitrate-mode", bitrateModeCBR)
		a.setInt32(f, "frame-rate", int32(cfg.FPS))
		a.setInt32(f, "i-frame-interval", int32(max(1, cfg.KeyframeEvery/cfg.FPS)))
		a.setInt32(f, "max-bframes", 0)
		switch cfg.Range {
		case RangeFull:
			a.setInt32(f, "color-range", mcRangeFull)
		case RangeLimited:
			a.setInt32(f, "color-range", mcRangeLimited)
		}
		if e.info.Hardware {
			a.setInt32(f, "latency", 1)
			a.setInt32(f, "prepend-sps-pps-to-idr-frames", 1)
			a.setInt32(f, "vendor.qti-ext-enc-low-latency.enable", 1)
			e.setRate(a, f)
		}
	}); err != nil {
		return nil, err
	}
	a := e.api
	if a.inputFormat != nil {
		if f := a.inputFormat(e.h); f != 0 {
			var v int32
			if a.getInt32(f, "stride", &v) && int(v) >= cfg.Width {
				e.stride = int(v)
			}
			if a.getInt32(f, "slice-height", &v) && int(v) >= cfg.Height {
				e.slice = int(v)
			}
			a.formatDelete(f)
		}
	}
	if a.setParameters != nil {
		e.syncParams = a.formatNew()
		a.setInt32(e.syncParams, "request-sync", 0)
	}
	e.onData = e.packet
	e.drain = func() error { return e.pump(0, noFormat, e.onData) }
	return e, nil
}

func (e *mcVideoEncoder) packet(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 {
		e.config = append(e.config[:0], b...)
		return nil
	}
	key := info.flags&mcFlagKey != 0
	prefix := 0
	if key && (e.cfg.Codec == H264 || e.cfg.Codec == H265) {
		prefix = len(e.config)
	}
	p := e.packets.get(prefix + len(b))
	copy(p.Data, e.config[:prefix])
	copy(p.Data[prefix:], b)
	p.PTS, p.Keyframe = time.Duration(info.pts)*time.Microsecond, key
	e.out.push(p)
	return nil
}

func (e *mcVideoEncoder) Send(f *VideoFrame) error {
	if err := checkEncoderFrame(f, e.cfg); err != nil {
		return err
	}
	if f.ForceKeyframe && e.syncParams != 0 {
		e.api.setParameters(e.h, e.syncParams)
	}
	idx, buf, err := e.acquireInput(e.drain)
	if err != nil {
		return err
	}
	uvAt := e.stride * e.slice
	need := uvAt + e.stride*(f.Height/2-1) + f.Width
	if len(buf) < need {
		return fmt.Errorf("mediacodec: input buffer of %d bytes is smaller than %d", len(buf), need)
	}
	writeNV12(buf, buf[uvAt:], e.stride, f)
	if err := e.queue(idx, uvAt+e.stride*(f.Height/2), f.PTS, 0); err != nil {
		return err
	}
	return e.drain()
}

func (e *mcVideoEncoder) Receive() (*EncodedPacket, error) {
	for {
		if p, ok := e.out.pop(); ok {
			return p, nil
		}
		if e.finished {
			return nil, endOfQueue(true)
		}
		if err := e.pump(e.receiveWait(), noFormat, e.onData); err != nil {
			return nil, err
		}
		if e.out.head == len(e.out.items) && !e.flushed {
			return nil, ErrAgain
		}
	}
}

func (e *mcVideoEncoder) Flush() error { return e.sendEOS(e.drain) }

// SetBitrate implements BitrateSetter. It needs AMediaCodec_setParameters,
// which Android 8 (API 26) added.
func (e *mcVideoEncoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.h == 0 {
		return ErrClosed
	}
	a := e.api
	if a.setParameters == nil {
		return errBitrateUnsupported(e.info.Name)
	}
	if e.rateParams == 0 {
		e.rateParams = a.formatNew()
	}
	a.setInt32(e.rateParams, "video-bitrate", int32(min(bps, math.MaxInt32)))
	if st := a.setParameters(e.h, e.rateParams); st != 0 {
		return fmt.Errorf("mediacodec: bitrate %d refused with status %d", bps, st)
	}
	e.cfg.Bitrate = bps
	return nil
}

func (e *mcVideoEncoder) Close() error {
	if e.syncParams != 0 {
		e.api.formatDelete(e.syncParams)
		e.syncParams = 0
	}
	if e.rateParams != 0 {
		e.api.formatDelete(e.rateParams)
		e.rateParams = 0
	}
	return e.mcCodec.Close()
}

// --- Opus ---

type mcAudioDecoder struct {
	mcCodec
	lost   [1]byte
	cfg    AudioConfig
	frames audioPool
	out    fifo[*AudioFrame]
	onData func([]byte, *mcBufferInfo) error
	drain  func() error
}

func newMCAudioDecoder(l *Library, cfg AudioConfig, info BackendInfo) (AudioDecoder, error) {
	d := &mcAudioDecoder{cfg: cfg}
	d.lost[0] = 0xF8 // TOC: CELT fullband 20 ms, mono, code 0
	if cfg.Channels == 2 {
		d.lost[0] = 0xFC
	}
	head := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, byte(cfg.Channels), 0, 0, 0, 0, 0, 0, 0, 0, 0}
	*(*uint32)(unsafe.Pointer(&head[12])) = uint32(cfg.SampleRate)
	var preSkipNs, seekPrerollNs [8]byte
	*(*uint64)(unsafe.Pointer(&seekPrerollNs[0])) = 80_000_000
	if err := d.open(l, info, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "sample-rate", int32(cfg.SampleRate))
		a.setInt32(f, "channel-count", int32(cfg.Channels))
		a.setBuffer(f, "csd-0", unsafe.Pointer(&head[0]), uintptr(len(head)))
		a.setBuffer(f, "csd-1", unsafe.Pointer(&preSkipNs[0]), 8)
		a.setBuffer(f, "csd-2", unsafe.Pointer(&seekPrerollNs[0]), 8)
	}); err != nil {
		return nil, err
	}
	runtime.KeepAlive(head)
	d.onData = d.samples
	d.drain = func() error { return d.pump(0, noFormat, d.onData) }
	return d, nil
}

func (d *mcAudioDecoder) samples(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 {
		return nil
	}
	f := d.frames.get(len(b) / 2)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&f.Samples[0])), len(b)&^1), b)
	f.SampleRate, f.Channels = d.cfg.SampleRate, d.cfg.Channels
	f.PTS = time.Duration(info.pts) * time.Microsecond
	d.out.push(f)
	return nil
}

func (d *mcAudioDecoder) Send(p Packet) error {
	idx, buf, err := d.acquireInput(d.drain)
	if err != nil {
		return err
	}
	if len(p.Data) == 0 {
		// A lost packet. A frame of zero bytes makes Opus conceal 20 ms.
		p.Data = d.lost[:1]
	}
	if len(p.Data) > len(buf) {
		return fmt.Errorf("%w: packet exceeds codec buffer", ErrInvalid)
	}
	copy(buf, p.Data)
	if err := d.queue(idx, len(p.Data), p.PTS, 0); err != nil {
		return err
	}
	return d.drain()
}

func (d *mcAudioDecoder) Receive() (*AudioFrame, error) {
	for {
		if f, ok := d.out.pop(); ok {
			return f, nil
		}
		if d.finished {
			return nil, endOfQueue(true)
		}
		if err := d.pump(d.receiveWait(), noFormat, d.onData); err != nil {
			return nil, err
		}
		if d.out.head == len(d.out.items) && !d.flushed {
			return nil, ErrAgain
		}
	}
}

func (d *mcAudioDecoder) Flush() error { return d.sendEOS(d.drain) }

type mcAudioEncoder struct {
	mcCodec
	cfg     AudioConfig
	packets packetPool
	out     fifo[*EncodedPacket]
	onData  func([]byte, *mcBufferInfo) error
	drain   func() error
}

func newMCAudioEncoder(l *Library, cfg AudioConfig, info BackendInfo) (AudioEncoder, error) {
	e := &mcAudioEncoder{cfg: cfg}
	bitrate := cfg.Bitrate
	if bitrate == 0 {
		bitrate = 64000
	}
	if err := e.open(l, info, func(a *mediaAPI, f uintptr) {
		a.setInt32(f, "sample-rate", int32(cfg.SampleRate))
		a.setInt32(f, "channel-count", int32(cfg.Channels))
		a.setInt32(f, "bitrate", int32(bitrate))
	}); err != nil {
		return nil, err
	}
	e.onData = e.packet
	e.drain = func() error { return e.pump(0, noFormat, e.onData) }
	return e, nil
}

func (e *mcAudioEncoder) packet(b []byte, info *mcBufferInfo) error {
	if info.flags&mcFlagConfig != 0 {
		return nil
	}
	p := e.packets.get(len(b))
	copy(p.Data, b)
	p.PTS, p.Keyframe = time.Duration(info.pts)*time.Microsecond, true
	e.out.push(p)
	return nil
}

func (e *mcAudioEncoder) Send(f *AudioFrame) error {
	if f.SampleRate != e.cfg.SampleRate || f.Channels != e.cfg.Channels || len(f.Samples)%e.cfg.Channels != 0 {
		return fmt.Errorf("%w: audio frame does not match encoder format", ErrInvalid)
	}
	if len(f.Samples) == 0 {
		return nil
	}
	pcm := unsafe.Slice((*byte)(unsafe.Pointer(&f.Samples[0])), 2*len(f.Samples))
	pts := f.PTS
	bytesPerSecond := e.cfg.SampleRate * e.cfg.Channels * 2
	for len(pcm) > 0 {
		idx, buf, err := e.acquireInput(e.drain)
		if err != nil {
			return err
		}
		n := copy(buf, pcm)
		if err := e.queue(idx, n, pts, 0); err != nil {
			return err
		}
		pcm = pcm[n:]
		pts += time.Duration(n) * time.Second / time.Duration(bytesPerSecond)
	}
	return e.drain()
}

func (e *mcAudioEncoder) Receive() (*EncodedPacket, error) {
	for {
		if p, ok := e.out.pop(); ok {
			return p, nil
		}
		if e.finished {
			return nil, endOfQueue(true)
		}
		if err := e.pump(e.receiveWait(), noFormat, e.onData); err != nil {
			return nil, err
		}
		if e.out.head == len(e.out.items) && !e.flushed {
			return nil, ErrAgain
		}
	}
}

func (e *mcAudioEncoder) Flush() error { return e.sendEOS(e.drain) }

func mediaCodecBackends() []backend {
	avail := func(*Library) error { _, err := loadMedia(); return err }
	var out []backend
	for _, c := range []Codec{H264, H265, VP8, VP9} {
		dec := BackendInfo{"mediacodec", c, Decode, true}
		enc := BackendInfo{"mediacodec", c, Encode, true}
		out = append(out,
			backend{BackendInfo: dec, avail: avail, vdec: func(l *Library, cfg VideoConfig) (VideoDecoder, error) {
				return newMCVideoDecoder(l, cfg, dec)
			}},
			backend{BackendInfo: enc, avail: avail, venc: func(l *Library, cfg VideoConfig) (VideoEncoder, error) {
				return newMCVideoEncoder(l, cfg, enc)
			}})
	}
	dec := BackendInfo{"mediacodec", Opus, Decode, true}
	enc := BackendInfo{"mediacodec", Opus, Encode, true}
	return append(out,
		backend{BackendInfo: dec, avail: avail, adec: func(l *Library, cfg AudioConfig) (AudioDecoder, error) {
			return newMCAudioDecoder(l, cfg, dec)
		}},
		backend{BackendInfo: enc, avail: avail, aenc: func(l *Library, cfg AudioConfig) (AudioEncoder, error) {
			return newMCAudioEncoder(l, cfg, enc)
		}})
}

// mcMicros converts a wait to microseconds, capped so errors surface promptly.
func mcMicros(t time.Duration) uintptr {
	return uintptr(min(t, 20*time.Millisecond) / time.Microsecond)
}

// Wait implements Waiter.
func (d *mcVideoDecoder) Wait(t time.Duration) {
	if d.out.head == len(d.out.items) && !d.finished {
		d.pump(mcMicros(t), d.onFormat, d.onData)
	}
}

// Wait implements Waiter.
func (e *mcVideoEncoder) Wait(t time.Duration) {
	if e.out.head == len(e.out.items) && !e.finished {
		e.pump(mcMicros(t), noFormat, e.onData)
	}
}

// Wait implements Waiter.
func (d *mcAudioDecoder) Wait(t time.Duration) {
	if d.out.head == len(d.out.items) && !d.finished {
		d.pump(mcMicros(t), noFormat, d.onData)
	}
}

// Wait implements Waiter.
func (e *mcAudioEncoder) Wait(t time.Duration) {
	if e.out.head == len(e.out.items) && !e.finished {
		e.pump(mcMicros(t), noFormat, e.onData)
	}
}
