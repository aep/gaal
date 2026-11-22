//go:build android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Blocking dequeueOutputBuffer does not return early when a buffer arrives, so
// it adds its whole timeout to the latency. Async mode reports each buffer the
// moment it exists. Assembly callbacks queue the event and wake a futex.

const (
	mcRingSize = 128
	mcRingMask = mcRingSize - 1

	futexWaitPrivate = 128
)

type mcOutput struct {
	index int32
	_     int32
	info  mcBufferInfo
}

// mcShared is written by the callbacks in mc_thunk_android_*.s.
type mcShared struct {
	seq     atomic.Uint32 // futex word, advanced by every event
	status  atomic.Int32  // first codec error
	formats atomic.Uint32
	_       uint32
	inTail  atomic.Uint64
	outTail atomic.Uint64
	inputs  [mcRingSize]int32
	outputs [mcRingSize]mcOutput
}

func mcThunks() (input, output, format, failure uintptr)

// The thunks are only called from C. These declarations satisfy go vet.
func mcInputThunk()
func mcOutputThunk()
func mcFormatThunk()
func mcErrorThunk()

// enableAsync switches the codec to callbacks. It must precede configure. The
// callback struct is larger than 16 bytes, so arm64 passes a pointer to it and
// amd64 passes it on the stack.
func (m *mcCodec) enableAsync() {
	a := m.api
	m.shared = nil // a second open attempt must not keep the first one's state
	if a.setAsync == 0 {
		return
	}
	s := &mcShared{}
	in, out, format, failure := mcThunks()
	m.callbacks = [4]uintptr{in, out, format, failure}
	var rc uintptr
	if runtime.GOARCH == "arm64" {
		rc = m.c.C3(a.setAsync, m.h, uintptr(unsafe.Pointer(&m.callbacks)), uintptr(unsafe.Pointer(s)))
	} else {
		rc = m.c.C10(a.setAsync, m.h, uintptr(unsafe.Pointer(s)), 0, 0, 0, 0, in, out, format, failure)
	}
	if int32(rc) == 0 {
		m.shared = s
	}
}

// waitEvent sleeps until a new event or the timeout, given the sequence value
// read before the caller last checked its queues.
func (m *mcCodec) waitEvent(seq uint32, us uintptr) {
	m.timeout = syscall.NsecToTimespec(int64(us) * 1000)
	syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(&m.shared.seq)), futexWaitPrivate, uintptr(seq),
		uintptr(unsafe.Pointer(&m.timeout)), 0, 0)
}

func (m *mcCodec) asyncError() error {
	if st := m.shared.status.Load(); st != 0 {
		return fmt.Errorf("mediacodec: codec error %d", st)
	}
	return nil
}

func (m *mcCodec) inputMemory(idx int) (int, []byte, error) {
	p := m.c.C3(m.api.inputBuffer, m.h, uintptr(idx), uintptr(unsafe.Pointer(&m.size)))
	if p == 0 {
		return 0, nil, errors.New("mediacodec: no input buffer memory")
	}
	return idx, cMem(p, int(m.size)), nil
}

// acquireInput returns a free input buffer, running drain while none is free.
func (m *mcCodec) acquireInput(drain func() error) (int, []byte, error) {
	if m.shared == nil {
		return m.acquireInputSync(drain)
	}
	s := m.shared
	for try := 0; try < mcInputTries; try++ {
		seq := s.seq.Load()
		if err := m.asyncError(); err != nil {
			return 0, nil, err
		}
		if m.inHead != s.inTail.Load() {
			idx := int(s.inputs[m.inHead&mcRingMask])
			m.inHead++
			return m.inputMemory(idx)
		}
		if err := drain(); err != nil {
			return 0, nil, err
		}
		m.waitEvent(seq, mcInputWaitUs)
	}
	return 0, nil, errors.New("mediacodec: timed out waiting for an input buffer")
}

func (m *mcCodec) acquireInputSync(drain func() error) (int, []byte, error) {
	wait := uintptr(0)
	for try := 0; try < mcInputTries; try++ {
		idx := int(int64(m.c.C2(m.api.dequeueInput, m.h, wait)))
		if idx >= 0 {
			return m.inputMemory(idx)
		}
		if idx != mcTryLater {
			return 0, nil, fmt.Errorf("mediacodec: dequeue input failed with %d", idx)
		}
		if err := drain(); err != nil {
			return 0, nil, err
		}
		wait = mcInputWaitUs
	}
	return 0, nil, errors.New("mediacodec: timed out waiting for an input buffer")
}

// deliver passes one output buffer in m.buf to sink and returns it to the codec.
func (m *mcCodec) deliver(idx int, sink func([]byte, *mcBufferInfo) error) error {
	var err error
	m.current = idx
	if m.buf.size > 0 {
		p := m.c.C3(m.api.outputBuffer, m.h, uintptr(idx), uintptr(unsafe.Pointer(&m.size)))
		if p == 0 {
			err = errors.New("mediacodec: no output buffer memory")
		} else {
			err = sink(cMem(p, int(m.size))[m.buf.offset:m.buf.offset+m.buf.size], &m.buf)
		}
	}
	m.current = -1
	m.finished = m.buf.flags&mcFlagEOS != 0
	m.c.C3(m.api.release, m.h, uintptr(idx), 0)
	return err
}

// pump moves every ready output buffer to sink, waiting up to waitUs for the
// first one.
func (m *mcCodec) pump(waitUs uintptr, onFormat func() error, sink func(data []byte, info *mcBufferInfo) error) error {
	if m.shared == nil {
		return m.pumpSync(waitUs, onFormat, sink)
	}
	s := m.shared
	for !m.finished {
		seq := s.seq.Load()
		if err := m.asyncError(); err != nil {
			return err
		}
		if g := s.formats.Load(); g != m.seenFormats {
			m.seenFormats = g
			m.current = -1
			if err := onFormat(); err != nil {
				return err
			}
		}
		if m.outHead == s.outTail.Load() {
			if waitUs == 0 {
				return nil
			}
			m.waitEvent(seq, waitUs)
			waitUs = 0
			continue
		}
		e := &s.outputs[m.outHead&mcRingMask]
		m.outHead++
		m.buf = e.info
		waitUs = 0
		if err := m.deliver(int(e.index), sink); err != nil {
			return err
		}
	}
	return nil
}

// pumpSync serves Android versions without async callbacks. Dequeue timeouts
// would add latency, so it polls.
func (m *mcCodec) pumpSync(waitUs uintptr, onFormat func() error, sink func([]byte, *mcBufferInfo) error) error {
	got := false
	for !m.finished {
		idx := int(int64(m.c.C3(m.api.dequeueOutput, m.h, uintptr(unsafe.Pointer(&m.buf)), 0)))
		switch {
		case idx == mcTryLater:
			if !got && waitUs > 0 {
				time.Sleep(mcPollInterval)
			}
			return nil
		case idx == mcFormatChanged:
			m.current = -1
			if err := onFormat(); err != nil {
				return err
			}
		case idx == mcBuffersChange:
		case idx < 0:
			return fmt.Errorf("mediacodec: dequeue output failed with %d", idx)
		default:
			got = true
			if err := m.deliver(idx, sink); err != nil {
				return err
			}
		}
	}
	return nil
}
