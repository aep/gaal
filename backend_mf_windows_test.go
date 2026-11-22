//go:build windows && (amd64 || arm64)

package gaal

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

func mfDecode(list []string) [][]byte {
	out := make([][]byte, len(list))
	for i, s := range list {
		out[i], _ = base64.StdEncoding.DecodeString(s)
	}
	return out
}

func mfTestAPI(t testing.TB) *mfAPI {
	t.Helper()
	api, err := loadMF()
	if err != nil {
		t.Skip(err)
	}
	return api
}

func mfLib() *Library { return New(Options{Backend: mfBackendName}) }

// --- COM plumbing ---

// TestMFCallAllocs compares the call paths available for hot code.
func TestMFCallAllocs(t *testing.T) {
	api := mfTestAPI(t)
	m := &mfTransform{api: api}
	buf, err := m.newBuffer(64, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer m.c.release(buf)
	addRef, release := mfMethod(buf, slotAddRef), mfMethod(buf, slotRelease)
	args := []uintptr{buf}
	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"syscall.SyscallN", func() { syscall.SyscallN(addRef, args...); syscall.SyscallN(release, args...) }},
		{"purego.SyscallN", func() { purego.SyscallN(addRef, args...); purego.SyscallN(release, args...) }},
		{"mfCall", func() { m.c.addRef(buf); m.c.release(buf) }},
	} {
		a := testing.AllocsPerRun(1000, tc.fn)
		t.Logf("%s: %.2f allocations per AddRef+Release", tc.name, a)
		if a != 0 {
			t.Errorf("%s allocates %.2f", tc.name, a)
		}
	}
}

func TestMFSampleAndAttributes(t *testing.T) {
	api := mfTestAPI(t)
	m := &mfTransform{api: api}
	c := &m.c
	defer m.close()
	mt, err := api.newMediaType(c, &mediaVideo, &fmtNV12)
	if err != nil {
		t.Fatal(err)
	}
	defer c.release(mt)
	c.setU64(mt, &attrFrameSize, mfPair(640, 480))
	c.setU32(mt, &attrStride, 640)
	if v, ok := c.getU64(mt, &attrFrameSize); !ok || v != mfPair(640, 480) {
		t.Errorf("frame size %x %v", v, ok)
	}
	if v, ok := c.getGUID(mt, &attrSubtype); !ok || v != fmtNV12 {
		t.Errorf("subtype %v %v", v, ok)
	}
	blob := []byte{0, 0, 0, 1, 0x67, 1, 2}
	if hr := c.m3(mt, slotSetBlob, gp(&attrSeqHeader), uintptr(unsafe.Pointer(&blob[0])), uintptr(len(blob))); mfFailed(hr) {
		t.Fatalf("SetBlob %#x", hr)
	}
	if got := c.getBlob(mt, &attrSeqHeader); string(got) != string(blob) {
		t.Errorf("blob %x", got)
	}

	in, dst, err := m.begin(100)
	if err != nil {
		t.Fatal(err)
	}
	copy(dst, "hello")
	c.m0(in.buf, slotBufUnlock)
	if r := c.refs(in.sample); r != 1 {
		t.Errorf("sample refs %d", r)
	}
	if r := c.refs(in.buf); r != 2 {
		t.Errorf("buffer refs %d", r)
	}
	c.m1(in.buf, slotBufSetLen, 5)
	c.m1(in.sample, slotSampleSetTime, 12345)
	if got := m.samplePTS(in.sample); got != 1234500 {
		t.Errorf("sample time %v", got)
	}
	addr, _, size, err := m.lockSample(in.sample)
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 || string(cMem(addr, 5)) != "hello" {
		t.Errorf("sample content %d", size)
	}
	m.unlockSample()
	// A sample still referenced elsewhere must not be handed out again.
	c.addRef(in.sample)
	if m.freeInput() != nil {
		t.Error("referenced sample was reused")
	}
	c.release(in.sample)
	if m.freeInput() != in {
		t.Error("released sample was not reused")
	}
}

// Some encoders offset their timestamps. Input order must then decide.
func TestMFOffsetEncoderPTS(t *testing.T) {
	api := mfTestAPI(t)
	m := &mfTransform{api: api, orderedPTS: true}
	defer m.close()
	s, err := m.newSample()
	if err != nil {
		t.Fatal(err)
	}
	defer m.c.release(s)
	const offset = int64(1000 * time.Hour / 100)
	for i := 0; i < 3*mfPTSRingSize; i++ {
		want := time.Duration(i) * time.Second / 30
		m.notePTS(want)
		m.c.m1(s, slotSampleSetTime, uintptr(offset+mfHNS(want)))
		if got := m.samplePTS(s); got != want {
			t.Fatalf("frame %d: got %v, want %v", i, got, want)
		}
	}
}

func TestMFPTSRoundTrip(t *testing.T) {
	api := mfTestAPI(t)
	m := &mfTransform{api: api}
	defer m.close()
	s, err := m.newSample()
	if err != nil {
		t.Fatal(err)
	}
	defer m.c.release(s)
	want := time.Second / 30 // not a multiple of 100 ns
	m.c.m1(s, slotSampleSetTime, uintptr(m.notePTS(want)))
	if got := m.samplePTS(s); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMFEnumerate logs what is installed, and checks each listed backend opens.
func TestMFEnumerate(t *testing.T) {
	api := mfTestAPI(t)
	c := newMFCall()
	for _, tc := range []struct {
		codec Codec
		dir   Direction
	}{{H264, Decode}, {VP8, Decode}, {JPEG, Decode}, {Opus, Decode}, {H264, Encode}} {
		spec, _ := mfSpecFor(tc.codec, tc.dir)
		list, err := api.enumerate(c, spec)
		if err != nil {
			t.Errorf("%s %s: %v", tc.codec, tc.dir, err)
			continue
		}
		hw := 0
		for _, a := range list {
			if mfIsHardware(c, a) {
				hw++
			}
			c.release(a)
		}
		t.Logf("%s %s: %d transforms, %d hardware", tc.codec, tc.dir, len(list), hw)
	}
	for _, b := range allBackends() {
		if b.Name == mfBackendName && b.avail != nil {
			t.Logf("backend %s %s hardware=%v: %v", b.Codec, b.Dir, b.Hardware, b.avail(nil))
		}
	}
}

func TestMFHardwareDetection(t *testing.T) {
	mfTestAPI(t)
	dll, err := syscall.LoadDLL("mfplat.dll")
	if err != nil {
		t.Fatal(err)
	}
	create, err := dll.FindProc("MFCreateAttributes")
	if err != nil {
		t.Fatal(err)
	}
	c := newMFCall()
	attrs := func() uintptr {
		c.p = 0
		c.a[0], c.a[1] = uintptr(unsafe.Pointer(&c.p)), 2
		if hr := uint32(c.raw(create.Addr(), 2)); mfFailed(hr) {
			t.Fatalf("MFCreateAttributes %#x", hr)
		}
		return c.p
	}
	soft := attrs()
	defer c.release(soft)
	c.setU32(soft, &attrMFTFlags, 1)
	if mfIsHardware(c, soft) {
		t.Error("sync MFT reported as hardware")
	}
	flagged := attrs()
	defer c.release(flagged)
	c.setU32(flagged, &attrMFTFlags, mftEnumHardware|2)
	if !mfIsHardware(c, flagged) {
		t.Error("hardware flag ignored")
	}
	url := attrs()
	defer c.release(url)
	name, _ := syscall.UTF16PtrFromString("VEN_0000")
	if hr := c.m2(url, 25, gp(&attrHWURL), uintptr(unsafe.Pointer(name))); mfFailed(hr) { // SetString
		t.Fatalf("SetString %#x", hr)
	}
	if !mfIsHardware(c, url) {
		t.Error("hardware URL ignored")
	}
}

// mfFeed passes bytes through mfEncoder.onSample as if an encoder made them.
func mfFeed(t *testing.T, e *mfEncoder, data []byte) *EncodedPacket {
	t.Helper()
	m := e.m
	s, err := m.newSample()
	if err != nil {
		t.Fatal(err)
	}
	defer m.c.release(s)
	buf, err := m.newBuffer(len(data), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer m.c.release(buf)
	if err := m.setBuffer(s, buf); err != nil {
		t.Fatal(err)
	}
	m.c.p = 0
	m.c.m3(buf, slotBufLock, uintptr(unsafe.Pointer(&m.c.p)), 0, 0)
	copy(cMem(m.c.p, len(data)), data)
	m.c.m0(buf, slotBufUnlock)
	m.c.m1(buf, slotBufSetLen, uintptr(len(data)))
	if err := e.onSample(s); err != nil {
		t.Fatal(err)
	}
	p, ok := e.out.pop()
	if !ok {
		t.Fatal("no packet")
	}
	return p
}

func TestMFEncoderOutputNormalized(t *testing.T) {
	params := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2}
	idr := []byte{0, 0, 0, 1, 0x65, 3}
	e := &mfEncoder{m: &mfTransform{api: mfTestAPI(t)}, params: params}
	defer e.m.close()

	p := mfFeed(t, e, idr)
	if !p.Keyframe || string(p.Data) != string(params)+string(idr) {
		t.Errorf("IDR without parameter sets: key=%v %x", p.Keyframe, p.Data)
	}
	p.Release()

	p = mfFeed(t, e, []byte{0, 0, 0, 2, 0x41, 9}) // length-prefixed P slice
	if p.Keyframe || string(p.Data) != string([]byte{0, 0, 0, 1, 0x41, 9}) {
		t.Errorf("length-prefixed: key=%v %x", p.Keyframe, p.Data)
	}
	p.Release()

	// Parameter sets seen in the stream serve later keyframes.
	e.params = nil
	mfFeed(t, e, append(append([]byte{}, params...), idr...)).Release()
	p = mfFeed(t, e, idr)
	if string(p.Data) != string(params)+string(idr) {
		t.Errorf("cached parameter sets: %x", p.Data)
	}
	p.Release()
}

// TestMFCodecAPIReport logs which encoder properties the system accepts.
func TestMFCodecAPIReport(t *testing.T) {
	e := mfEncoderOrSkip(t, VideoConfig{Codec: H264, Width: 160, Height: 120})
	defer e.Close()
	m := e.(*failover[*VideoFrame, *EncodedPacket]).cur.(*mfEncoder).m
	t.Logf("async=%v hardware=%v ICodecAPI=%v", m.async, m.hw, m.codec != 0)
	if m.codec == 0 {
		return
	}
	for _, p := range []struct {
		name string
		key  *mfGUID
		vt   uint16
		v    uint64
	}{
		{"RateControlMode", &apiRateControl, vtUI4, mfRateControlCBR}, {"MeanBitRate", &apiMeanBitrate, vtUI4, 1000000},
		{"MaxBitRate", &apiMaxBitrate, vtUI4, 1000000}, {"GOPSize", &apiGOPSize, vtUI4, 30},
		{"DefaultBPictureCount", &apiBFrames, vtUI4, 0}, {"LowLatencyMode", &attrLowLatency, vtBool, 0xffff},
		{"ForceKeyFrame", &apiForceKey, vtUI4, 1},
	} {
		t.Logf("%s: hresult 0x%08x", p.name, m.c.setCodecValue(m.codec, p.key, p.vt, p.v))
	}
}

// --- Bitstream helpers ---

func TestMFNALHelpers(t *testing.T) {
	au := []byte{0, 0, 0, 1, 0x67, 9, 0, 0, 1, 0x68, 8, 0, 0, 1, 0x65, 7}
	if m := mfNALMask(au); m != 1<<7|1<<8|1<<5 {
		t.Errorf("mask %b", m)
	}
	if got := mfExtractParamSets(au); string(got) != string([]byte{0, 0, 0, 1, 0x67, 9, 0, 0, 0, 1, 0x68, 8}) {
		t.Errorf("params %x", got)
	}
	if mfValidH264([]byte("this is not h264")) || !mfValidH264(au) {
		t.Error("mfValidH264")
	}
	l := []byte{0, 0, 0, 2, 0x65, 1, 0, 0, 0, 1, 0x41}
	if !mfLengthToStartCodes(l) || mfNALMask(l) != 1<<5|1<<1 {
		t.Errorf("length conversion %x", l)
	}
	if mfLengthToStartCodes([]byte{0, 0, 9, 9, 1}) {
		t.Error("bad length accepted")
	}
	avcc := []byte{1, 66, 0, 30, 0xff, 0xe1, 0, 2, 0x67, 1, 1, 0, 2, 0x68, 2}
	want := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2}
	if got := mfParamSets(avcc); string(got) != string(want) {
		t.Errorf("avcC %x", got)
	}
}

func TestMFHeaderParsers(t *testing.T) {
	p, err := New(Options{Backend: "go-image-jpeg"}).EncodeJPEG(testFrame(64, 48, 0), 80)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if w, h, ok := mfJPEGSize(p.Data); !ok || w != 64 || h != 48 {
		t.Errorf("jpeg %d %d %v", w, h, ok)
	}
	if _, _, ok := mfJPEGSize([]byte("garbage garbage")); ok {
		t.Error("garbage accepted as JPEG")
	}
	vp8 := mfDecode(mfTestVP8)
	if w, h, key, ok := mfVP8Size(vp8[0]); !ok || !key || w != 64 || h != 48 {
		t.Errorf("vp8 %d %d %v %v", w, h, key, ok)
	}
	if _, _, key, ok := mfVP8Size(vp8[1]); !ok || key {
		t.Errorf("vp8 inter %v %v", key, ok)
	}
}

func TestMFConvert(t *testing.T) {
	// 4x2 picture with one row of padding columns and a 2x2 crop at x=2.
	f := NewVideoFrame(2, 2, NV12)
	nv12 := []byte{
		0, 0, 1, 2, 9, 9,
		0, 0, 3, 4, 9, 9,
		0, 0, 50, 60, 9, 9,
	}
	mfConvert(f, nv12, mfNV12, 6, 2, 2, 0)
	if string(f.Y) != string([]byte{1, 2, 3, 4}) || string(f.U[:2]) != string([]byte{50, 60}) {
		t.Errorf("nv12 %v %v", f.Y, f.U)
	}
	i420 := []byte{1, 2, 3, 4, 10, 20}
	mfConvert(f, i420, mfI420, 2, 2, 0, 0)
	if string(f.U[:2]) != string([]byte{10, 20}) {
		t.Errorf("i420 %v", f.U)
	}
	mfConvert(f, i420, mfYV12, 2, 2, 0, 0)
	if string(f.U[:2]) != string([]byte{20, 10}) {
		t.Errorf("yv12 %v", f.U)
	}
	yuy2 := []byte{1, 10, 2, 20, 3, 11, 4, 21}
	mfConvert(f, yuy2, mfYUY2, 4, 2, 0, 0)
	if string(f.Y) != string([]byte{1, 2, 3, 4}) || string(f.U[:2]) != string([]byte{10, 20}) {
		t.Errorf("yuy2 %v %v", f.Y, f.U)
	}
}

// --- Asynchronous driver, against a scripted MFT ---

// fakeMFT implements enough of IMFTransform in Go for the event protocol. Its
// generator is a real IMFMediaEventQueue, whose GetEvent has the same slot.
type fakeMFT struct {
	vtbl    *[26]uintptr
	c       *mfCall
	queue   uintptr
	attrs   uintptr
	starve  bool
	pending int
	inputs  int
	early   int // ProcessInput calls without a NeedInput credit
	credits int
	hns     int64
}

var (
	fake      *fakeMFT
	fakeVtbl  [26]uintptr
	fakeNull  mfGUID
	fakeVar   [3]uint64
	fakeSetup bool
)

func (f *fakeMFT) post(kind, status uint32) {
	f.c.m4(f.queue, 7, uintptr(kind), gp(&fakeNull), uintptr(status), uintptr(unsafe.Pointer(&fakeVar)))
}

func fakeInstall() {
	if fakeSetup {
		return
	}
	fakeSetup = true
	notImpl := syscall.NewCallback(func(this, a, b, c, d uintptr) uintptr { return hrNotImpl })
	for i := range fakeVtbl {
		fakeVtbl[i] = notImpl
	}
	set := func(slot int, fn func(this, a, b, c, d uintptr) uintptr) { fakeVtbl[slot] = syscall.NewCallback(fn) }
	store := func(dst, v uintptr) { *(*uintptr)(mfPtr(dst)) = v }
	set(slotQI, func(this, iid, out, _, _ uintptr) uintptr {
		switch *(*mfGUID)(mfPtr(iid)) {
		case iidEventGen:
			fake.c.addRef(fake.queue)
			store(out, fake.queue)
			return 0
		case iidTransform:
			store(out, this)
			return 0
		}
		store(out, 0)
		return 0x80004002
	})
	set(slotAddRef, func(_, _, _, _, _ uintptr) uintptr { return 2 })
	set(slotRelease, func(_, _, _, _, _ uintptr) uintptr { return 1 })
	set(slotGetAttributes, func(_, out, _, _, _ uintptr) uintptr {
		fake.c.addRef(fake.attrs)
		store(out, fake.attrs)
		return 0
	})
	set(slotInStreamInfo, func(_, _, out, _, _ uintptr) uintptr {
		*(*mftInputInfo)(mfPtr(out)) = mftInputInfo{}
		return 0
	})
	set(slotOutStreamInfo, func(_, _, out, _, _ uintptr) uintptr {
		*(*mftOutputInfo)(mfPtr(out)) = mftOutputInfo{size: 16}
		return 0
	})
	set(slotProcessMessage, func(_, msg, _, _, _ uintptr) uintptr {
		f := fake
		switch msg {
		case mftMsgStartStrm:
			if !f.starve {
				f.credits++
				f.post(meNeedInput, 0)
			}
		case mftMsgDrain:
			f.post(meDrainComplete, 0)
		}
		return 0
	})
	set(slotProcessInput, func(_, _, sample, _, _ uintptr) uintptr {
		f := fake
		if f.credits == 0 {
			f.early++
			return hrNotAccepting
		}
		f.credits--
		f.inputs++
		f.pending++
		f.c.m1(sample, slotSampleGetTime, uintptr(unsafe.Pointer(&f.c.i64)))
		f.hns = f.c.i64
		f.post(meHaveOutput, 0)
		f.credits++
		f.post(meNeedInput, 0)
		return 0
	})
	set(slotProcessOutput, func(_, _, _, buffers, _ uintptr) uintptr {
		f := fake
		if f.pending == 0 {
			return hrNeedMoreInput
		}
		f.pending--
		out := (*mftOutputBuffer)(mfPtr(buffers))
		f.c.m1(out.sample, slotSampleSetTime, uintptr(f.hns))
		return 0
	})
}

type countSink struct {
	m       *mfTransform
	samples int
	last    time.Duration
}

func (s *countSink) onSample(sample uintptr) error {
	s.samples++
	s.last = s.m.samplePTS(sample)
	return nil
}
func (s *countSink) onFormat() error { return nil }

func newFakeTransform(t *testing.T, starve bool) (*mfTransform, *countSink) {
	t.Helper()
	api := mfTestAPI(t)
	fakeInstall()
	dll, err := syscall.LoadDLL("mfplat.dll")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMFT{vtbl: &fakeVtbl, c: newMFCall(), starve: starve}
	for _, s := range []struct {
		name string
		n    int
		dst  *uintptr
	}{{"MFCreateEventQueue", 1, &f.queue}, {"MFCreateAttributes", 2, &f.attrs}} {
		p, err := dll.FindProc(s.name)
		if err != nil {
			t.Fatal(err)
		}
		f.c.a[0], f.c.a[1] = uintptr(unsafe.Pointer(s.dst)), 4
		if hr := uint32(f.c.raw(p.Addr(), s.n)); mfFailed(hr) {
			t.Fatalf("%s: %#x", s.name, hr)
		}
	}
	f.c.setU32(f.attrs, &attrAsync, 1)
	fake = f
	sink := &countSink{}
	m := &mfTransform{api: api, sink: sink, hw: true, timeout: 150 * time.Millisecond, xf: uintptr(unsafe.Pointer(f))}
	sink.m = m
	if err := m.bind(); err != nil {
		t.Fatal(err)
	}
	if !m.async {
		t.Fatal("transform not detected as asynchronous")
	}
	if v, ok := f.c.getU32(f.attrs, &attrAsyncOK); !ok || v != 1 {
		t.Error("MF_TRANSFORM_ASYNC_UNLOCK not set")
	}
	if err := m.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.c.m0(f.queue, 9) // IMFMediaEventQueue::Shutdown
		m.close()
		f.c.release(f.queue)
		f.c.release(f.attrs)
	})
	return m, sink
}

func fakeSend(t testing.TB, m *mfTransform, pts time.Duration) error {
	in, dst, err := m.begin(8)
	if err != nil {
		return err
	}
	copy(dst, "input")
	return m.commit(in, 5, pts, 0, false)
}

func TestMFAsyncProtocol(t *testing.T) {
	m, sink := newFakeTransform(t, false)
	const n = 20
	want := time.Second / 30
	for i := 0; i < n; i++ {
		if err := fakeSend(t, m, want); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := m.drain(); err != nil {
		t.Fatal(err)
	}
	if fake.early != 0 {
		t.Errorf("%d ProcessInput calls without METransformNeedInput", fake.early)
	}
	if fake.inputs != n || sink.samples != n {
		t.Errorf("inputs %d, outputs %d, want %d", fake.inputs, sink.samples, n)
	}
	if sink.last != want {
		t.Errorf("pts %v, want %v", sink.last, want)
	}
}

func TestMFAsyncAllocs(t *testing.T) {
	m, _ := newFakeTransform(t, false)
	step := func() {
		if err := fakeSend(t, m, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := m.service(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		step()
	}
	a := testing.AllocsPerRun(200, step)
	t.Logf("%.2f allocations per async input/output cycle", a)
	if a != 0 {
		t.Errorf("%.2f allocations", a)
	}
}

func TestMFAsyncSendIsBounded(t *testing.T) {
	m, _ := newFakeTransform(t, true)
	start := time.Now()
	err := fakeSend(t, m, 0)
	if err == nil || errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestMFAsyncDrainIsBounded(t *testing.T) {
	m, _ := newFakeTransform(t, true)
	fakeVtblDrain := fakeVtbl[slotProcessMessage]
	fakeVtbl[slotProcessMessage] = syscall.NewCallback(func(_, _, _, _, _ uintptr) uintptr { return 0 })
	defer func() { fakeVtbl[slotProcessMessage] = fakeVtblDrain }()
	if err := m.drain(); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestMFAsyncErrorEvents(t *testing.T) {
	m, _ := newFakeTransform(t, false)
	fake.post(meError, hrBadStreamData)
	if err := m.service(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("stream data error: %v", err)
	}
	if err := m.service(); err != nil {
		t.Errorf("stream unusable after corrupt data: %v", err)
	}
	m, _ = newFakeTransform(t, false)
	fake.post(meError, 0x80004005)
	if err := m.service(); err == nil || errors.Is(err, ErrCorrupt) {
		t.Errorf("device error: %v", err)
	}
}

// --- Real transforms, where the system has them ---

func mfDecoderOrSkip(t *testing.T, codec Codec) VideoDecoder {
	t.Helper()
	d, err := mfLib().NewVideoDecoder(VideoConfig{Codec: codec})
	if err != nil {
		t.Skip(err)
	}
	t.Logf("backend %+v", d.Backend())
	return d
}

func mfDecodeAll(t *testing.T, d VideoDecoder, pkts [][]byte) []*VideoFrame {
	t.Helper()
	var out []*VideoFrame
	keep := func(f *VideoFrame) { out = append(out, f) }
	for i, p := range pkts {
		if err := d.Send(Packet{Data: p, PTS: time.Duration(i) * time.Second / 30, Keyframe: i == 0}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		drain(t, d, keep)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, keep)
	return out
}

func TestMFDecodeH264Clip(t *testing.T) {
	d := mfDecoderOrSkip(t, H264)
	defer d.Close()
	pkts := mfDecode(mfTestH264)
	frames := mfDecodeAll(t, d, pkts)
	if len(frames) != len(pkts) {
		t.Errorf("decoded %d frames, want %d", len(frames), len(pkts))
	}
	for i, f := range frames {
		if f.Width != 64 || f.Height != 48 || f.Format != NV12 {
			t.Errorf("frame %d: %dx%d format %d", i, f.Width, f.Height, f.Format)
		}
		if want := time.Duration(i) * time.Second / 30; f.PTS != want {
			t.Errorf("frame %d: pts %v, want %v", i, f.PTS, want)
		}
		f.Release()
	}
}

func TestMFDecodeVP8Clip(t *testing.T) {
	d := mfDecoderOrSkip(t, VP8)
	defer d.Close()
	pkts := mfDecode(mfTestVP8)
	frames := mfDecodeAll(t, d, pkts)
	if len(frames) != len(pkts) {
		t.Errorf("decoded %d frames, want %d", len(frames), len(pkts))
	}
	for _, f := range frames {
		f.Release()
	}
}

func TestMFDecodeJPEG(t *testing.T) {
	src := testFrame(64, 48, 0)
	p, err := New(Options{Backend: "go-image-jpeg"}).EncodeJPEG(src, 90)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	d := mfDecoderOrSkip(t, JPEG)
	defer d.Close()
	frames := mfDecodeAll(t, d, [][]byte{p.Data})
	if len(frames) != 1 {
		t.Fatalf("decoded %d frames", len(frames))
	}
	if q := psnrY(src, frames[0]); q < 30 {
		t.Errorf("PSNR %.1f", q)
	}
	frames[0].Release()
}

func TestMFDecodeOpus(t *testing.T) {
	d, err := mfLib().NewAudioDecoder(AudioConfig{Codec: Opus})
	if err != nil {
		t.Skip(err)
	}
	defer d.Close()
	samples := 0
	count := func(f *AudioFrame) { samples += len(f.Samples); f.Release() }
	for _, p := range mfDecode(mfTestOpus) {
		if err := d.Send(Packet{Data: p}); err != nil {
			t.Fatal(err)
		}
		drain(t, d, count)
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, count)
	if samples == 0 {
		t.Error("no audio decoded")
	}
}

func TestMFCorruptInput(t *testing.T) {
	garbage := Packet{Data: []byte("this is certainly not compressed video")}
	for _, codec := range []Codec{H264, JPEG, VP8} {
		t.Run(codec.String(), func(t *testing.T) {
			d := mfDecoderOrSkip(t, codec)
			defer d.Close()
			if err := d.Send(garbage); !errors.Is(err, ErrCorrupt) {
				t.Errorf("got %v, want ErrCorrupt", err)
			}
			if _, err := d.Receive(); err != ErrAgain {
				t.Errorf("receive after corrupt input: %v", err)
			}
		})
	}
}

func mfEncoderOrSkip(t *testing.T, cfg VideoConfig) VideoEncoder {
	t.Helper()
	e, err := mfLib().NewVideoEncoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	t.Logf("backend %+v", e.Backend())
	return e
}

func TestMFEncodeH264(t *testing.T) {
	const w, h, frames = 320, 240, 30
	cfg := VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 1_000_000, KeyframeEvery: 10}
	e := mfEncoderOrSkip(t, cfg)
	defer e.Close()
	var pkts []Packet
	keep := func(p *EncodedPacket) { pkts = append(pkts, clonePacket(p.Packet)); p.Release() }
	for i := 0; i < frames; i++ {
		f := testFrame(w, h, i)
		f.ForceKeyframe = i == 15
		if err := e.Send(f); err != nil {
			t.Fatal(i, err)
		}
		drain(t, e, keep)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, e, keep)
	if _, err := e.Receive(); err != io.EOF {
		t.Errorf("after drain: %v", err)
	}
	if len(pkts) != frames {
		t.Errorf("got %d packets, want %d", len(pkts), frames)
	}
	keys := 0
	var last time.Duration = -1
	for i, p := range pkts {
		if !mfHasStartCode(p.Data) {
			t.Fatalf("packet %d is not Annex B: %x", i, p.Data[:min(8, len(p.Data))])
		}
		if p.PTS <= last {
			t.Errorf("packet %d: pts %v after %v, output is reordered", i, p.PTS, last)
		}
		last = p.PTS
		if p.Keyframe {
			keys++
			if m := mfNALMask(p.Data); m&(1<<7) == 0 || m&(1<<8) == 0 {
				t.Errorf("keyframe %d lacks SPS/PPS", i)
			}
		}
	}
	t.Logf("%d packets, %d keyframes", len(pkts), keys)
	if len(pkts) > 0 && !pkts[0].Keyframe {
		t.Error("first packet is not a keyframe")
	}
}

// TestMFRoundTrip checks real pictures through the encoder and decoder.
func TestMFRoundTrip(t *testing.T) {
	const w, h, frames = 320, 240, 20
	cfg := VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 3_000_000}
	e := mfEncoderOrSkip(t, cfg)
	defer e.Close()
	d := mfDecoderOrSkip(t, H264)
	defer d.Close()
	got := 0
	onFrame := func(f *VideoFrame) {
		if q := psnrY(testFrame(w, h, got), f); q < 25 {
			t.Errorf("frame %d: PSNR %.1f dB", got, q)
		}
		got++
		f.Release()
	}
	onPacket := func(p *EncodedPacket) {
		if err := d.Send(p.Packet); err != nil {
			t.Fatal(err)
		}
		p.Release()
		drain(t, d, onFrame)
	}
	for i := 0; i < frames; i++ {
		if err := e.Send(testFrame(w, h, i)); err != nil {
			t.Fatal(err)
		}
		drain(t, e, onPacket)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, e, onPacket)
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, d, onFrame)
	if got != frames {
		t.Errorf("decoded %d of %d frames", got, frames)
	}
}

func TestMFForceKeyframe(t *testing.T) {
	const w, h = 160, 120
	e := mfEncoderOrSkip(t, VideoConfig{Codec: H264, Width: w, Height: h, KeyframeEvery: 1000})
	defer e.Close()
	var keys []time.Duration
	keep := func(p *EncodedPacket) {
		if p.Keyframe {
			keys = append(keys, p.PTS)
		}
		p.Release()
	}
	for i := 0; i < 12; i++ {
		f := testFrame(w, h, i)
		f.ForceKeyframe = i == 7
		if err := e.Send(f); err != nil {
			t.Fatal(i, err)
		}
		drain(t, e, keep)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, e, keep)
	forced := testFrame(w, h, 7).PTS
	found := false
	for _, k := range keys {
		found = found || k == forced
	}
	if !found {
		t.Errorf("no keyframe at %v, keyframes at %v", forced, keys)
	}
}

func TestMFSteadyStateAllocs(t *testing.T) {
	t.Run("transcode", func(t *testing.T) {
		const w, h = 320, 240
		cfg := VideoConfig{Codec: H264, Width: w, Height: h}
		e := mfEncoderOrSkip(t, cfg)
		defer e.Close()
		d := mfDecoderOrSkip(t, H264)
		defer d.Close()
		src := testFrame(w, h, 0)
		out := 0
		onFrame := func(f *VideoFrame) { out++; f.Release() }
		onPacket := func(p *EncodedPacket) {
			if err := d.Send(p.Packet); err != nil {
				t.Fatal(err)
			}
			p.Release()
			drain(t, d, onFrame)
		}
		step := func() {
			src.PTS += time.Millisecond
			if err := e.Send(src); err != nil {
				t.Fatal(err)
			}
			drain(t, e, onPacket)
		}
		for n := 0; n < 30; n++ {
			step()
		}
		if out == 0 {
			t.Fatal("no frames decoded")
		}
		a := testing.AllocsPerRun(200, step)
		t.Logf("%.2f allocations per transcoded frame", a)
		if a != 0 {
			t.Errorf("%.2f allocations", a)
		}
	})
	t.Run("decode", func(t *testing.T) {
		d := mfDecoderOrSkip(t, H264)
		defer d.Close()
		pkts := mfDecode(mfTestH264)
		release := func(f *VideoFrame) { f.Release() }
		i, pts := 1, time.Duration(0)
		send := func(p []byte, key bool) {
			pts += time.Millisecond
			if err := d.Send(Packet{Data: p, PTS: pts, Keyframe: key}); err != nil {
				t.Fatal(err)
			}
			drain(t, d, release)
		}
		send(pkts[0], true)
		// Repeat the intra picture, which every decoder can take at any time.
		step := func() { i++; send(pkts[0], true) }
		for n := 0; n < 30; n++ {
			step()
		}
		a := testing.AllocsPerRun(200, step)
		t.Logf("%.2f allocations per decoded frame", a)
		if a != 0 {
			t.Errorf("%.2f allocations", a)
		}
	})
	t.Run("encode", func(t *testing.T) {
		const w, h = 320, 240
		e := mfEncoderOrSkip(t, VideoConfig{Codec: H264, Width: w, Height: h})
		defer e.Close()
		src := testFrame(w, h, 0)
		release := func(p *EncodedPacket) { p.Release() }
		step := func() {
			src.PTS += time.Millisecond
			if err := e.Send(src); err != nil {
				t.Fatal(err)
			}
			drain(t, e, release)
		}
		for n := 0; n < 30; n++ {
			step()
		}
		a := testing.AllocsPerRun(200, step)
		t.Logf("%.2f allocations per encoded frame", a)
		if a != 0 {
			t.Errorf("%.2f allocations", a)
		}
	})
}

// Fixtures: 64x48 test pattern, made with ffmpeg (x264 baseline, libvpx, libopus).

var mfTestH264 = []string{
	"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",
	"AAAAAUGaIFcb",
	"AAAAAUGaQJxnV8A=",
	"AAAAAUGaYf8bB9g+zYHmIj4AGQIFy0CRsCv2nQ5DgnosdkCAAKgqUhAEyMBoABMBwSUu42YGKJEIkYgjOcCCAaAYMYUPqUB1s2bMbqn2or4f0HMXE790VQJeEjjuIY63mCRXSZeDHn8HE5OcNy5+MGk6F9x+CDUdHlKFC8kMLO4iYNuJg2ULy+AbuGKmBAMCaIDEBA1GXd4/G7BqPA3MGwa8Z140jz9cN5wADBwaWGoLA0FgwOLS0eMsliD3QmDBgLzlZB2N2MP+KO9u7u7+FN27ve7u73ufBPu3d3d8nT5xCl0ohsaYe3Xq",
	"AAAAAUGagP/D8cxj7h5FZs1gL2CCBxb3WA9UQQC4OSEUBMAA8REJMIgsb7Girjs0mK91cbZjfnOrj+tc40pS5wAGcE/MZAn3pi5bjq7BJREkLIifdHhvN7EnA9QlG41w//8FXBK+BtUpETB1QhuuMkzn74IOCF4vsqAvDQxXgbHy2zm5yGBajRo1Qn/k6ExfOIXyioYhJUvz+K2E/f/jt6FeKvrhy7vzIZiAuNQf8A==",
	"AAAAAUGaoF/w9J6DcpgSSgkfYqdAQanjBQH4ZwocMFBdccMKkWigUbw14MMoYCwC3sIcAA94Iq0vVAGN2WBVWPBUi0Yr8MV9FoE/wQfQ6ig2mVTIGCwIkDQ++FRaOC9E834UsGeEgIB3QZQGqDyxDQqDgJ4xlQZfCxzKblNym5Tlo5M+WMbGPgmLe97nF9Sg+o7fDIrkIJCDzM2YO/E0YvUd6p7oV/Wb6p/n9Dcv38JHh7dvgA==",
}

var mfTestVP8 = []string{
	"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",
}

var mfTestOpus = []string{
	"eIF7xhF29HUAAAeOymnbu8xI3Ba9m9NYJ3iTvDdEXs8tlwBpgBGALAM4q0Vh/Ym3fg0XvyClK7XqK17Tp0jRGPILXAFPX6GWIa7e4FMA1mVlPx+my7l3v5diwRLPjcJ9/QCA//rGiLbeELhPxiNr6ofJBQ==",
	"eJ4eoef8lU1OqhgKyl0HZEQ0lEU+ZmbKRO0fDURr80/MpVVFQZ608/wTOGV57ZnwG3+9PJJz1e6P66t2MqX0oQw1vqu7YZm/vQ==",
	"eJnCX3Wc/Es6Ratafn17N3wCtEph8Iknh85Q++PDBMuvmWR8Fxozjeuuuhmb2+HXGbXIoXnWHLNo5SslmAxcvWQIEbsU0LsIDfn/2wU=",
	"eJnCX3Mt0i3t13/Ecp7k6ydXoxbzs7YsA/xJGYN4ZJ1BTGoW/IDqE8YDSb+lGUaRRozGHj2G2W5XqJVVkDid8wOIcZCDb4CKlkW5ZMO2TQ==",
	"eJnCX3Mt0jEzv3s0inEPd9XU6LWLeMVtG4+LaEvkaY4a3rjGH3j/UjY4L8PZT39GCiKWdrbcgt3eRZz7pgkox/JRmfaG+V+ZJD/wbAt7DsUM",
}
