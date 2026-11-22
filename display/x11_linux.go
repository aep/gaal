//go:build linux && !android

package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/aep/gaal"
)

var le = binary.LittleEndian

// Features tests can switch off to exercise the fallback paths.
const (
	x11NoXV = 1 << iota
	x11NoRender
	x11NoShm
)

var x11Disable int

const (
	fourccNV12 = 0x3231564E
	fourccYV12 = 0x32315659
	fourccI420 = 0x30323449
)

type x11Path int

const (
	x11PathNone x11Path = iota
	x11PathXV
	x11PathRender
	x11PathShmPut
	x11PathPutImage
)

type xreply struct {
	data []byte // the whole reply, or nil after an error
	err  error
}

type x11 struct {
	d    *Display
	conn *net.UnixConn

	wmu     sync.Mutex // protects the socket write side and the fields below
	seq     uint16
	waiters map[uint16]chan xreply
	frame   wbuf // request buffer for the frame path

	syncSeq  uint16
	syncWait bool
	syncAt   int64

	idBase, idNext, idMask uint32
	maxRequest             int
	root, visual           uint32
	win, gc                uint32
	wmProtocols, wmDelete  uint32

	shmOp, xvOp, renderOp byte
	shmErrBase            byte
	xvPort                uint32
	xvIDs                 map[uint32]bool
	xvColorspace          uint32 // attribute atom, 0 if absent
	xvFailed              atomic.Bool
	renderFailed          atomic.Bool
	pictWindow            uint32 // picture of the window
	pictFormat            uint32

	winW, winH atomic.Int32
	dead       atomic.Bool

	// Current path. These change only in prepare, which the engine serializes
	// with fill, and under wmu so that submit sees them consistently.
	path      x11Path
	l         layout
	mem       *shmem
	seg       uint32
	bufSize   int
	bufW      int // pixel buffers: size of each buffer
	bufH      int
	stride    int
	pixmaps   [3]uint32
	pictures  [3]uint32
	xvID      uint32
	xvPitch   [3]int
	xvOffset  [3]int
	heap      [3][]byte // putimage path
	staging   []byte    // frame-sized image when scaling on the CPU
	converted bool
	lastSrc   rect
	lastDst   rect
}

func openX11(d *Display, o Options) (backend, error) {
	num, err := x11DisplayNumber(os.Getenv("DISPLAY"))
	if err != nil {
		return nil, err
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "/tmp/.X11-unix/X" + num, Net: "unix"})
	if err != nil {
		return nil, err
	}
	x := &x11{d: d, conn: c, waiters: map[uint16]chan xreply{}}
	x.winW.Store(int32(o.Width))
	x.winH.Store(int32(o.Height))
	if err := x.handshake(num); err != nil {
		c.Close()
		return nil, err
	}
	go x.reader()
	if err := x.setup(o); err != nil {
		c.Close()
		return nil, err
	}
	return x, nil
}

func x11DisplayNumber(s string) (string, error) {
	if s == "" {
		return "", errors.New("DISPLAY is not set")
	}
	i := strings.LastIndexByte(s, ':')
	if i < 0 || (i > 0 && s[:i] != "unix" && !strings.HasPrefix(s, "/")) {
		return "", fmt.Errorf("DISPLAY %q is not a local display", s)
	}
	n := s[i+1:]
	if j := strings.IndexByte(n, '.'); j >= 0 {
		n = n[:j]
	}
	if n == "" {
		return "", fmt.Errorf("malformed DISPLAY %q", s)
	}
	return n, nil
}

func pad4(n int) int { return (n + 3) &^ 3 }

func xauthCookie(display string) []byte {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".Xauthority")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	next := func() []byte {
		if len(b) < 2 {
			b = nil
			return nil
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			b = nil
			return nil
		}
		v := b[2 : 2+n]
		b = b[2+n:]
		return v
	}
	var fallback []byte
	for len(b) > 2 {
		b = b[2:] // family
		next()    // address
		num := string(next())
		name := string(next())
		data := next()
		if name == "MIT-MAGIC-COOKIE-1" {
			if num == display {
				return data
			}
			if num == "" && fallback == nil {
				fallback = data
			}
		}
	}
	return fallback
}

func (x *x11) handshake(display string) error {
	const authName = "MIT-MAGIC-COOKIE-1"
	cookie := xauthCookie(display)
	name := authName
	if cookie == nil {
		name = ""
	}
	req := make([]byte, 12+pad4(len(name))+pad4(len(cookie)))
	req[0] = 'l'
	le.PutUint16(req[2:], 11)
	le.PutUint16(req[6:], uint16(len(name)))
	le.PutUint16(req[8:], uint16(len(cookie)))
	copy(req[12:], name)
	copy(req[12+pad4(len(name)):], cookie)
	if _, err := x.conn.Write(req); err != nil {
		return err
	}
	var head [8]byte
	if _, err := io.ReadFull(x.conn, head[:]); err != nil {
		return err
	}
	info := make([]byte, 4*int(le.Uint16(head[6:])))
	if _, err := io.ReadFull(x.conn, info); err != nil {
		return err
	}
	if head[0] != 1 {
		return fmt.Errorf("connection refused: %s", info[:min(int(head[1]), len(info))])
	}
	if info[22] != 0 {
		return errors.New("server uses most-significant-byte-first images")
	}
	x.idBase, x.idMask = le.Uint32(info[4:]), le.Uint32(info[8:])
	x.maxRequest = int(le.Uint16(info[18:])) * 4
	vendor, formats := int(le.Uint16(info[16:])), int(info[21])
	s := info[32+pad4(vendor)+8*formats:]
	x.root = le.Uint32(s)
	depths := int(s[39])
	p := s[40:]
	for i := 0; i < depths && x.visual == 0; i++ {
		depth, count := p[0], int(le.Uint16(p[2:]))
		for j := 0; j < count; j++ {
			v := p[8+24*j:]
			if depth == 24 && v[4] == 4 && le.Uint32(v[8:]) == 0xff0000 &&
				le.Uint32(v[12:]) == 0xff00 && le.Uint32(v[16:]) == 0xff {
				x.visual = le.Uint32(v)
				break
			}
		}
		p = p[8+24*count:]
	}
	if x.visual == 0 {
		return errors.New("no 24-bit true colour visual")
	}
	return nil
}

func (x *x11) newID() uint32 {
	x.wmu.Lock()
	defer x.wmu.Unlock()
	id := x.idBase | (x.idNext & x.idMask)
	x.idNext++
	return id
}

// xreq builds one request. Field helpers append, and finish fills the length.
type xreq struct{ b []byte }

func newReq(op, detail byte) *xreq { return &xreq{b: []byte{op, detail, 0, 0}} }

func (r *xreq) u8(v ...byte) *xreq { r.b = append(r.b, v...); return r }
func (r *xreq) u16(v ...int) *xreq {
	for _, n := range v {
		r.b = le.AppendUint16(r.b, uint16(n))
	}
	return r
}
func (r *xreq) u32(v ...uint32) *xreq {
	for _, n := range v {
		r.b = le.AppendUint32(r.b, n)
	}
	return r
}
func (r *xreq) str(s string) *xreq {
	r.b = append(r.b, s...)
	for len(r.b)%4 != 0 {
		r.b = append(r.b, 0)
	}
	return r
}
func (r *xreq) finish() []byte {
	le.PutUint16(r.b[2:], uint16(len(r.b)/4))
	return r.b
}

// send writes one request without waiting.
func (x *x11) send(r *xreq) error {
	x.wmu.Lock()
	defer x.wmu.Unlock()
	x.seq++
	_, err := x.conn.Write(r.finish())
	return err
}

// call writes one request and waits for its reply or error.
func (x *x11) call(r *xreq) ([]byte, error) { return x.callFD(r, -1) }

func (x *x11) callFD(r *xreq, fd int) ([]byte, error) {
	ch := make(chan xreply, 1)
	x.wmu.Lock()
	if x.dead.Load() {
		x.wmu.Unlock()
		return nil, ErrClosed
	}
	x.seq++
	x.waiters[x.seq] = ch
	var err error
	if fd >= 0 {
		_, _, err = x.conn.WriteMsgUnix(r.finish(), syscall.UnixRights(fd), nil)
	} else {
		_, err = x.conn.Write(r.finish())
	}
	x.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	rep := <-ch
	return rep.data, rep.err
}

// check sends a request that has no reply and waits until the server has
// processed it, returning its error if it failed.
func (x *x11) check(r *xreq) error { return x.checkFD(r, -1) }

func (x *x11) checkFD(r *xreq, fd int) error {
	errCh := make(chan xreply, 1)
	ch := make(chan xreply, 1)
	sync := newReq(43, 0).finish()
	x.wmu.Lock()
	if x.dead.Load() {
		x.wmu.Unlock()
		return ErrClosed
	}
	x.seq++
	x.waiters[x.seq] = errCh
	first := x.seq
	var err error
	if fd >= 0 {
		_, _, err = x.conn.WriteMsgUnix(r.finish(), syscall.UnixRights(fd), nil)
	} else {
		_, err = x.conn.Write(r.finish())
	}
	x.seq++
	x.waiters[x.seq] = ch
	if err == nil {
		_, err = x.conn.Write(sync)
	}
	x.wmu.Unlock()
	if err != nil {
		return err
	}
	rep := <-ch
	if rep.err != nil {
		return rep.err
	}
	select {
	case e := <-errCh:
		return e.err
	default:
	}
	x.wmu.Lock()
	delete(x.waiters, first)
	x.wmu.Unlock()
	return nil
}

func (x *x11) reader() {
	buf := make([]byte, 1<<16)
	have := 0
	var err error
	for err == nil {
		var n int
		n, err = x.conn.Read(buf[have:])
		have += n
		off := 0
		for have-off >= 32 {
			m := buf[off:have]
			size := 32
			if m[0] == 1 || m[0]&0x7f == 35 {
				size += 4 * int(le.Uint32(m[4:]))
			}
			if size > len(m) {
				break
			}
			x.dispatch(m[:size])
			off += size
		}
		copy(buf, buf[off:have])
		have -= off
		// Grow for a reply that does not fit, such as GetImage.
		if have >= 32 && (buf[0] == 1 || buf[0]&0x7f == 35) {
			if size := 32 + 4*int(le.Uint32(buf[4:])); size > len(buf) {
				bigger := make([]byte, size)
				copy(bigger, buf[:have])
				buf = bigger
			}
		}
	}
	x.wmu.Lock()
	x.dead.Store(true)
	for s, ch := range x.waiters {
		ch <- xreply{err: ErrClosed}
		delete(x.waiters, s)
	}
	x.wmu.Unlock()
	if !x.d.e.isClosed() {
		x.d.e.events.push(Event{Kind: EventClose})
	}
}

func (x *x11) dispatch(m []byte) {
	seq := le.Uint16(m[2:])
	switch m[0] {
	case 0: // error
		x.wmu.Lock()
		ch := x.waiters[seq]
		delete(x.waiters, seq)
		x.wmu.Unlock()
		err := fmt.Errorf("x11: error %d for request %d.%d", m[1], m[10], le.Uint16(m[8:]))
		if ch != nil {
			ch <- xreply{err: err}
			return
		}
		switch {
		case x.xvOp != 0 && m[10] == x.xvOp:
			x.xvFailed.Store(true)
			x.d.e.invalidate()
		case x.renderOp != 0 && m[10] == x.renderOp:
			x.renderFailed.Store(true)
			x.d.e.invalidate()
		default:
			x.d.e.fail(err)
		}
	case 1: // reply
		x.wmu.Lock()
		if x.syncWait && seq == x.syncSeq {
			x.syncWait = false
			at := x.syncAt
			x.wmu.Unlock()
			x.d.e.presented(at, now())
			x.d.e.frameDone()
			return
		}
		ch := x.waiters[seq]
		delete(x.waiters, seq)
		x.wmu.Unlock()
		if ch != nil {
			ch <- xreply{data: append([]byte(nil), m...)}
		}
	default:
		x.event(m)
	}
}

func (x *x11) event(m []byte) {
	switch m[0] & 0x7f {
	case 12: // Expose
		if le.Uint16(m[16:]) == 0 {
			x.d.e.redraw()
		}
	case 22: // ConfigureNotify
		w, h := int(le.Uint16(m[20:])), int(le.Uint16(m[22:]))
		x.winW.Store(int32(w))
		x.winH.Store(int32(h))
		if x.d.e.resized(w, h) {
			x.send(newReq(61, 0).u32(x.win).u16(0, 0, 0, 0)) // ClearArea
			x.wmu.Lock()
			scaled := x.path == x11PathShmPut || x.path == x11PathPutImage
			x.wmu.Unlock()
			if scaled {
				x.d.e.invalidate()
			}
			x.d.e.redraw()
		}
	case 33: // ClientMessage
		if le.Uint32(m[8:]) == x.wmProtocols && le.Uint32(m[12:]) == x.wmDelete {
			x.d.e.events.push(Event{Kind: EventClose})
		}
	}
}

func (x *x11) atom(name string) (uint32, error) {
	r, err := x.call(newReq(16, 0).u16(len(name), 0).str(name))
	if err != nil {
		return 0, err
	}
	return le.Uint32(r[8:]), nil
}

func (x *x11) extension(name string) (major, firstError byte, ok bool) {
	r, err := x.call(newReq(98, 0).u16(len(name), 0).str(name))
	if err != nil || r[8] == 0 {
		return 0, 0, false
	}
	return r[9], r[11], true
}

func (x *x11) setup(o Options) error {
	x.win, x.gc = x.newID(), x.newID()
	cmap := x.newID()
	if err := x.check(newReq(78, 0).u32(cmap, x.root, x.visual)); err != nil {
		return err
	}
	const mask = 2 | 8 | 0x800 | 0x2000 // back pixel, border pixel, event mask, colormap
	const events = 1<<15 | 1<<17        // Exposure, StructureNotify
	err := x.check(newReq(1, 24).u32(x.win, x.root).u16(0, 0, o.Width, o.Height, 0, 1).
		u32(x.visual, mask, 0, 0, events, cmap))
	if err != nil {
		return err
	}
	if err := x.check(newReq(55, 0).u32(x.gc, x.win, 1<<16, 0)); err != nil { // no GraphicsExposures
		return err
	}
	var aerr error
	atom := func(name string) uint32 {
		a, err := x.atom(name)
		if err != nil {
			aerr = err
		}
		return a
	}
	x.wmProtocols, x.wmDelete = atom("WM_PROTOCOLS"), atom("WM_DELETE_WINDOW")
	netName, utf8 := atom("_NET_WM_NAME"), atom("UTF8_STRING")
	state, full := atom("_NET_WM_STATE"), atom("_NET_WM_STATE_FULLSCREEN")
	if aerr != nil {
		return aerr
	}
	prop := func(p, typ uint32, format byte, count int, data func(*xreq)) {
		r := newReq(18, 0).u32(x.win, p, typ).u8(format, 0, 0, 0).u32(uint32(count))
		data(r)
		x.send(r)
	}
	text := func(r *xreq) { r.str(o.Title) }
	prop(39, 31, 8, len(o.Title), text) // WM_NAME, STRING
	prop(netName, utf8, 8, len(o.Title), text)
	prop(x.wmProtocols, 4, 32, 1, func(r *xreq) { r.u32(x.wmDelete) })
	if o.Fullscreen {
		prop(state, 4, 32, 1, func(r *xreq) { r.u32(full) })
	}
	x.detectExtensions()
	return x.check(newReq(8, 0).u32(x.win)) // MapWindow
}

func (x *x11) detectExtensions() {
	if op, errBase, ok := x.extension("MIT-SHM"); ok && x11Disable&x11NoShm == 0 {
		r, err := x.call(newReq(op, 0))
		// AttachFd needs version 1.2.
		if err == nil && (le.Uint16(r[8:]) > 1 || le.Uint16(r[10:]) >= 2) {
			x.shmOp, x.shmErrBase = op, errBase
		}
	}
	if x.shmOp == 0 {
		return
	}
	if op, _, ok := x.extension("RENDER"); ok && x11Disable&x11NoRender == 0 {
		x.setupRender(op)
	}
	if op, _, ok := x.extension("XVideo"); ok && x11Disable&x11NoXV == 0 {
		x.setupXV(op)
	}
}

func (x *x11) setupRender(op byte) {
	if _, err := x.call(newReq(op, 0).u32(0, 11)); err != nil {
		return
	}
	r, err := x.call(newReq(op, 1))
	if err != nil {
		return
	}
	n := int(le.Uint32(r[8:]))
	for i := 0; i < n; i++ {
		f := r[32+28*i:]
		if f[4] == 1 && f[5] == 24 && le.Uint16(f[8:]) == 16 && le.Uint16(f[10:]) == 255 &&
			le.Uint16(f[12:]) == 8 && le.Uint16(f[16:]) == 0 && le.Uint16(f[22:]) == 0 {
			x.pictFormat = le.Uint32(f)
			break
		}
	}
	if x.pictFormat == 0 {
		return
	}
	x.pictWindow = x.newID()
	if x.check(newReq(op, 4).u32(x.pictWindow, x.win, x.pictFormat, 0)) == nil {
		x.renderOp = op
	}
}

func (x *x11) setupXV(op byte) {
	r, err := x.call(newReq(op, 1).u32(x.win))
	if err != nil {
		return
	}
	count := int(le.Uint16(r[8:]))
	p := r[32:]
	for a := 0; a < count; a++ {
		base, nameLen := le.Uint32(p), int(le.Uint16(p[4:]))
		ports, formats, kind := int(le.Uint16(p[6:])), int(le.Uint16(p[8:])), p[10]
		q := p[12+pad4(nameLen):]
		usable := false
		for i := 0; i < formats; i++ {
			usable = usable || le.Uint32(q[8*i:]) == x.visual
		}
		p = q[8*formats:]
		if !usable || kind&0x10 == 0 { // XvImageMask
			continue
		}
		for port := base; port < base+uint32(ports); port++ {
			g, err := x.call(newReq(op, 3).u32(port, 0))
			if err != nil || g[1] != 0 {
				continue
			}
			if x.useXVPort(op, port) {
				return
			}
		}
	}
}

func (x *x11) useXVPort(op byte, port uint32) bool {
	r, err := x.call(newReq(op, 16).u32(port))
	if err != nil {
		return false
	}
	ids := map[uint32]bool{}
	for i, n := 0, int(le.Uint32(r[8:])); i < n; i++ {
		id := le.Uint32(r[32+128*i:])
		if id == fourccNV12 || id == fourccYV12 || id == fourccI420 {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return false
	}
	x.xvOp, x.xvPort, x.xvIDs = op, port, ids
	if a, err := x.atom("XV_COLORSPACE"); err == nil {
		if _, err := x.call(newReq(op, 14).u32(port, a)); err == nil {
			x.xvColorspace = a
		}
	}
	return true
}

func (x *x11) canXV(l layout) bool {
	if x.xvOp == 0 || x.xvFailed.Load() || l.full || l.width > 8192 || l.height > 8192 {
		return false // XVideo has no way to say the samples are full range
	}
	return l.matrix == BT601 || x.xvColorspace != 0
}

// prepare implements backend.
func (x *x11) prepare(l layout) (int, error) {
	x.releasePath()
	x.l = l
	if x.canXV(l) {
		if err := x.prepareXV(l); err == nil {
			return 3, nil
		}
		x.xvFailed.Store(true)
		x.releasePath()
	}
	if x.renderOp != 0 && !x.renderFailed.Load() {
		if err := x.prepareRender(l); err == nil {
			return 3, nil
		}
		x.renderFailed.Store(true)
		x.releasePath()
	}
	return 3, x.prepareCPU(l)
}

func (x *x11) setPath(p x11Path, conv bool) {
	x.wmu.Lock()
	x.path, x.converted = p, conv
	x.lastSrc, x.lastDst = rect{}, rect{}
	x.wmu.Unlock()
}

func (x *x11) attachShm(size int) error {
	m, err := newShmem(size, false)
	if err != nil {
		return err
	}
	seg := x.newID()
	if err := x.checkFD(newReq(x.shmOp, 6).u32(seg).u8(0, 0, 0, 0), m.fd); err != nil {
		m.free()
		return err
	}
	x.mem, x.seg = m, seg
	return nil
}

func (x *x11) prepareXV(l layout) error {
	order := []uint32{fourccI420, fourccYV12, fourccNV12}
	if l.format == gaal.NV12 {
		order = []uint32{fourccNV12, fourccI420, fourccYV12}
	}
	var id uint32
	for _, c := range order {
		if x.xvIDs[c] {
			id = c
			break
		}
	}
	r, err := x.call(newReq(x.xvOp, 17).u32(x.xvPort, id).u16(l.width, l.height))
	if err != nil {
		return err
	}
	planes, size := int(le.Uint32(r[8:])), int(le.Uint32(r[12:]))
	want := 3
	if id == fourccNV12 {
		want = 2
	}
	if planes != want || size <= 0 || len(r) < 32+8*planes {
		return errors.New("unexpected XVideo image layout")
	}
	for i := 0; i < planes; i++ {
		x.xvPitch[i] = int(le.Uint32(r[32+4*i:]))
		x.xvOffset[i] = int(le.Uint32(r[32+4*planes+4*i:]))
	}
	if x.xvOffset[0] != 0 || x.xvPitch[0] < l.width {
		return errors.New("unexpected XVideo image layout")
	}
	x.xvID = id
	x.bufSize = (size + 4095) &^ 4095
	if err := x.attachShm(3 * x.bufSize); err != nil {
		return err
	}
	if x.xvColorspace != 0 {
		v := uint32(0)
		if l.matrix == BT709 {
			v = 1
		}
		if err := x.check(newReq(x.xvOp, 13).u32(x.xvPort, x.xvColorspace, v)); err != nil {
			return err
		}
	}
	x.setPath(x11PathXV, false)
	return nil
}

func (x *x11) prepareRender(l layout) error {
	x.stride = 4 * l.width
	x.bufW, x.bufH = l.width, l.height
	x.bufSize = (x.stride*l.height + 4095) &^ 4095
	if err := x.attachShm(3 * x.bufSize); err != nil {
		return err
	}
	bilinear := "bilinear"
	for i := range x.pixmaps {
		pm, pic := x.newID(), x.newID()
		err := x.check(newReq(x.shmOp, 5).u32(pm, x.win).u16(l.width, l.height).
			u8(24, 0, 0, 0).u32(x.seg, uint32(i*x.bufSize)))
		if err != nil {
			return err
		}
		x.pixmaps[i] = pm
		// CPRepeat with RepeatPad keeps the frame edges from fading out.
		err = x.check(newReq(x.renderOp, 4).u32(pic, pm, x.pictFormat, 1, 3))
		if err != nil {
			return err
		}
		x.pictures[i] = pic
		err = x.check(newReq(x.renderOp, 30).u32(pic).u16(len(bilinear), 0).str(bilinear))
		if err != nil {
			return err
		}
	}
	x.setPath(x11PathRender, true)
	return nil
}

// prepareCPU makes window-sized buffers that the frame is scaled into.
func (x *x11) prepareCPU(l layout) error {
	w, h := max(int(x.winW.Load()), 1), max(int(x.winH.Load()), 1)
	x.bufW, x.bufH, x.stride = w, h, 4*w
	x.bufSize = (x.stride*h + 4095) &^ 4095
	if w != l.width || h != l.height {
		x.staging = make([]byte, 4*l.width*l.height)
	} else {
		x.staging = nil
	}
	if x.shmOp != 0 {
		if err := x.attachShm(3 * x.bufSize); err == nil {
			x.setPath(x11PathShmPut, true)
			return nil
		}
	}
	for i := range x.heap {
		x.heap[i] = make([]byte, x.stride*h)
	}
	x.setPath(x11PathPutImage, true)
	return nil
}

func (x *x11) releasePath() {
	x.setPath(x11PathNone, false)
	for i := range x.pixmaps {
		if x.pictures[i] != 0 {
			x.send(newReq(x.renderOp, 7).u32(x.pictures[i]))
			x.pictures[i] = 0
		}
		if x.pixmaps[i] != 0 {
			x.send(newReq(54, 0).u32(x.pixmaps[i]))
			x.pixmaps[i] = 0
		}
		x.heap[i] = nil
	}
	if x.mem != nil {
		x.send(newReq(x.shmOp, 2).u32(x.seg))
		x.mem.free()
		x.mem = nil
	}
	x.staging = nil
}

// fill implements backend.
func (x *x11) fill(i int, f *gaal.VideoFrame) {
	c := x.l.coefs()
	switch x.path {
	case x11PathXV:
		b := x.mem.data[i*x.bufSize : (i+1)*x.bufSize]
		ys := x.xvPitch[0]
		switch x.xvID {
		case fourccNV12:
			toNV12(b, ys, b[x.xvOffset[1]:], x.xvPitch[1], f)
		case fourccYV12:
			toPlanar(b, ys, b[x.xvOffset[2]:], b[x.xvOffset[1]:], x.xvPitch[1], f)
		default:
			toPlanar(b, ys, b[x.xvOffset[1]:], b[x.xvOffset[2]:], x.xvPitch[1], f)
		}
	case x11PathRender:
		toBGRX(x.mem.data[i*x.bufSize:(i+1)*x.bufSize], x.stride, f, c)
	case x11PathShmPut, x11PathPutImage:
		var dst []byte
		if x.path == x11PathShmPut {
			dst = x.mem.data[i*x.bufSize : (i+1)*x.bufSize]
		} else {
			dst = x.heap[i]
		}
		if x.staging == nil {
			toBGRX(dst, x.stride, f, c)
			return
		}
		toBGRX(x.staging, 4*f.Width, f, c)
		sr, dr := place(x.d.e.opts.Scale, f.Width, f.Height, x.bufW, x.bufH)
		scaleBGRX(dst, x.stride, x.bufW, x.bufH, x.staging, 4*f.Width, sr, dr)
	}
}

func put16(b []byte, v ...int) {
	for i, n := range v {
		le.PutUint16(b[2*i:], uint16(n))
	}
}

func put32(b []byte, v ...uint32) {
	for i, n := range v {
		le.PutUint32(b[4*i:], n)
	}
}

// grow extends b by n zeroed bytes and returns the new part.
func (w *wbuf) grow(n int) []byte {
	s := len(w.b)
	if cap(w.b) < s+n {
		nb := make([]byte, s, 2*(s+n))
		copy(nb, w.b)
		w.b = nb
	}
	w.b = w.b[:s+n]
	p := w.b[s:]
	clear(p)
	return p
}

func header(b []byte, op, detail byte) {
	b[0], b[1] = op, detail
	le.PutUint16(b[2:], uint16(len(b)/4))
}

// submit implements backend.
func (x *x11) submit(i int) error {
	if x.dead.Load() {
		return ErrClosed
	}
	ww, wh := int(x.winW.Load()), int(x.winH.Load())
	shownAt := x.d.e.submittedAt()
	x.wmu.Lock()
	defer x.wmu.Unlock()
	if x.path == x11PathNone {
		return errors.New("x11: no presentation path")
	}
	w := &x.frame
	w.b = w.b[:0]
	sr, dr := place(x.d.e.opts.Scale, x.l.width, x.l.height, ww, wh)
	n := uint16(0) // requests in this batch
	switch x.path {
	case x11PathXV:
		p := w.grow(52)
		header(p, x.xvOp, 19) // XvShmPutImage
		put32(p[4:], x.xvPort, x.win, x.gc, x.seg, x.xvID, uint32(i*x.bufSize))
		put16(p[28:], sr.x, sr.y, sr.w, sr.h, dr.x, dr.y, dr.w, dr.h, x.l.width, x.l.height)
		n++
	case x11PathRender:
		p := w.grow(44)
		header(p, x.renderOp, 28) // SetPictureTransform
		sx := (int64(sr.w) << 16) / int64(max(dr.w, 1))
		sy := (int64(sr.h) << 16) / int64(max(dr.h, 1))
		put32(p[4:], x.pictures[i], uint32(sx), 0, uint32(sr.x<<16), 0, uint32(sy), uint32(sr.y<<16), 0, 0, 1<<16)
		p = w.grow(36)
		header(p, x.renderOp, 8) // Composite
		p[4] = 1                 // PictOpSrc
		put32(p[8:], x.pictures[i], 0, x.pictWindow)
		put16(p[20:], 0, 0, 0, 0, dr.x, dr.y, dr.w, dr.h)
		n += 2
	case x11PathShmPut:
		p := w.grow(40)
		header(p, x.shmOp, 3) // ShmPutImage
		put32(p[4:], x.win, x.gc)
		put16(p[12:], x.bufW, x.bufH, 0, 0, x.bufW, x.bufH, 0, 0)
		p[28], p[29] = 24, 2 // depth, ZPixmap
		put32(p[32:], x.seg, uint32(i*x.bufSize))
		n++
	case x11PathPutImage:
		lines := max(1, (x.maxRequest-24)/x.stride)
		for y := 0; y < x.bufH; y += lines {
			c := min(lines, x.bufH-y)
			p := w.grow(24 + c*x.stride)
			header(p, 72, 2) // PutImage, ZPixmap
			put32(p[4:], x.win, x.gc)
			put16(p[12:], x.bufW, c, 0, y)
			p[21] = 24
			copy(p[24:], x.heap[i][y*x.stride:(y+c)*x.stride])
			n++
		}
	}
	p := w.grow(4)
	header(p, 43, 0) // GetInputFocus, whose reply says the server got this far
	n++
	x.seq += n
	x.syncSeq, x.syncAt, x.syncWait = x.seq, shownAt, true
	_, err := x.conn.Write(w.b)
	return err
}

// info implements backend.
func (x *x11) info() Info {
	x.wmu.Lock()
	defer x.wmu.Unlock()
	path := "none"
	switch x.path {
	case x11PathXV:
		path = map[uint32]string{fourccNV12: "xv-nv12", fourccYV12: "xv-yv12", fourccI420: "xv-i420"}[x.xvID]
	case x11PathRender:
		path = "shm-render"
	case x11PathShmPut:
		path = "shm-put"
	case x11PathPutImage:
		path = "putimage"
	}
	return Info{Backend: "x11", Path: path, Converted: x.converted, LatencySource: "server"}
}

// readback returns the window content.
func (x *x11) readback() (*image.RGBA, ReadbackSource, error) {
	w, h := int(x.winW.Load()), int(x.winH.Load())
	r, err := x.call(newReq(73, 2).u32(x.win).u16(0, 0, w, h).u32(0xffffffff))
	if err != nil {
		return nil, "", err
	}
	return bgrxToRGBA(r[32:], 4*w, w, h), ReadbackWindow, nil
}

// close implements backend.
func (x *x11) close() error {
	x.send(newReq(4, 0).u32(x.win)) // DestroyWindow
	// A round trip makes sure the server saw it before the socket goes away.
	x.call(newReq(43, 0))
	err := x.conn.Close()
	x.wmu.Lock()
	x.path = x11PathNone
	x.wmu.Unlock()
	if x.mem != nil {
		x.mem.free()
		x.mem = nil
	}
	return err
}
