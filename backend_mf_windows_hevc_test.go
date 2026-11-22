//go:build windows && (amd64 || arm64)

package gaal

import (
	"bytes"
	"errors"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Wine has no HEVC or VP9 transforms, so these tests drive the real backend code
// against a scripted asynchronous MFT. The script checks what the backend asks
// for. It says nothing about how a real decoder or encoder behaves.

func h265NAL(t int, body ...byte) []byte {
	return append([]byte{0, 0, 0, 1, byte(t << 1), 1}, body...)
}

func h265AU(nals ...[]byte) []byte { return bytes.Join(nals, nil) }

// h265Lengths rewrites Annex B with 4-byte start codes as length-prefixed.
func h265Lengths(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := 0; i < len(out); {
		next := bytes.Index(out[i+4:], []byte{0, 0, 0, 1})
		n := len(out) - i - 4
		if next >= 0 {
			n = next
		}
		out[i], out[i+1], out[i+2], out[i+3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		i += 4 + n
	}
	return out
}

var (
	testVPS    = h265NAL(32, 0xa1)
	testSPS    = h265NAL(33, 0xa2)
	testPPS    = h265NAL(34, 0xa3)
	testParams = h265AU(testVPS, testSPS, testPPS)
	testIDR    = h265NAL(19, 0x80, 7)
	testCRA    = h265NAL(21, 0x80, 7)
	testTrail  = h265NAL(1, 0x80, 8)
)

// vp9Bits builds a VP9 header from (value, width) pairs.
func vp9Bits(pairs ...int) []byte {
	var out []byte
	n := 0
	for i := 0; i < len(pairs); i += 2 {
		for b := pairs[i+1] - 1; b >= 0; b-- {
			if n%8 == 0 {
				out = append(out, 0)
			}
			out[n/8] |= byte(pairs[i]>>b&1) << (7 - n%8)
			n++
		}
	}
	return out
}

func vp9Key(w, h int) []byte {
	return vp9Bits(2, 2, 0, 2, 0, 1, 0, 1, 1, 1, 0, 1, 0x498342, 24, 2, 3, 0, 1, w-1, 16, h-1, 16, 0, 8)
}

func vp9Inter() []byte { return vp9Bits(2, 2, 0, 2, 0, 1, 1, 1, 1, 1, 0, 1, 0, 16) }

func TestVP9Header(t *testing.T) {
	f, ok := parseVP9Frame(vp9Key(1920, 1080))
	if !ok || !f.Key || !f.Show || f.Width != 1920 || f.Height != 1080 || f.BitDepth != 8 || f.SubX != 1 || f.SubY != 1 {
		t.Errorf("keyframe: %+v %v", f, ok)
	}
	if f, ok := parseVP9Frame(vp9Inter()); !ok || f.Key || f.Width != 0 {
		t.Errorf("inter frame: %+v %v", f, ok)
	}
	if f, ok := parseVP9Frame(vp9Bits(2, 2, 0, 2, 1, 1, 3, 3)); !ok || !f.ShowExisting {
		t.Errorf("show existing: %+v %v", f, ok)
	}
	p2 := vp9Bits(2, 2, 0, 1, 1, 1, 0, 1, 0, 1, 1, 1, 0, 1, 0x498342, 24, 0, 1, 2, 3, 1, 1, 63, 16, 47, 16)
	if f, ok := parseVP9Frame(p2); !ok || f.Profile != 2 || f.BitDepth != 10 || !f.FullRange || f.Width != 64 {
		t.Errorf("profile 2: %+v %v", f, ok)
	}
	for _, b := range [][]byte{nil, {0}, []byte("garbage"), vp9Key(64, 48)[:6], {0x82, 0x49, 0x83, 0x43, 0, 0, 0, 0, 0, 0}} {
		if _, ok := parseVP9Frame(b); ok {
			t.Errorf("accepted %x", b)
		}
	}
}

func TestMFH265Helpers(t *testing.T) {
	au := h265AU(testParams, testIDR)
	if m := mfH265Mask(au); m != 1<<32|1<<33|1<<34|1<<19 {
		t.Errorf("mask %x", m)
	}
	if !mfValidH265(au) || mfValidH265([]byte("this is not h265")) || mfValidH265([]byte{0, 0, 1, 0xff, 0xff, 0}) {
		t.Error("mfValidH265")
	}
	if got := mfExtractNALs(au, mfIsH265Param); !bytes.Equal(got, testParams) {
		t.Errorf("extract %x", got)
	}
	if got := mfH265ParamSets(h265AU(testParams, h265NAL(39, 1))); !bytes.Equal(got, testParams) {
		t.Errorf("Annex B header %x", got)
	}
	hvcC := make([]byte, 23)
	hvcC[0], hvcC[22] = 1, 4
	for _, n := range [][]byte{testVPS, testSPS, testPPS, h265NAL(39, 1)} {
		nal := n[4:]
		hvcC = append(hvcC, nal[0]>>1, 0, 1, byte(len(nal)>>8), byte(len(nal)))
		hvcC = append(hvcC, nal...)
	}
	if got := mfH265ParamSets(hvcC); !bytes.Equal(got, testParams) {
		t.Errorf("hvcC %x", got)
	}
	if got := mfH265ParamSets(hvcC[:len(hvcC)-1]); got != nil {
		t.Errorf("truncated hvcC %x", got)
	}
}

func TestMFSpecsAndBackends(t *testing.T) {
	for _, tc := range []struct {
		c   Codec
		d   Direction
		sub mfGUID
	}{{H265, Decode, fmtHEVC}, {VP9, Decode, fmtVP90}} {
		s, ok := mfSpecFor(tc.c, tc.d)
		if !ok || s.in.sub != tc.sub || s.category != &catVideoDec {
			t.Errorf("%v %v: %+v %v", tc.c, tc.d, s, ok)
		}
	}
	s, ok := mfSpecFor(H265, Encode)
	if !ok || s.out.sub != fmtHEVC || s.in.sub != fmtNV12 || s.category != &catVideoEnc {
		t.Errorf("h265 encode: %+v %v", s, ok)
	}
	if _, ok := mfSpecFor(VP9, Encode); ok {
		t.Error("VP9 encode must stay with libvpx")
	}
	if fmtHEVC.d1 != 0x43564548 || fmtVP90.d1 != 0x30395056 {
		t.Errorf("fourcc %x %x", fmtHEVC.d1, fmtVP90.d1)
	}
	seen := map[BackendInfo]bool{}
	for _, b := range mfBackends() {
		seen[b.BackendInfo] = true
	}
	for _, hw := range []bool{true, false} {
		for _, i := range []BackendInfo{{mfBackendName, H265, Decode, hw}, {mfBackendName, H265, Encode, hw}, {mfBackendName, VP9, Decode, hw}} {
			if !seen[i] {
				t.Errorf("missing backend %+v", i)
			}
		}
	}
}

func TestMFH265EncoderOutput(t *testing.T) {
	e := &mfEncoder{m: &mfTransform{api: mfTestAPI(t)}, cfg: VideoConfig{Codec: H265}, params: testParams}
	defer e.m.close()
	for _, tc := range []struct {
		name string
		in   []byte
		want []byte
		key  bool
	}{
		{"IDR without parameter sets", testIDR, h265AU(testParams, testIDR), true},
		{"CRA without parameter sets", testCRA, h265AU(testParams, testCRA), true},
		{"keyframe with parameter sets", h265AU(testParams, testIDR), h265AU(testParams, testIDR), true},
		{"length-prefixed IDR", h265Lengths(testIDR), h265AU(testParams, testIDR), true},
		{"length-prefixed P frame", h265Lengths(testTrail), testTrail, false},
		{"P frame", testTrail, testTrail, false},
	} {
		p := mfFeed(t, e, tc.in)
		if p.Keyframe != tc.key || !bytes.Equal(p.Data, tc.want) {
			t.Errorf("%s: key=%v %x", tc.name, p.Keyframe, p.Data)
		}
		p.Release()
	}
	e.params = nil
	mfFeed(t, e, h265AU(testParams, testIDR)).Release()
	p := mfFeed(t, e, testIDR)
	if !bytes.Equal(p.Data, h265AU(testParams, testIDR)) {
		t.Errorf("cached parameter sets: %x", p.Data)
	}
	p.Release()
}

// --- Scripted transform ---

type scriptState struct {
	inSub, outSub mfGUID
	inSize        uint64
	profile       uint32
	testOnly      int
	codecValues   map[mfGUID]uint64
	outType       uintptr
	output        func(n int) []byte
	produced      int
}

var (
	script     *scriptState
	scriptVtbl [26]uintptr
	codecVtbl  [10]uintptr
	codecObj   = &codecVtbl
	scriptOnce bool
)

func scriptInstall() {
	fakeInstall()
	if scriptOnce {
		return
	}
	scriptOnce = true
	scriptVtbl = fakeVtbl
	set := func(slot int, fn func(this, a, b, c, d uintptr) uintptr) { scriptVtbl[slot] = syscall.NewCallback(fn) }
	store := func(dst, v uintptr) { *(*uintptr)(mfPtr(dst)) = v }
	set(slotQI, func(this, iid, out, _, _ uintptr) uintptr {
		switch *(*mfGUID)(mfPtr(iid)) {
		case iidEventGen:
			fake.c.addRef(fake.queue)
			store(out, fake.queue)
		case iidTransform:
			store(out, this)
		case iidCodecAPI:
			store(out, uintptr(unsafe.Pointer(&codecObj)))
		default:
			store(out, 0)
			return 0x80004002
		}
		return 0
	})
	set(slotOutStreamInfo, func(_, _, out, _, _ uintptr) uintptr {
		*(*mftOutputInfo)(mfPtr(out)) = mftOutputInfo{size: 1 << 16}
		return 0
	})
	set(slotSetInputType, func(_, _, mt, flags, _ uintptr) uintptr {
		if flags&mftSetTestOnly != 0 {
			script.testOnly++
			return 0
		}
		script.inSub, _ = fake.c.getGUID(mt, &attrSubtype)
		script.inSize, _ = fake.c.getU64(mt, &attrFrameSize)
		return 0
	})
	set(slotSetOutputType, func(_, _, mt, _, _ uintptr) uintptr {
		script.outSub, _ = fake.c.getGUID(mt, &attrSubtype)
		script.profile, _ = fake.c.getU32(mt, &attrProfile)
		return 0
	})
	set(slotOutAvailable, func(_, _, index, out, _ uintptr) uintptr {
		if index != 0 || script.outType == 0 {
			return hrNoMoreTypes
		}
		fake.c.addRef(script.outType)
		store(out, script.outType)
		return 0
	})
	set(slotProcessOutput, func(_, _, _, buffers, _ uintptr) uintptr {
		f := fake
		if f.pending == 0 {
			return hrNeedMoreInput
		}
		f.pending--
		out := (*mftOutputBuffer)(mfPtr(buffers))
		data := script.output(script.produced)
		script.produced++
		c := f.c
		c.p = 0
		c.m1(out.sample, slotSampleContiguous, uintptr(unsafe.Pointer(&c.p)))
		buf := c.p
		c.p = 0
		c.m3(buf, slotBufLock, uintptr(unsafe.Pointer(&c.p)), 0, 0)
		copy(cMem(c.p, len(data)), data)
		c.m0(buf, slotBufUnlock)
		c.m1(buf, slotBufSetLen, uintptr(len(data)))
		c.release(buf)
		c.m1(out.sample, slotSampleSetTime, uintptr(f.hns))
		return 0
	})
	ok := syscall.NewCallback(func(_, _, _, _, _ uintptr) uintptr { return 1 })
	for i := range codecVtbl {
		codecVtbl[i] = syscall.NewCallback(func(_, _, _, _, _ uintptr) uintptr { return 0x80004001 })
	}
	codecVtbl[slotAddRef], codecVtbl[slotRelease] = ok, ok
	codecVtbl[slotCodecSetValue] = syscall.NewCallback(func(_, key, v, _, _ uintptr) uintptr {
		script.codecValues[*(*mfGUID)(mfPtr(key))] = (*[3]uint64)(mfPtr(v))[1]
		return 0
	})
}

// newScriptTransform returns a bound transform that has not been started.
func newScriptTransform(t *testing.T, sink mfSink) *mfTransform {
	t.Helper()
	api := mfTestAPI(t)
	scriptInstall()
	dll, err := syscall.LoadDLL("mfplat.dll")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMFT{vtbl: &scriptVtbl, c: newMFCall()}
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
	script = &scriptState{codecValues: map[mfGUID]uint64{}}
	m := &mfTransform{api: api, sink: sink, hw: true, timeout: time.Second, xf: uintptr(unsafe.Pointer(f))}
	if err := m.bind(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.c.m0(f.queue, 9) // IMFMediaEventQueue::Shutdown
		m.close()
		f.c.release(script.outType)
		f.c.release(f.queue)
		f.c.release(f.attrs)
	})
	return m
}

func TestMFScriptedH265Encoder(t *testing.T) {
	const w, h, frames, forced = 64, 48, 4, 2
	cfg := VideoConfig{Codec: H265, Width: w, Height: h, FPS: 30, Bitrate: 1_000_000, KeyframeEvery: 60}
	e := &mfEncoder{cfg: cfg, info: BackendInfo{mfBackendName, H265, Encode, true}, dur: time.Second / 30}
	m := newScriptTransform(t, e)
	if err := e.setup(m); err != nil {
		t.Fatal(err)
	}
	if script.outSub != fmtHEVC || script.inSub != fmtNV12 || script.profile != mfProfileHEVC {
		t.Errorf("types: output %v input %v profile %d", script.outSub, script.inSub, script.profile)
	}
	if v, ok := fake.c.getU32(fake.attrs, &attrLowLatency); !ok || v != 1 {
		t.Error("MF_LOW_LATENCY is not set")
	}
	v := script.codecValues
	if v[attrLowLatency] != 0xffff || v[apiBFrames] != 0 || v[apiGOPSize] != 60 || v[apiMeanBitrate] != 1_000_000 {
		t.Errorf("codec properties %v", v)
	}
	if _, ok := v[apiBFrames]; !ok {
		t.Error("B-frame count was not set")
	}
	script.output = func(n int) []byte {
		switch n {
		case 0:
			return h265Lengths(h265AU(testParams, testIDR)) // parameter sets in band
		case forced:
			return h265Lengths(testIDR) // this one needs them added
		}
		return h265Lengths(testTrail)
	}
	var got []*EncodedPacket
	for i := 0; i < frames; i++ {
		f := testFrame(w, h, i)
		f.ForceKeyframe = i == forced
		if err := e.Send(f); err != nil {
			t.Fatal(err)
		}
		if i == forced && script.codecValues[apiForceKey] != 1 {
			t.Error("ForceKeyFrame was not requested")
		}
		p, err := ReceiveWait(VideoEncoder(e), time.Second)
		if err != nil {
			t.Fatal(i, err)
		}
		got = append(got, p)
	}
	for i, p := range got {
		want, key := testTrail, false
		if i == 0 || i == forced {
			want, key = h265AU(testParams, testIDR), true
		}
		if p.Keyframe != key || !bytes.Equal(p.Data, want) {
			t.Errorf("packet %d: key=%v %x", i, p.Keyframe, p.Data)
		}
		if p.PTS != testFrame(w, h, i).PTS {
			t.Errorf("packet %d: pts %v", i, p.PTS)
		}
		p.Release()
	}
}

// scriptDecoder runs one packet through a scripted decoder.
func scriptDecoder(t *testing.T, codec Codec, sub *mfGUID, first []byte) *mfVideoDecoder {
	t.Helper()
	const w, h = 64, 48
	d := &mfVideoDecoder{info: BackendInfo{mfBackendName, codec, Decode, true}, sub: sub, sized: codec == VP9}
	m := newScriptTransform(t, d)
	d.m = m
	mt, err := m.api.newMediaType(fake.c, &mediaVideo, &fmtNV12)
	if err != nil {
		t.Fatal(err)
	}
	fake.c.setU64(mt, &attrFrameSize, mfPair(w, h))
	script.outType = mt
	picture := make([]byte, w*h*3/2)
	for i := range picture {
		picture[i] = 77
	}
	script.output = func(int) []byte { return picture }
	if err := d.setup(m); err != nil {
		t.Fatal(err)
	}
	const pts = 5 * time.Millisecond
	if err := d.Send(Packet{Data: first, PTS: pts, Keyframe: true}); err != nil {
		t.Fatal(err)
	}
	if script.inSub != *sub {
		t.Errorf("input type %v", script.inSub)
	}
	f, err := ReceiveWait(VideoDecoder(d), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != w || f.Height != h || f.Format != NV12 || f.PTS != pts || f.Y[0] != 77 {
		t.Errorf("frame %dx%d format %d pts %v", f.Width, f.Height, f.Format, f.PTS)
	}
	f.Release()
	return d
}

func TestMFScriptedH265Decoder(t *testing.T) {
	d := scriptDecoder(t, H265, &fmtHEVC, h265AU(testParams, testIDR))
	if script.inSize != 0 {
		t.Errorf("input size %x, H.265 takes it from the stream", script.inSize)
	}
	if err := d.Send(Packet{Data: []byte("this is certainly not video")}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: %v", err)
	}
	if err := d.Send(Packet{Data: testTrail, PTS: time.Second}); err != nil {
		t.Fatal(err)
	}
	f, err := ReceiveWait(VideoDecoder(d), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.Release()
}

func TestMFScriptedVP9Decoder(t *testing.T) {
	d := scriptDecoder(t, VP9, &fmtVP90, vp9Key(64, 48))
	if script.inSize != mfPair(64, 48) {
		t.Errorf("input size %x", script.inSize)
	}
	if err := d.Send(Packet{Data: vp9Inter(), PTS: time.Second}); err != nil {
		t.Fatal(err)
	}
	f, err := ReceiveWait(VideoDecoder(d), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.Release()
	if err := d.Send(Packet{Data: []byte("garbage")}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: %v", err)
	}
}

func TestMFVP9NeedsKeyframe(t *testing.T) {
	d := &mfVideoDecoder{info: BackendInfo{mfBackendName, VP9, Decode, true}, sub: &fmtVP90, sized: true, m: &mfTransform{}}
	if err := d.prepare(Packet{Data: vp9Inter()}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("inter frame first: %v", err)
	}
}
