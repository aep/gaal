//go:build windows && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	// mfTimeout bounds every wait on an MFT. Hardware can take a while to
	// produce its first event, but an MFT silent for this long has failed.
	mfTimeout = 5 * time.Second
	// mfMaxInputs bounds the input samples an MFT can hold at once.
	mfMaxInputs  = 32
	mfMaxChanges = 8
	// mfMaxOutputs bounds the samples one pullAll may collect. A transform
	// that keeps returning output without new input is broken, and looping on
	// it would hang the caller.
	mfMaxOutputs  = 256
	mfPTSRingSize = 128
)

var errMFDead = errors.New("mediafoundation: transform could not be reopened")

// mfSink receives what a transform produces.
type mfSink interface {
	onSample(sample uintptr) error
	onFormat() error // the output type must be negotiated again
}

type mfInput struct {
	sample, buf uintptr
	size        int
}

type mfPTS struct {
	hns int64
	ns  time.Duration
}

// mfTransform drives one IMFTransform, synchronous or asynchronous.
type mfTransform struct {
	c        mfCall
	api      *mfAPI
	sink     mfSink
	activate uintptr
	xf       uintptr
	attrs    uintptr
	gen      uintptr // IMFMediaEventGenerator, async only
	codec    uintptr // ICodecAPI, may be 0

	name    string // MFT_FRIENDLY_NAME_Attribute, if the transform has one
	untimed bool   // the next commit produces no output: note no timestamp
	lastLen uint32 // size and time of the last output sample, for errors
	lastHNS int64
	hw      bool
	async   bool
	started bool
	timeout time.Duration

	inInfo  mftInputInfo
	outInfo mftOutputInfo

	inputs    []mfInput
	outSample uintptr
	outBuf    uintptr
	outSize   int

	needInput int
	drained   bool
	err       error // sticky failure reported by an async event

	locked   uintptr // buffer locked by lockSample
	locked2D uintptr

	pts     [mfPTSRingSize]mfPTS
	ptsNext uint64
	ptsOut  uint64
	// orderedPTS says output order equals input order.
	orderedPTS bool
	// oneToOne says each input gives exactly one output sample, in order, so
	// outputs take input timestamps by count. The sample times are not used:
	// the FLAC decoder stamps output with the time of a later input.
	oneToOne bool
}

// mfOpen activates the best MFT for spec of the wanted kind that setup accepts.
func mfOpen(spec mfSpec, hw bool, sink mfSink, setup func(*mfTransform) error) (*mfTransform, error) {
	api, err := loadMF()
	if err != nil {
		return nil, err
	}
	c := newMFCall()
	list, err := api.enumerate(c, spec)
	if err != nil {
		return nil, err
	}
	var result *mfTransform
	errs := []error{}
	for _, a := range list {
		if result != nil || mfIsHardware(c, a) != hw {
			c.release(a)
			continue
		}
		m := &mfTransform{api: api, sink: sink, hw: hw, activate: a, timeout: mfTimeout}
		if err := m.attach(); err == nil {
			err = setup(m)
			if err == nil {
				result = m
				continue
			}
			errs = append(errs, err)
		} else {
			errs = append(errs, err)
		}
		m.close() // releases a
	}
	if result != nil {
		return result, nil
	}
	if len(errs) == 0 {
		return nil, errMFNoTransform
	}
	return nil, errors.Join(errs...)
}

func (m *mfTransform) attach() error {
	c := &m.c
	var name [128]uint16
	c.u = 0
	if !mfFailed(c.m4(m.activate, slotGetString, gp(&attrFriendlyName), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), uintptr(unsafe.Pointer(&c.u)))) {
		m.name = syscall.UTF16ToString(name[:])
	}
	c.p = 0
	if hr := c.m2(m.activate, slotActivateObject, gp(&iidTransform), uintptr(unsafe.Pointer(&c.p))); mfFailed(hr) {
		return mfError("activate transform", hr)
	}
	m.xf = c.p
	return m.bind()
}

// bind prepares m.xf: it unlocks an asynchronous MFT and requests low latency.
func (m *mfTransform) bind() error {
	c := &m.c
	c.p = 0
	if !mfFailed(c.m1(m.xf, slotGetAttributes, uintptr(unsafe.Pointer(&c.p)))) {
		m.attrs = c.p
	}
	if m.attrs != 0 {
		if v, ok := c.getU32(m.attrs, &attrAsync); ok && v != 0 {
			m.async = true
			if hr := c.setU32(m.attrs, &attrAsyncOK, 1); mfFailed(hr) {
				return mfError("unlock async transform", hr)
			}
		}
		// Stored as VT_UI4, which is what decoders read. Encoders also get the
		// VT_BOOL form through ICodecAPI.
		c.setU32(m.attrs, &attrLowLatency, 1)
	}
	if m.async {
		gen, hr := c.qi(m.xf, &iidEventGen)
		if mfFailed(hr) {
			return mfError("query event generator", hr)
		}
		m.gen = gen
	}
	m.codec, _ = c.qi(m.xf, &iidCodecAPI)
	return nil
}

// refreshInfo reads the stream requirements, which depend on the media types.
func (m *mfTransform) refreshInfo() error {
	c := &m.c
	if hr := c.m2(m.xf, slotInStreamInfo, 0, uintptr(unsafe.Pointer(&c.inInfo))); mfFailed(hr) {
		return mfError("input stream info", hr)
	}
	if hr := c.m2(m.xf, slotOutStreamInfo, 0, uintptr(unsafe.Pointer(&c.outInfo))); mfFailed(hr) {
		return mfError("output stream info", hr)
	}
	m.inInfo, m.outInfo = c.inInfo, c.outInfo
	return nil
}

func (m *mfTransform) message(msg uint32) error {
	if hr := m.c.m2(m.xf, slotProcessMessage, uintptr(msg), 0); mfFailed(hr) {
		return mfError("process message", hr)
	}
	return nil
}

// start begins streaming. The media types must be set.
func (m *mfTransform) start() error {
	if err := m.refreshInfo(); err != nil {
		return err
	}
	if err := m.message(mftMsgBegin); err != nil {
		return err
	}
	if err := m.message(mftMsgStartStrm); err != nil {
		return err
	}
	m.started = true
	return nil
}

func (m *mfTransform) close() {
	c := &m.c
	if m.xf != 0 {
		if m.started {
			c.m2(m.xf, slotProcessMessage, mftMsgFlush, 0)
			c.m2(m.xf, slotProcessMessage, mftMsgEnd, 0)
		}
		if m.async {
			if s, _ := c.qi(m.xf, &iidMFShutdown); s != 0 {
				c.m0(s, slotShutdown)
				c.release(s)
			}
		}
	}
	for _, in := range m.inputs {
		c.release(in.buf)
		c.release(in.sample)
	}
	m.inputs = nil
	c.release(m.outBuf)
	c.release(m.outSample)
	c.release(m.gen)
	c.release(m.codec)
	c.release(m.attrs)
	c.release(m.xf)
	if m.activate != 0 {
		c.m0(m.activate, slotShutdownObject)
		c.release(m.activate)
	}
	m.outBuf, m.outSample, m.gen, m.codec, m.attrs, m.xf, m.activate = 0, 0, 0, 0, 0, 0, 0
	m.started = false
}

func (m *mfTransform) newBuffer(size, align int) (uintptr, error) {
	if align < 16 {
		align = 16
	}
	c := &m.c
	c.p = 0
	c.a[0], c.a[1], c.a[2] = uintptr(size), uintptr(align-1), uintptr(unsafe.Pointer(&c.p))
	if hr := uint32(c.raw(m.api.createAligned, 3)); mfFailed(hr) {
		return 0, mfError("create buffer", hr)
	}
	return c.p, nil
}

func (m *mfTransform) newSample() (uintptr, error) {
	c := &m.c
	c.p = 0
	c.a[0] = uintptr(unsafe.Pointer(&c.p))
	if hr := uint32(c.raw(m.api.createSample, 1)); mfFailed(hr) {
		return 0, mfError("create sample", hr)
	}
	return c.p, nil
}

// setBuffer makes buf the only buffer of sample.
func (m *mfTransform) setBuffer(sample, buf uintptr) error {
	c := &m.c
	c.m0(sample, slotSampleRemoveAll)
	if hr := c.m1(sample, slotSampleAddBuffer, buf); mfFailed(hr) {
		return mfError("add buffer", hr)
	}
	return nil
}

// freeInput finds a sample the MFT no longer references. An MFT may keep input
// samples after ProcessInput, so a sample is only reused once the reference
// counts show that the MFT dropped it. The sample holds one reference to its
// buffer and the pool one to each.
func (m *mfTransform) freeInput() *mfInput {
	c := &m.c
	for i := range m.inputs {
		in := &m.inputs[i]
		if c.refs(in.sample) == 1 && c.refs(in.buf) == 2 {
			return in
		}
	}
	return nil
}

// begin returns a locked input buffer of n bytes. Follow it with commit.
func (m *mfTransform) begin(n int) (*mfInput, []byte, error) {
	if m.xf == 0 && m.api == nil {
		return nil, nil, errMFDead
	}
	if m.inputs == nil {
		m.inputs = make([]mfInput, 0, mfMaxInputs)
	}
	if size := int(m.inInfo.size); n < size {
		n = size
	}
	in := m.freeInput()
	if in == nil && len(m.inputs) < mfMaxInputs {
		s, err := m.newSample()
		if err != nil {
			return nil, nil, err
		}
		m.inputs = append(m.inputs, mfInput{sample: s})
		in = &m.inputs[len(m.inputs)-1]
	}
	if in == nil {
		deadline := time.Now().Add(m.timeout)
		for spins := 0; in == nil; spins++ {
			if err := m.service(); err != nil {
				return nil, nil, err
			}
			if in = m.freeInput(); in != nil {
				break
			}
			if err := m.idle(spins, deadline, "a free input sample"); err != nil {
				return nil, nil, err
			}
		}
	}
	c := &m.c
	if in.size < n {
		size := n + n/4
		buf, err := m.newBuffer(size, int(m.inInfo.alignment))
		if err != nil {
			return nil, nil, err
		}
		if err := m.setBuffer(in.sample, buf); err != nil {
			c.release(buf)
			return nil, nil, err
		}
		c.release(in.buf)
		in.buf, in.size = buf, size
	}
	c.p = 0
	if hr := c.m3(in.buf, slotBufLock, uintptr(unsafe.Pointer(&c.p)), 0, 0); mfFailed(hr) {
		return nil, nil, mfError("lock input", hr)
	}
	return in, cMem(c.p, in.size), nil
}

func mfHNS(d time.Duration) int64 { return int64(d) / 100 }

// notePTS remembers the exact timestamp, as MF time has 100 ns resolution.
func (m *mfTransform) notePTS(ns time.Duration) int64 {
	hns := mfHNS(ns)
	m.pts[m.ptsNext%mfPTSRingSize] = mfPTS{hns, ns}
	m.ptsNext++
	return hns
}

// samplePTS returns the original timestamp of an output sample. It matches the
// sample time against recent inputs. With orderedPTS set, output follows input
// order, so a sample whose time matches nothing takes the oldest pending input:
// some encoders offset their timestamps.
func (m *mfTransform) samplePTS(sample uintptr) time.Duration {
	c := &m.c
	oldest := uint64(0)
	if m.ptsNext > mfPTSRingSize {
		oldest = m.ptsNext - mfPTSRingSize
	}
	m.ptsOut = max(m.ptsOut, oldest)
	timed := !mfFailed(c.m1(sample, slotSampleGetTime, uintptr(unsafe.Pointer(&c.i64))))
	hns := c.i64
	if timed && !m.oneToOne {
		for s := m.ptsOut; s < m.ptsNext; s++ {
			if e := &m.pts[s%mfPTSRingSize]; e.hns == hns {
				m.ptsOut = s + 1
				return e.ns
			}
		}
		if !m.orderedPTS {
			for s := oldest; s < m.ptsOut; s++ { // reordered output
				if e := &m.pts[s%mfPTSRingSize]; e.hns == hns {
					return e.ns
				}
			}
			return time.Duration(hns) * 100
		}
	}
	if m.ptsOut < m.ptsNext {
		m.ptsOut++
		return m.pts[(m.ptsOut-1)%mfPTSRingSize].ns
	}
	if timed {
		return time.Duration(hns) * 100
	}
	return 0
}

// commit unlocks the buffer from begin and submits n bytes of it.
func (m *mfTransform) commit(in *mfInput, n int, pts, duration time.Duration, key bool) error {
	c := &m.c
	c.m0(in.buf, slotBufUnlock)
	if hr := c.m1(in.buf, slotBufSetLen, uintptr(n)); mfFailed(hr) {
		return mfError("set input length", hr)
	}
	hns := mfHNS(pts)
	if m.untimed {
		m.untimed = false // input without output of its own, such as a header
	} else {
		hns = m.notePTS(pts)
	}
	c.m1(in.sample, slotSampleSetTime, uintptr(hns))
	if duration > 0 {
		c.m1(in.sample, slotSampleSetDuration, uintptr(mfHNS(duration)))
	}
	v := uint32(0)
	if key {
		v = 1
	}
	c.setU32(in.sample, &attrCleanPoint, v)
	return m.processInput(in.sample)
}

func (m *mfTransform) processInput(sample uintptr) error {
	c := &m.c
	var deadline time.Time
	for spins := 0; ; spins++ {
		if err := m.service(); err != nil {
			return err
		}
		if !m.async || m.needInput > 0 {
			hr := c.m3(m.xf, slotProcessInput, 0, sample, 0)
			if !mfFailed(hr) {
				if m.async {
					m.needInput--
					return nil
				}
				return m.pullAll()
			}
			if hr != hrNotAccepting {
				if mfIsCorrupt(hr) && !m.async {
					c.m2(m.xf, slotProcessMessage, mftMsgFlush, 0)
				}
				return mfDataError("process input", hr)
			}
			if m.async {
				m.needInput = 0
			} else if err := m.pullAll(); err != nil {
				return err
			}
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(m.timeout)
		}
		if err := m.idle(spins, deadline, "the transform to accept input"); err != nil {
			return err
		}
	}
}

// idle pauses between polls and enforces the deadline.
func (m *mfTransform) idle(spins int, deadline time.Time, what string) error {
	if spins < 32 {
		runtime.Gosched()
		return nil
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("mediafoundation: timed out waiting for %s", what)
	}
	time.Sleep(200 * time.Microsecond)
	return nil
}

// service handles every pending event of an asynchronous MFT without blocking.
// No IMFAsyncCallback is involved, so Go is never called from a foreign thread.
func (m *mfTransform) service() error {
	if m.err != nil {
		return m.err
	}
	if !m.async {
		return nil
	}
	c := &m.c
	for {
		c.p = 0
		hr := c.m2(m.gen, slotGetEvent, mfEventNoWait, uintptr(unsafe.Pointer(&c.p)))
		if hr == hrNoEvents {
			return nil
		}
		if mfFailed(hr) {
			return m.fail(mfError("get event", hr))
		}
		ev := c.p
		c.u, c.u2 = 0, 0
		hr = c.m1(ev, slotEventType, uintptr(unsafe.Pointer(&c.u)))
		c.m1(ev, slotEventStatus, uintptr(unsafe.Pointer(&c.u2)))
		kind, status := c.u, c.u2
		c.release(ev)
		if mfFailed(hr) {
			return m.fail(mfError("event type", hr))
		}
		if mfFailed(status) {
			return m.fail(mfDataError("transform event", status))
		}
		switch kind {
		case meNeedInput:
			m.needInput++
		case meHaveOutput:
			if _, err := m.pull(); err != nil {
				return m.fail(err)
			}
		case meDrainComplete:
			m.drained = true
		case meError:
			return m.fail(mfError("transform error event", status))
		}
	}
}

// fail records a failure of the MFT. Damaged input leaves the stream usable.
func (m *mfTransform) fail(err error) error {
	if !errors.Is(err, ErrCorrupt) {
		m.err = err
	}
	return err
}

func (m *mfTransform) ensureOutput() error {
	if m.outInfo.flags&mftOutProvides != 0 {
		return nil
	}
	size := int(m.outInfo.size)
	if size == 0 {
		if m.outInfo.flags&mftOutCanProvide != 0 {
			return nil
		}
		size = 1 << 16
	}
	c := &m.c
	if m.outSample == 0 {
		s, err := m.newSample()
		if err != nil {
			return err
		}
		m.outSample = s
	}
	if m.outSize < size {
		buf, err := m.newBuffer(size, int(m.outInfo.alignment))
		if err != nil {
			return err
		}
		if err := m.setBuffer(m.outSample, buf); err != nil {
			c.release(buf)
			return err
		}
		c.release(m.outBuf)
		m.outBuf, m.outSize = buf, size
	}
	c.m1(m.outBuf, slotBufSetLen, 0)
	return nil
}

// pull calls ProcessOutput once and hands any sample to the sink.
func (m *mfTransform) pull() (bool, error) {
	c := &m.c
	for changes := 0; changes < mfMaxChanges; changes++ {
		if err := m.ensureOutput(); err != nil {
			return false, err
		}
		own := uintptr(0)
		if m.outBuf != 0 && m.outInfo.flags&mftOutProvides == 0 {
			own = m.outSample
		}
		c.output = mftOutputBuffer{sample: own}
		c.u = 0
		hr := c.m4(m.xf, slotProcessOutput, 0, 1, uintptr(unsafe.Pointer(&c.output)), uintptr(unsafe.Pointer(&c.u)))
		got, events := c.output.sample, c.output.events
		if events != 0 {
			c.release(events)
		}
		switch {
		case hr == hrNeedMoreInput:
			m.dropForeign(got, own)
			return false, nil
		case hr == hrBufferTooSmall && own != 0:
			m.outInfo.size = uint32(m.outSize * 2)
			continue
		case hr == hrStreamChange, hr == hrTypeNotSet:
			m.dropForeign(got, own)
			if err := m.sink.onFormat(); err != nil {
				return false, err
			}
			if err := m.refreshInfo(); err != nil {
				return false, err
			}
			if hr == hrTypeNotSet && !m.outputTypeSet() {
				return false, nil
			}
			continue
		case mfFailed(hr):
			m.dropForeign(got, own)
			return false, mfDataError("process output", hr)
		}
		if got == 0 {
			return false, nil
		}
		c.u = 0
		c.m1(got, slotSampleTotalLength, uintptr(unsafe.Pointer(&c.u)))
		m.lastLen, m.lastHNS = c.u, -1
		if !mfFailed(c.m1(got, slotSampleGetTime, uintptr(unsafe.Pointer(&c.i64)))) {
			m.lastHNS = c.i64
		}
		err := m.sink.onSample(got)
		m.emptySample(got)
		m.dropForeign(got, own)
		return err == nil, err
	}
	return false, errors.New("mediafoundation: output format keeps changing")
}

// emptySample sets the length of every buffer in a read sample to zero. The
// ALAC decoder writes into a buffer of its own and appends to it, so without
// this each output would repeat all the ones before.
func (m *mfTransform) emptySample(sample uintptr) {
	c := &m.c
	c.u = 0
	if mfFailed(c.m1(sample, slotSampleBufferCount, uintptr(unsafe.Pointer(&c.u)))) {
		return
	}
	for i, n := uintptr(0), uintptr(c.u); i < n; i++ {
		c.p = 0
		if !mfFailed(c.m2(sample, slotSampleBufferAt, i, uintptr(unsafe.Pointer(&c.p)))) && c.p != 0 {
			buf := c.p
			c.m1(buf, slotBufSetLen, 0)
			c.release(buf)
		}
	}
}

func (m *mfTransform) dropForeign(got, own uintptr) {
	if got != 0 && got != own {
		m.c.release(got)
	}
}

func (m *mfTransform) outputTypeSet() bool {
	c := &m.c
	c.p = 0
	if mfFailed(c.m2(m.xf, slotOutCurrent, 0, uintptr(unsafe.Pointer(&c.p)))) {
		return false
	}
	c.release(c.p)
	return true
}

func (m *mfTransform) pullAll() error {
	if m.async {
		return nil
	}
	for n := 0; ; n++ {
		if n == mfMaxOutputs {
			return m.fail(fmt.Errorf("mediafoundation: transform returned %d samples without new input, the last of %d bytes at %d hns", n, m.lastLen, m.lastHNS))
		}
		got, err := m.pull()
		if err != nil || !got {
			return err
		}
	}
}

// drain ends the input and collects all remaining output, within the timeout.
func (m *mfTransform) drain() error {
	if m.xf == 0 {
		return errMFDead
	}
	if err := m.service(); err != nil {
		return err
	}
	if err := m.message(mftMsgEndOfStrm); err != nil {
		return err
	}
	m.drained = false
	if err := m.message(mftMsgDrain); err != nil {
		return err
	}
	if !m.async {
		return m.pullAll()
	}
	deadline := time.Now().Add(m.timeout)
	for spins := 0; ; spins++ {
		if err := m.service(); err != nil {
			return err
		}
		if m.drained {
			return nil
		}
		if err := m.idle(spins, deadline, "the transform to drain"); err != nil {
			return err
		}
	}
}

// lockSample maps the pixels or bytes of an output sample. It returns the
// address, the pitch when the buffer reports one, and the usable length.
func (m *mfTransform) lockSample(sample uintptr) (addr uintptr, pitch, size int, err error) {
	c := &m.c
	// A draining encoder can return a sample without buffers, which
	// ConvertToContiguousBuffer rejects with E_UNEXPECTED.
	c.u = 0
	if hr := c.m1(sample, slotSampleBufferCount, uintptr(unsafe.Pointer(&c.u))); !mfFailed(hr) && c.u == 0 {
		return 0, 0, 0, nil
	}
	c.p = 0
	if hr := c.m1(sample, slotSampleContiguous, uintptr(unsafe.Pointer(&c.p))); mfFailed(hr) {
		return 0, 0, 0, mfError("contiguous buffer", hr)
	}
	m.locked = c.p
	c.u = 0
	c.m1(m.locked, slotBufGetLen, uintptr(unsafe.Pointer(&c.u)))
	size = int(c.u)
	if q, _ := c.qi(m.locked, &iid2DBuffer); q != 0 {
		c.p, c.i = 0, 0
		if !mfFailed(c.m2(q, slot2DLock, uintptr(unsafe.Pointer(&c.p)), uintptr(unsafe.Pointer(&c.i)))) {
			if c.i > 0 {
				m.locked2D = q
				return c.p, int(c.i), size, nil
			}
			c.m0(q, slot2DUnlock) // bottom-up, use the plain view
		}
		c.release(q)
	}
	c.p, c.u2 = 0, 0
	if hr := c.m3(m.locked, slotBufLock, uintptr(unsafe.Pointer(&c.p)), 0, uintptr(unsafe.Pointer(&c.u2))); mfFailed(hr) {
		c.release(m.locked)
		m.locked = 0
		return 0, 0, 0, mfError("lock output", hr)
	}
	return c.p, 0, int(c.u2), nil
}

func (m *mfTransform) unlockSample() {
	c := &m.c
	if m.locked2D != 0 {
		c.m0(m.locked2D, slot2DUnlock)
		c.release(m.locked2D)
		m.locked2D = 0
	} else if m.locked != 0 {
		c.m0(m.locked, slotBufUnlock)
	}
	c.release(m.locked)
	m.locked = 0
}

// setType applies a media type and releases it.
func (m *mfTransform) setType(slot int, t uintptr, flags uintptr) uint32 {
	hr := m.c.m3(m.xf, slot, 0, t, flags)
	m.c.release(t)
	return hr
}
