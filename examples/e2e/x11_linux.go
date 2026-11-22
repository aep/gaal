//go:build linux && !android

package e2e

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/aep/gaal"
)

// X11 is a window that speaks the X protocol directly over its socket.
type X11 struct {
	conn         net.Conn
	win, gc      uint32
	depth        byte
	maxBytes     int
	w, h, stride int
	pixels       []byte
	req          []byte
}

var le = binary.LittleEndian

func pad4(n int) int { return (n + 3) &^ 3 }

func xauth(display string) []byte {
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

// NewX11 opens a window of the given size on $DISPLAY.
func NewX11(w, h int, title string) (*X11, error) {
	d := strings.TrimPrefix(os.Getenv("DISPLAY"), ":")
	if i := strings.IndexByte(d, '.'); i >= 0 {
		d = d[:i]
	}
	if d == "" {
		return nil, errors.New("x11: DISPLAY is not set")
	}
	conn, err := net.Dial("unix", "/tmp/.X11-unix/X"+d)
	if err != nil {
		return nil, err
	}
	x := &X11{conn: conn, w: w, h: h, stride: 4 * w, pixels: make([]byte, 4*w*h)}
	if err := x.setup(d, title); err != nil {
		conn.Close()
		return nil, err
	}
	return x, nil
}

func (x *X11) setup(display, title string) error {
	const authName = "MIT-MAGIC-COOKIE-1"
	cookie := xauth(display)
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
		return fmt.Errorf("x11: connection refused: %s", info[:min(int(head[1]), len(info))])
	}
	idBase, maxLen := le.Uint32(info[4:]), int(le.Uint16(info[18:]))
	x.maxBytes = maxLen * 4
	vendor, formats := int(le.Uint16(info[16:])), int(info[21])
	s := info[32+pad4(vendor)+8*formats:]
	root, rootVisual, rootDepth, depths := le.Uint32(s), le.Uint32(s[32:]), s[38], int(s[39])
	visual, colormap := uint32(0), uint32(0)
	p := s[40:]
	for i := 0; i < depths && visual == 0; i++ {
		depth, count := p[0], int(le.Uint16(p[2:]))
		for j := 0; j < count; j++ {
			v := p[8+24*j:]
			if depth == 24 && v[4] == 4 && le.Uint32(v[8:]) == 0xff0000 && le.Uint32(v[16:]) == 0xff {
				visual = le.Uint32(v)
				break
			}
		}
		p = p[8+24*count:]
	}
	if visual == 0 {
		return errors.New("x11: no 24-bit true colour visual")
	}
	x.depth = 24
	x.win, x.gc = idBase+1, idBase+2
	var buf []byte
	u16 := func(v int) { buf = le.AppendUint16(buf, uint16(v)) }
	u32 := func(v uint32) { buf = le.AppendUint32(buf, v) }
	begin := func(op, detail byte, words int) { buf = append(buf, op, detail); u16(words) }
	mask, values := uint32(2), []uint32{0} // CWBackPixel
	if visual != rootVisual || rootDepth != 24 {
		colormap = idBase + 3
		begin(78, 0, 4) // CreateColormap
		u32(colormap)
		u32(root)
		u32(visual)
		mask |= 0x2000 | 8 // CWColormap, CWBorderPixel
		values = []uint32{0, 0, colormap}
	}
	begin(1, x.depth, 8+len(values)) // CreateWindow
	u32(x.win)
	u32(root)
	u16(0)
	u16(0)
	u16(x.w)
	u16(x.h)
	u16(0)
	u16(1)
	u32(visual)
	u32(mask)
	for _, v := range values {
		u32(v)
	}
	begin(18, 0, 6+pad4(len(title))/4) // ChangeProperty WM_NAME
	u32(x.win)
	u32(39)
	u32(31)
	buf = append(buf, 8, 0, 0, 0)
	u32(uint32(len(title)))
	buf = append(buf, title...)
	buf = append(buf, make([]byte, pad4(len(title))-len(title))...)
	begin(55, 0, 4) // CreateGC
	u32(x.gc)
	u32(x.win)
	u32(0)
	begin(8, 0, 2) // MapWindow
	u32(x.win)
	_, err := x.conn.Write(buf)
	return err
}

// Show implements Display.
func (x *X11) Show(f *gaal.VideoFrame) error {
	if f.Width != x.w || f.Height != x.h {
		return fmt.Errorf("x11: frame is %dx%d, window is %dx%d", f.Width, f.Height, x.w, x.h)
	}
	ToBGRX(f, x.pixels, x.stride)
	const header = 24
	lines := max(1, (x.maxBytes-header)/x.stride)
	if cap(x.req) < header+lines*x.stride {
		x.req = make([]byte, header+lines*x.stride)
	}
	for y := 0; y < x.h; y += lines {
		n := min(lines, x.h-y)
		size := header + n*x.stride
		b := x.req[:size]
		b[0], b[1] = 72, 2 // PutImage, ZPixmap
		le.PutUint16(b[2:], uint16(size/4))
		le.PutUint32(b[4:], x.win)
		le.PutUint32(b[8:], x.gc)
		le.PutUint16(b[12:], uint16(x.w))
		le.PutUint16(b[14:], uint16(n))
		le.PutUint16(b[16:], 0)
		le.PutUint16(b[18:], uint16(y))
		b[20], b[21], b[22], b[23] = 0, x.depth, 0, 0
		copy(b[header:], x.pixels[y*x.stride:(y+n)*x.stride])
		if _, err := x.conn.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Verify implements Display by reading a region back from the window.
func (x *X11) Verify() (float64, error) {
	rw, rh := min(64, x.w), min(64, x.h)
	rx, ry := (x.w-rw)/2, (x.h-rh)/2
	var b [20]byte
	b[0], b[1] = 73, 2 // GetImage, ZPixmap
	le.PutUint16(b[2:], 5)
	le.PutUint32(b[4:], x.win)
	le.PutUint16(b[8:], uint16(rx))
	le.PutUint16(b[10:], uint16(ry))
	le.PutUint16(b[12:], uint16(rw))
	le.PutUint16(b[14:], uint16(rh))
	le.PutUint32(b[16:], 0xffffffff)
	if _, err := x.conn.Write(b[:]); err != nil {
		return 0, err
	}
	var head [32]byte
	for {
		if _, err := io.ReadFull(x.conn, head[:]); err != nil {
			return 0, err
		}
		switch head[0] {
		case 0:
			return 0, fmt.Errorf("x11: request failed with error code %d", head[1])
		case 1:
			data := make([]byte, 4*int(le.Uint32(head[4:])))
			if _, err := io.ReadFull(x.conn, data); err != nil {
				return 0, err
			}
			match, total := 0, 0
			for y := 0; y < rh; y++ {
				for c := 0; c < rw; c++ {
					got := data[(y*rw+c)*4:]
					want := x.pixels[(ry+y)*x.stride+(rx+c)*4:]
					total++
					if got[0] == want[0] && got[1] == want[1] && got[2] == want[2] {
						match++
					}
				}
			}
			return float64(match) / float64(total), nil
		}
	}
}

// Close implements Display.
func (x *X11) Close() error { return x.conn.Close() }
