//go:build !android && !windows && (amd64 || arm64)

package gaal

import (
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

// These call the libFLAC callbacks directly, so that they are exercised on every
// architecture even where libFLAC is not installed.

// thunkKeep forces test data to the heap, as C must not see goroutine stacks.
var thunkKeep []any

func onHeap[T any](v T) T {
	thunkKeep = append(thunkKeep, v)
	return v
}

func callThunk(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}

func TestFLACEncoderWriteThunk(t *testing.T) {
	write, _, _, _, _ := audioThunks()
	buf := onHeap(make([]byte, 16))
	s := onHeap(new(flacShared))
	s.out, s.outCap = uint64(uintptr(unsafe.Pointer(&buf[0]))), uint64(len(buf))
	client := uintptr(unsafe.Pointer(s))
	data := onHeap([]byte("0123456789"))
	src := uintptr(unsafe.Pointer(&data[0]))
	if r := callThunk(write, 0, src, 4, 0, 0, client); r != 0 {
		t.Fatalf("status %d", r)
	}
	if s.count != 0 || s.outLen != 4 {
		t.Fatalf("header write: count %d, length %d", s.count, s.outLen)
	}
	callThunk(write, 0, src, 10, 1152, 0, client)
	if string(buf[:14]) != "01230123456789" || s.count != 1 || s.ends[0] != 14 {
		t.Fatalf("frame write: %q, count %d, end %d", buf[:14], s.count, s.ends[0])
	}
	callThunk(write, 0, src, 10, 1152, 0, client)
	if s.overflow == 0 || s.outLen != 14 {
		t.Fatalf("overflow not reported: %d, length %d", s.overflow, s.outLen)
	}
}

func TestFLACDecoderThunks(t *testing.T) {
	_, read, write, failure, _ := audioThunks()
	in := onHeap([]byte("abcdefgh"))
	s := onHeap(new(flacShared))
	s.in, s.inLeft = uint64(uintptr(unsafe.Pointer(&in[0]))), uint64(len(in))
	client := uintptr(unsafe.Pointer(s))
	dst := onHeap(make([]byte, 16))
	sizes := onHeap(new([1]uint64))
	sizes[0] = 5
	if r := callThunk(read, 0, uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(sizes)), client); r != 0 || sizes[0] != 5 {
		t.Fatalf("read: status %d, size %d", r, sizes[0])
	}
	sizes[0] = 16
	if r := callThunk(read, 0, uintptr(unsafe.Pointer(&dst[5])), uintptr(unsafe.Pointer(sizes)), client); r != 0 || sizes[0] != 3 {
		t.Fatalf("short read: status %d, size %d", r, sizes[0])
	}
	if string(dst[:8]) != "abcdefgh" {
		t.Fatalf("read %q", dst[:8])
	}
	sizes[0] = 16
	if r := callThunk(read, 0, uintptr(unsafe.Pointer(&dst[0])), uintptr(unsafe.Pointer(sizes)), client); r != 1 || sizes[0] != 0 {
		t.Fatalf("end of input: status %d, size %d", r, sizes[0])
	}

	for _, c := range []struct {
		bits int
		in   int32
		want int16
	}{{16, -1234, -1234}, {24, 0x123456, 0x1234}, {24, -0x123456, -0x1235}, {8, 0x12, 0x1200}} {
		left := onHeap([]int32{c.in, 1 << (c.bits - 2)})
		right := onHeap([]int32{-c.in, 0})
		chans := onHeap(&[2]*int32{&left[0], &right[0]})
		header := onHeap(&[5]uint32{2, 48000, 2, 0, uint32(c.bits)}) // blocksize, rate, channels, assignment, bits
		pcm := onHeap(make([]int16, 8))
		*s = flacShared{pcm: uint64(uintptr(unsafe.Pointer(&pcm[0]))), pcmCap: 8}
		r := callThunk(write, 0, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(chans)), client)
		if r != 0 || s.pcmLen != 4 || s.blocks != 2 || s.channels != 2 || s.rate != 48000 || s.decoded != 1 {
			t.Fatalf("%d bits: status %d, shared %+v", c.bits, r, s)
		}
		if pcm[0] != c.want || pcm[2] != 0x4000 {
			t.Errorf("%d bits: samples %v, want %d first and 0x4000 third", c.bits, pcm[:4], c.want)
		}
		header[0] = 100
		callThunk(write, 0, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(chans)), client)
		if s.full == 0 || s.pcmLen != 4 {
			t.Errorf("%d bits: overflow not reported", c.bits)
		}
	}
	callThunk(failure, 0, 1, client)
	if s.errors != 1 {
		t.Errorf("errors = %d", s.errors)
	}
}
