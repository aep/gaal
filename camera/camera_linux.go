//go:build linux && !android && (amd64 || arm64)

package camera

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/aep/gaal"
)

// V4L2 spoken directly to the kernel. The numbers are from linux/videodev2.h
// for 64-bit kernels and user space.
const (
	vidiocQueryCap  = 0x80685600
	vidiocEnumFmt   = 0xc0405602
	vidiocSFmt      = 0xc0d05605
	vidiocReqBufs   = 0xc0145608
	vidiocQueryBuf  = 0xc0585609
	vidiocQBuf      = 0xc058560f
	vidiocDQBuf     = 0xc0585611
	vidiocStreamOn  = 0x40045612
	vidiocStreamOff = 0x40045613
	vidiocSParm     = 0xc0cc5616

	capCapture       = 0x00000001
	capCaptureMPlane = 0x00001000
	capStreaming     = 0x04000000
	capDeviceCaps    = 0x80000000

	bufTypeCapture       = 1
	bufTypeCaptureMPlane = 9
	memoryMmap           = 1
	fieldNone            = 1

	bufFlagError         = 0x0040
	bufFlagTimestampMask = 0xe000
	bufFlagMonotonic     = 0x2000

	capStructSize = 104
	fmtDescSize   = 64
	formatSize    = 208
	parmSize      = 204
	reqBufsSize   = 20
	bufferSize    = 88
	planeSize     = 64
	planeFmtSize  = 20

	fmtPixFormat   = 16
	fmtPixStride   = 24
	fmtPixPriv     = 36
	fmtPixQuant    = 48
	fmtMPlaneFmts  = 28
	fmtMPlaneCount = 188
	fmtMPlaneQuant = 191
	pixPrivMagic   = 0xfeedcafe
	quantFull      = 1
	quantLimited   = 2

	bufOffFlags  = 12
	bufOffSec    = 24
	bufOffUsec   = 32
	bufOffMemory = 60
	bufOffM      = 64
	bufOffLength = 72
	planeDataOff = 16

	numBuffers = 4
	maxPlanes  = 8 // VIDEO_MAX_PLANES

	// maxFrameAge bounds a driver timestamp that is believed. An older one is
	// a clock mix-up, and the dequeue time is used instead.
	maxFrameAge = 2 * time.Second
)

// formatFilter, when set by a test, limits the pixel formats tried.
var formatFilter func(uint32) bool

func get32(b []byte, off int) uint32    { return *(*uint32)(unsafe.Pointer(&b[off])) }
func put32(b []byte, off int, v uint32) { *(*uint32)(unsafe.Pointer(&b[off])) = v }
func get64(b []byte, off int) uint64    { return *(*uint64)(unsafe.Pointer(&b[off])) }
func put64(b []byte, off int, v uint64) { *(*uint64)(unsafe.Pointer(&b[off])) = v }

func ioctl(fd int, req uintptr, arg []byte) error {
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&arg[0])))
		runtime.KeepAlive(arg)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

func openNode(path string) (int, error) {
	return syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
}

func cstr(b []byte) string {
	if i := slices.Index(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// queryCap describes an open node and returns its device capabilities.
func queryCap(fd int) (Device, uint32, error) {
	var c [capStructSize]byte
	if err := ioctl(fd, vidiocQueryCap, c[:]); err != nil {
		return Device{}, 0, err
	}
	d := Device{Driver: cstr(c[0:16]), Name: cstr(c[16:48]), Bus: cstr(c[48:80])}
	caps := get32(c[:], 84)
	if caps&capDeviceCaps != 0 {
		caps = get32(c[:], 88)
	}
	return d, caps, nil
}

// captureBufType picks the buffer type for a node's capabilities. Single
// planar is preferred when a driver offers both.
func captureBufType(caps uint32) (uint32, bool) {
	if caps&capStreaming == 0 {
		return 0, false
	}
	if caps&capCapture != 0 {
		return bufTypeCapture, true
	}
	if caps&capCaptureMPlane != 0 {
		return bufTypeCaptureMPlane, true
	}
	return 0, false
}

func deviceFormats(fd int, bt uint32) []uint32 {
	var have []uint32
	for i := uint32(0); ; i++ {
		var d [fmtDescSize]byte
		put32(d[:], 0, i)
		put32(d[:], 4, bt)
		if err := ioctl(fd, vidiocEnumFmt, d[:]); err != nil {
			return have
		}
		have = append(have, get32(d[:], 44))
	}
}

// supportedFormats are the device's formats that are read, best first.
func supportedFormats(fd int, bt uint32) []uint32 {
	have := deviceFormats(fd, bt)
	var out []uint32
	for _, f := range preferredFormats {
		if slices.Contains(have, f) {
			out = append(out, f)
		}
	}
	return out
}

func nodeNumber(path string) (int, bool) {
	base := filepath.Base(path)
	n, err := strconv.Atoi(strings.TrimPrefix(base, "video"))
	return n, err == nil && strings.HasPrefix(base, "video")
}

func videoNodes() []string {
	paths, _ := filepath.Glob("/dev/video*")
	var nodes []string
	for _, p := range paths {
		if _, ok := nodeNumber(p); ok {
			nodes = append(nodes, p)
		}
	}
	slices.SortFunc(nodes, func(a, b string) int {
		x, _ := nodeNumber(a)
		y, _ := nodeNumber(b)
		return x - y
	})
	return nodes
}

func devices() ([]Device, error) {
	var out []Device
	for _, path := range videoNodes() {
		fd, err := openNode(path)
		if err != nil {
			continue
		}
		d, caps, err := queryCap(fd)
		usable := false
		if err == nil {
			if bt, ok := captureBufType(caps); ok {
				usable = len(supportedFormats(fd, bt)) > 0
			}
		}
		syscall.Close(fd)
		if usable {
			d.Path = path
			out = append(out, d)
		}
	}
	return out, nil
}

func monotonicNow() time.Duration {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0) // CLOCK_MONOTONIC
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
}

type v4l2Camera struct {
	// mu is held by a capture call while it uses the buffers, so that Close
	// cannot unmap them under it. Close wakes it through the eventfd.
	mu     sync.Mutex
	closed atomic.Bool
	wake   int

	fd       int
	name     string
	bufType  uint32
	nplanes  int
	format   uint32
	width    int
	height   int
	strides  []int
	rng      gaal.ColorRange
	maps     [][][]byte // buffer, plane
	keepJPEG bool

	buf    [bufferSize]byte
	planes []byte // v4l2_plane array, on the heap so the kernel can write it
	poll   [16]byte

	// the buffer a capture call holds, read out of buf before it is reused
	held      int
	heldFlags uint32
	heldTS    time.Duration
	heldUsed  [maxPlanes]int
	heldOff   [maxPlanes]int
	raw       rawFrame
	rawPlanes [maxPlanes][]byte

	wallBase time.Time
	monoBase time.Duration

	// the camera's own capture times, for a UVC camera that sends them;
	// nil: the driver's stamps
	meta    *uvcMeta
	heldSeq uint32

	frames  gaal.FramePool
	jpeg    []byte
	lib     *gaal.Library
	decoder gaal.VideoDecoder
}

func open(cfg Config) (Camera, error) {
	if cfg.Device != "" {
		return openDevice(cfg.Device, cfg)
	}
	var errs []error
	for _, p := range videoNodes() {
		c, err := openDevice(p, cfg)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, errors.New("camera: no video devices")
	}
	return nil, errors.Join(errs...)
}

func openDevice(path string, cfg Config) (*v4l2Camera, error) {
	fd, err := openNode(path)
	if err != nil {
		return nil, fmt.Errorf("camera: open %s: %w", path, err)
	}
	wake, _, e := syscall.Syscall(syscall.SYS_EVENTFD2, 0, syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("camera: eventfd: %w", e)
	}
	c := &v4l2Camera{fd: fd, wake: int(wake), held: -1, lib: cfg.Library, keepJPEG: cfg.KeepJPEG,
		wallBase: time.Now(), monoBase: monotonicNow()}
	if err := c.setup(path, cfg); err != nil {
		c.release()
		return nil, err
	}
	return c, nil
}

func (c *v4l2Camera) setup(path string, cfg Config) error {
	dev, caps, err := queryCap(c.fd)
	if err != nil {
		return fmt.Errorf("camera: %s: QUERYCAP: %w", path, err)
	}
	bt, ok := captureBufType(caps)
	if !ok {
		return fmt.Errorf("camera: %s is not a streaming video capture node (caps 0x%08x)", path, caps)
	}
	c.bufType = bt
	mplane := bt == bufTypeCaptureMPlane
	formats := supportedFormats(c.fd, bt)
	if formatFilter != nil {
		formats = slices.DeleteFunc(formats, func(f uint32) bool { return !formatFilter(f) })
	}
	if len(formats) == 0 {
		var have []string
		for _, f := range deviceFormats(c.fd, bt) {
			have = append(have, fourccString(f))
		}
		return fmt.Errorf("camera: %s offers no supported pixel format (device has %v)", path, have)
	}

	var lastErr error
	for _, f := range formats {
		var fm [formatSize]byte
		put32(fm[:], 0, bt)
		put32(fm[:], 8, uint32(cfg.Width))
		put32(fm[:], 12, uint32(cfg.Height))
		put32(fm[:], fmtPixFormat, f)
		put32(fm[:], 20, fieldNone)
		if err := ioctl(c.fd, vidiocSFmt, fm[:]); err != nil {
			lastErr = fmt.Errorf("S_FMT %s: %w", fourccString(f), err)
			continue
		}
		if got := get32(fm[:], fmtPixFormat); got != f {
			lastErr = fmt.Errorf("S_FMT %s: driver chose %s", fourccString(f), fourccString(got))
			continue
		}
		c.format = f
		c.width, c.height = int(get32(fm[:], 8)), int(get32(fm[:], 12))
		quant := uint32(0)
		if mplane {
			c.nplanes = min(max(int(fm[fmtMPlaneCount]), 1), maxPlanes)
			c.strides = make([]int, c.nplanes)
			for i := range c.strides {
				c.strides[i] = int(get32(fm[:], fmtMPlaneFmts+i*planeFmtSize+4))
			}
			quant = uint32(fm[fmtMPlaneQuant])
		} else {
			c.nplanes = 1
			c.strides = []int{int(get32(fm[:], fmtPixStride))}
			if get32(fm[:], fmtPixPriv) == pixPrivMagic {
				quant = get32(fm[:], fmtPixQuant)
			}
		}
		switch {
		case isJPEG(f), quant == quantFull:
			c.rng = gaal.RangeFull
		case quant == quantLimited:
			c.rng = gaal.RangeLimited
		}
		break
	}
	if c.format == 0 {
		return fmt.Errorf("camera: %s: could not set a pixel format: %v", path, lastErr)
	}
	if cfg.FPS > 0 {
		var parm [parmSize]byte
		put32(parm[:], 0, bt)
		put32(parm[:], 12, 1)
		put32(parm[:], 16, uint32(cfg.FPS))
		_ = ioctl(c.fd, vidiocSParm, parm[:]) // best effort: the driver keeps its own rate
	}

	var req [reqBufsSize]byte
	put32(req[:], 0, numBuffers)
	put32(req[:], 4, bt)
	put32(req[:], 8, memoryMmap)
	if err := ioctl(c.fd, vidiocReqBufs, req[:]); err != nil {
		return fmt.Errorf("camera: REQBUFS: %w", err)
	}
	count := int(get32(req[:], 0))
	c.planes = make([]byte, maxPlanes*planeSize)
	c.maps = make([][][]byte, count)
	for i := range count {
		c.prepareBuf(i)
		if err := ioctl(c.fd, vidiocQueryBuf, c.buf[:]); err != nil {
			return fmt.Errorf("camera: QUERYBUF[%d]: %w", i, err)
		}
		c.maps[i] = make([][]byte, c.nplanes)
		for p := range c.nplanes {
			var off int64
			var length int
			if mplane {
				pl := c.planes[p*planeSize:]
				length, off = int(get32(pl, 4)), int64(get32(pl, 8))
			} else {
				off, length = int64(get32(c.buf[:], bufOffM)), int(get32(c.buf[:], bufOffLength))
			}
			m, err := syscall.Mmap(c.fd, off, length, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				return fmt.Errorf("camera: mmap[%d.%d]: %w", i, p, err)
			}
			c.maps[i][p] = m
		}
	}
	for i := range count {
		if err := c.queue(i); err != nil {
			return fmt.Errorf("camera: QBUF[%d]: %w", i, err)
		}
	}
	kind := make([]byte, 4)
	put32(kind, 0, bt)
	if err := ioctl(c.fd, vidiocStreamOn, kind); err != nil {
		return fmt.Errorf("camera: STREAMON: %w", err)
	}
	c.meta = openUVCMeta(path)
	c.raw = rawFrame{format: c.format, width: c.width, height: c.height, strides: c.strides}
	api := ""
	if mplane {
		api = fmt.Sprintf(" mplane/%d", c.nplanes)
	}
	c.name = fmt.Sprintf("%s (%s) %dx%d %s%s", path, dev.Name, c.width, c.height, fourccString(c.format), api)
	return nil
}

// prepareBuf fills c.buf for buffer i, or for a dequeue when i is negative.
func (c *v4l2Camera) prepareBuf(i int) {
	b := c.buf[:]
	clear(b)
	if i >= 0 {
		put32(b, 0, uint32(i))
	}
	put32(b, 4, c.bufType)
	put32(b, bufOffMemory, memoryMmap)
	if c.bufType == bufTypeCaptureMPlane {
		clear(c.planes)
		put64(b, bufOffM, uint64(uintptr(unsafe.Pointer(&c.planes[0]))))
		put32(b, bufOffLength, uint32(c.nplanes))
	}
}

func (c *v4l2Camera) queue(i int) error {
	c.prepareBuf(i)
	err := ioctl(c.fd, vidiocQBuf, c.buf[:])
	runtime.KeepAlive(c.planes)
	return err
}

// requeue gives the held buffer back to the driver.
func (c *v4l2Camera) requeue() error {
	if c.held < 0 {
		return nil
	}
	i := c.held
	c.held = -1
	if err := c.queue(i); err != nil {
		return fmt.Errorf("camera: QBUF: %w", err)
	}
	return nil
}

// wait polls the device and the wake eventfd.
func (c *v4l2Camera) wait(timeout time.Duration) error {
	const pollIn = 1
	clear(c.poll[:])
	put32(c.poll[:], 0, uint32(c.fd))
	put32(c.poll[:], 4, pollIn)
	put32(c.poll[:], 8, uint32(c.wake))
	put32(c.poll[:], 12, pollIn)
	ts := syscall.NsecToTimespec(int64(timeout))
	_, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&c.poll)), 2, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	if e != 0 && e != syscall.EINTR {
		return fmt.Errorf("camera: poll: %w", e)
	}
	if c.closed.Load() {
		return ErrClosed
	}
	return nil
}

// dequeue takes every ready buffer and holds the newest whole one, giving
// older and damaged ones straight back. It reports whether it holds one.
func (c *v4l2Camera) dequeue() (bool, error) {
	got := false
	for {
		c.prepareBuf(-1)
		err := ioctl(c.fd, vidiocDQBuf, c.buf[:])
		runtime.KeepAlive(c.planes)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || got {
				return got, nil
			}
			return false, fmt.Errorf("camera: DQBUF: %w", err)
		}
		b := c.buf[:]
		i, flags := int(get32(b, 0)), get32(b, bufOffFlags)
		if flags&bufFlagError != 0 || i < 0 || i >= len(c.maps) {
			if err := c.queue(i); err != nil {
				return false, fmt.Errorf("camera: QBUF: %w", err)
			}
			continue
		}
		// Read this buffer out before queueing the older one reuses c.buf.
		var used, off [maxPlanes]int
		for p := range c.nplanes {
			if c.bufType == bufTypeCaptureMPlane {
				pl := c.planes[p*planeSize:]
				used[p], off[p] = int(get32(pl, 0)), int(get32(pl, planeDataOff))
			} else {
				used[p] = int(get32(b, 8))
			}
		}
		ts := time.Duration(-1)
		if flags&bufFlagTimestampMask == bufFlagMonotonic {
			ts = time.Duration(get64(b, bufOffSec))*time.Second + time.Duration(get64(b, bufOffUsec))*time.Microsecond
		}
		if err := c.requeue(); err != nil {
			return false, err
		}
		c.held, c.heldFlags, c.heldTS, c.heldUsed, c.heldOff = i, flags, ts, used, off
		c.heldSeq = get32(b, bufOffSequence)
		got = true
	}
}

// timing is when the held buffer was captured: wall clock and PTS.
func (c *v4l2Camera) timing() (time.Time, time.Duration) {
	now := monotonicNow()
	ts := c.heldTS
	if ts <= 0 || now-ts < 0 || now-ts > maxFrameAge {
		ts = now
	} else if c.meta != nil {
		// the driver stamped the picture when its first packet came: the
		// camera started capturing it before (uvcmeta_linux.go)
		if at, ok := c.meta.captured(c.heldSeq, ts); ok {
			ts = at
		}
	}
	return c.wallBase.Add(ts - c.monoBase), ts - c.monoBase
}

// heldPlanes are the held buffer's bytes, plane by plane.
func (c *v4l2Camera) heldPlanes() [][]byte {
	for p := range c.nplanes {
		m := c.maps[c.held][p]
		start, end := c.heldOff[p], c.heldUsed[p]
		if end <= 0 || end > len(m) {
			end = len(m)
		}
		start = min(max(start, 0), end)
		c.rawPlanes[p] = m[start:end]
	}
	return c.rawPlanes[:c.nplanes]
}

func (c *v4l2Camera) Name() string { return c.name }

func (c *v4l2Camera) Next(timeout time.Duration) (*gaal.VideoFrame, error) {
	cp, err := c.capture(timeout, false)
	if err != nil {
		return nil, err
	}
	return cp.Frame, nil
}

func (c *v4l2Camera) Capture(timeout time.Duration) (Capture, error) {
	return c.capture(timeout, c.keepJPEG)
}

func (c *v4l2Camera) capture(timeout time.Duration, keepJPEG bool) (Capture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for {
		if c.closed.Load() {
			return Capture{}, ErrClosed
		}
		left := time.Until(deadline)
		if left <= 0 {
			return Capture{}, fmt.Errorf("camera: no frame in %v: %w", timeout, gaal.ErrTimeout)
		}
		if err := c.wait(left); err != nil {
			return Capture{}, err
		}
		got, err := c.dequeue()
		if err != nil {
			return Capture{}, err
		}
		if !got {
			continue
		}
		out, ok, err := c.take(keepJPEG, left)
		if qerr := c.requeue(); err == nil && qerr != nil {
			err = qerr
		}
		if err != nil || !ok {
			if out.Frame != nil {
				out.Frame.Release()
			}
			if err != nil {
				return Capture{}, err
			}
			continue // a damaged picture: wait for the next
		}
		return out, nil
	}
}

// take turns the held buffer into a capture. ok is false for a picture to
// skip.
func (c *v4l2Camera) take(keepJPEG bool, timeout time.Duration) (Capture, bool, error) {
	captured, pts := c.timing()
	out := Capture{Width: c.width, Height: c.height, Captured: captured}
	planes := c.heldPlanes()
	if !isJPEG(c.format) {
		c.raw.planes = planes
		f, err := convert(&c.raw, &c.frames)
		c.raw.planes = nil
		if err != nil {
			return out, false, nil // a short buffer: skip it
		}
		f.PTS, f.Range = pts, c.rng
		out.Frame = f
		return out, true, nil
	}
	if !jpegWhole(planes[0]) {
		return out, false, nil
	}
	c.jpeg = gaal.AppendCompleteJPEG(c.jpeg[:0], planes[0])
	if keepJPEG {
		out.JPEG = c.jpeg
		return out, true, nil
	}
	if c.decoder == nil {
		lib := c.lib
		if lib == nil {
			lib = gaal.New(gaal.Options{})
		}
		d, err := lib.NewVideoDecoder(gaal.VideoConfig{Codec: gaal.MJPEG})
		if err != nil {
			return out, false, err
		}
		c.decoder = d
		c.name += " via " + d.Backend().Name
	}
	if err := c.decoder.Send(gaal.Packet{Data: c.jpeg, PTS: pts, Keyframe: true}); err != nil {
		if errors.Is(err, gaal.ErrCorrupt) {
			return out, false, nil
		}
		return out, false, err
	}
	f, err := gaal.ReceiveWait(c.decoder, timeout)
	if err != nil {
		if errors.Is(err, gaal.ErrCorrupt) || errors.Is(err, gaal.ErrTimeout) || errors.Is(err, gaal.ErrAgain) {
			return out, false, nil
		}
		return out, false, err
	}
	f.PTS = pts
	if f.Range == gaal.RangeUnspecified {
		f.Range = gaal.RangeFull
	}
	out.Frame = f
	return out, true, nil
}

// Close may be called while another goroutine is blocked capturing: that call
// returns ErrClosed.
func (c *v4l2Camera) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	var one [8]byte
	one[0] = 1
	syscall.Write(c.wake, one[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.release()
	return nil
}

func (c *v4l2Camera) release() {
	if c.fd >= 0 {
		kind := make([]byte, 4)
		put32(kind, 0, c.bufType)
		_ = ioctl(c.fd, vidiocStreamOff, kind)
		for _, planes := range c.maps {
			for _, m := range planes {
				if m != nil {
					syscall.Munmap(m)
				}
			}
		}
		c.maps = nil
		syscall.Close(c.fd)
		c.fd = -1
	}
	if c.wake >= 0 {
		syscall.Close(c.wake)
		c.wake = -1
	}
	if c.decoder != nil {
		c.decoder.Close()
		c.decoder = nil
	}
	if c.meta != nil {
		c.meta.close()
		c.meta = nil
	}
}

var _ Capturer = (*v4l2Camera)(nil)
