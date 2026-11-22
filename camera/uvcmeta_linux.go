//go:build linux && !android && (amd64 || arm64)

package camera

import (
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Sensor timestamps of UVC cameras.
//
// The time a V4L2 driver stamps a UVC picture with is when its first USB
// packet arrived: the camera exposed it, read it out and compressed it
// before, which takes it 10 to 20 ms or more. A UVC camera may tell when it
// started to capture each picture: the payload headers of its packets carry a
// PTS, the camera's clock at the start of the capture, and an SCR, the
// camera's clock when the packet was sent. Linux hands the headers out,
// together with the host's time of each packet, on the camera's metadata
// node (V4L2_META_FMT_UVC, a second /dev/video node of the same device).
//
// From them a picture is timed by the camera: its capture started (SCR - PTS)
// before the packet that said so arrived, in ticks of the camera's clock,
// whose rate is measured from the SCRs and the host times of the packets.
// The header descriptor states the rate as well (dwClockFrequency), but
// cameras misreport it. A camera that sends no PTS or SCR, a node that cannot
// be opened, and times that are not plausible leave the driver's stamp.

const (
	capMetaCapture  = 0x00800000
	bufTypeMeta     = 13 // V4L2_BUF_TYPE_META_CAPTURE
	bufOffSequence  = 56
	bufOffBytesUsed = 8

	uvcHeaderPTS = 0x04 // bmHeaderInfo: dwPresentationTime follows
	uvcHeaderSCR = 0x08 // bmHeaderInfo: scrSourceClock follows

	metaBuffers = 8
	// metaEntry is the size of struct uvc_meta_buf before the header bytes
	// that follow its length and flags: ns (8), sof (2), length, flags.
	metaEntry = 12

	// clockSpan is how far apart the two packets the camera's clock rate is
	// measured between are at least, and clockAnchor how far apart at most
	// (well under a wrap of the 32-bit SCR, 89 s at 48 MHz).
	clockSpan   = time.Second
	clockAnchor = 20 * time.Second
	// maxCaptureLead bounds how long before the driver's stamp a capture may
	// have started: longer is a clock that is wrong.
	maxCaptureLead = 500 * time.Millisecond
	// metaKept is how many pictures' capture times are kept to be matched
	// with their video buffers.
	metaKept = 16
)

var pixUVCH = fourcc('U', 'V', 'C', 'H')

// uvcMeta reads the metadata node of a UVC camera and times its pictures by
// the camera's clock.
type uvcMeta struct {
	fd   int
	path string
	maps [][]byte
	buf  [bufferSize]byte

	clock   uvcClock
	capture [metaKept]struct {
		seq uint32
		at  time.Duration // host monotonic; 0: none
	}
}

// openUVCMeta opens and streams the metadata node of the UVC camera whose
// video node is video, or returns nil when it has none that can be used.
func openUVCMeta(video string) *uvcMeta {
	path := uvcMetaNode(video)
	if path == "" {
		return nil
	}
	m, err := startUVCMeta(path)
	if err != nil {
		return nil
	}
	return m
}

// uvcMetaNode finds the node of the same device as video that offers UVC
// payload header metadata.
func uvcMetaNode(video string) string {
	devOf := func(node string) string {
		d, err := filepath.EvalSymlinks(filepath.Join("/sys/class/video4linux", filepath.Base(node), "device"))
		if err != nil {
			return ""
		}
		return d
	}
	dev := devOf(video)
	if dev == "" {
		return ""
	}
	for _, p := range videoNodes() {
		if p == video || devOf(p) != dev {
			continue
		}
		fd, err := openNode(p)
		if err != nil {
			continue
		}
		_, caps, err := queryCap(fd)
		ok := err == nil && caps&capMetaCapture != 0
		if ok {
			ok = false
			for _, f := range deviceFormats(fd, bufTypeMeta) {
				ok = ok || f == pixUVCH
			}
		}
		_ = syscall.Close(fd)
		if ok {
			return p
		}
	}
	return ""
}

func startUVCMeta(path string) (m *uvcMeta, err error) {
	fd, err := openNode(path)
	if err != nil {
		return nil, err
	}
	m = &uvcMeta{fd: fd, path: path}
	defer func() {
		if err != nil {
			m.close()
		}
	}()
	var fm [formatSize]byte
	put32(fm[:], 0, bufTypeMeta)
	put32(fm[:], 8, pixUVCH)
	if err := ioctl(fd, vidiocSFmt, fm[:]); err != nil {
		return nil, fmt.Errorf("S_FMT: %w", err)
	}
	var req [reqBufsSize]byte
	put32(req[:], 0, metaBuffers)
	put32(req[:], 4, bufTypeMeta)
	put32(req[:], 8, memoryMmap)
	if err := ioctl(fd, vidiocReqBufs, req[:]); err != nil {
		return nil, fmt.Errorf("REQBUFS: %w", err)
	}
	n := int(get32(req[:], 0))
	for i := range n {
		m.prepare(i)
		if err := ioctl(fd, vidiocQueryBuf, m.buf[:]); err != nil {
			return nil, fmt.Errorf("QUERYBUF: %w", err)
		}
		mp, err := syscall.Mmap(fd, int64(get32(m.buf[:], bufOffM)), int(get32(m.buf[:], bufOffLength)), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("mmap: %w", err)
		}
		m.maps = append(m.maps, mp)
	}
	for i := range n {
		m.prepare(i)
		if err := ioctl(fd, vidiocQBuf, m.buf[:]); err != nil {
			return nil, fmt.Errorf("QBUF: %w", err)
		}
	}
	kind := make([]byte, 4)
	put32(kind, 0, bufTypeMeta)
	if err := ioctl(fd, vidiocStreamOn, kind); err != nil {
		return nil, fmt.Errorf("STREAMON: %w", err)
	}
	return m, nil
}

func (m *uvcMeta) prepare(i int) {
	b := m.buf[:]
	clear(b)
	if i >= 0 {
		put32(b, 0, uint32(i))
	}
	put32(b, 4, bufTypeMeta)
	put32(b, bufOffMemory, memoryMmap)
}

// drain reads every metadata buffer that is ready.
func (m *uvcMeta) drain() {
	for {
		m.prepare(-1)
		if err := ioctl(m.fd, vidiocDQBuf, m.buf[:]); err != nil {
			return
		}
		i, seq, used := int(get32(m.buf[:], 0)), get32(m.buf[:], bufOffSequence), int(get32(m.buf[:], bufOffBytesUsed))
		if i >= 0 && i < len(m.maps) {
			if at, ok := m.clock.picture(m.maps[i][:min(used, len(m.maps[i]))]); ok {
				m.capture[seq%metaKept] = struct {
					seq uint32
					at  time.Duration
				}{seq, at}
			}
		}
		m.prepare(i)
		if err := ioctl(m.fd, vidiocQBuf, m.buf[:]); err != nil {
			return
		}
		runtime.KeepAlive(m.maps)
	}
}

// captured is when picture seq began to be captured, host monotonic, if the
// camera said so plausibly: before the driver stamped it at stamp, but not
// long before.
func (m *uvcMeta) captured(seq uint32, stamp time.Duration) (time.Duration, bool) {
	m.drain()
	c := m.capture[seq%metaKept]
	if c.seq != seq || c.at == 0 {
		return 0, false
	}
	if lead := stamp - c.at; lead < 0 || lead > maxCaptureLead {
		return 0, false
	}
	return c.at, true
}

func (m *uvcMeta) close() {
	if m.fd < 0 {
		return
	}
	kind := make([]byte, 4)
	put32(kind, 0, bufTypeMeta)
	_ = ioctl(m.fd, vidiocStreamOff, kind)
	for _, mp := range m.maps {
		_ = syscall.Munmap(mp)
	}
	m.maps = nil
	_ = syscall.Close(m.fd)
	m.fd = -1
}

// uvcClock relates the camera's clock to the host's, from the packets of
// its pictures.
type uvcClock struct {
	// the packet the rate is measured from, and the one that replaces it
	// once it is clockAnchor old
	anchor, next struct {
		host time.Duration
		scr  uint32
	}
	anchored bool
	rate     float64 // ticks of the camera's clock per nanosecond; 0: not known yet
}

// header is one payload header of a metadata buffer.
type uvcHeader struct {
	host     time.Duration // when the host had the packet, monotonic
	pts, scr uint32
	hasPTS   bool
	hasSCR   bool
}

// nextUVCHeader reads the first payload header of a metadata buffer and
// returns the rest; ok is false at its end.
func nextUVCHeader(b []byte) (h uvcHeader, rest []byte, ok bool) {
	if len(b) < metaEntry {
		return h, nil, false
	}
	length, flags := int(b[10]), b[11]
	size := metaEntry + length - 2
	if length < 2 || size > len(b) {
		return h, nil, false
	}
	h.host = time.Duration(get64(b, 0))
	fields := b[metaEntry:size]
	if flags&uvcHeaderPTS != 0 && len(fields) >= 4 {
		h.pts, h.hasPTS = get32(fields, 0), true
		fields = fields[4:]
	}
	if flags&uvcHeaderSCR != 0 && len(fields) >= 6 {
		h.scr, h.hasSCR = get32(fields, 0), true
	}
	return h, b[size:], true
}

// picture learns the clock from the headers of one picture's metadata buffer
// and returns when the picture began to be captured, host monotonic. Of its
// headers with a PTS and an SCR, the one that gives the earliest time is the
// one the host had soonest after the camera sent it.
func (k *uvcClock) picture(b []byte) (time.Duration, bool) {
	var at time.Duration
	found := false
	for {
		h, rest, ok := nextUVCHeader(b)
		if !ok {
			break
		}
		b = rest
		if !h.hasSCR {
			continue
		}
		k.observe(h.host, h.scr)
		if !h.hasPTS || k.rate == 0 {
			continue
		}
		ticks := h.scr - h.pts // modulo 2^32: the PTS is a little older
		t := h.host - time.Duration(float64(ticks)/k.rate)
		if !found || t < at {
			at, found = t, true
		}
	}
	return at, found
}

// observe takes a packet sent at camera time scr and had by the host at host.
// The rate is the ticks between an anchor packet at least clockSpan ago and
// this one, over the host time between them. The anchor moves on, to a packet
// half of clockAnchor younger, before the SCR could wrap.
func (k *uvcClock) observe(host time.Duration, scr uint32) {
	if !k.anchored || host < k.anchor.host {
		k.anchor.host, k.anchor.scr = host, scr
		k.next, k.anchored, k.rate = k.anchor, true, 0
		return
	}
	if host-k.anchor.host > clockAnchor {
		k.anchor = k.next
	}
	if host-k.next.host >= clockAnchor/2 {
		k.next.host, k.next.scr = host, scr
	}
	if span := host - k.anchor.host; span >= clockSpan {
		if r := float64(scr-k.anchor.scr) / float64(span); r > 0 {
			k.rate = r
		}
	}
}
