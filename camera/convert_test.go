package camera

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"

	"github.com/aep/gaal"
)

// A 4x2 picture: luma 10+i, U 100.., V 200.. per chroma sample.

func checkFrame(t *testing.T, f *gaal.VideoFrame, format gaal.PixelFormat, y []byte, u, v []byte) {
	t.Helper()
	if f.Format != format || f.Width != 4 || f.Height != 2 {
		t.Fatalf("got %v %dx%d", f.Format, f.Width, f.Height)
	}
	for row := range 2 {
		if got := f.Y[row*f.YStride : row*f.YStride+4]; !bytes.Equal(got, y[row*4:row*4+4]) {
			t.Fatalf("luma row %d: %v, want %v", row, got, y[row*4:row*4+4])
		}
	}
	if format == gaal.NV12 {
		got := f.U[:4]
		want := []byte{u[0], v[0], u[1], v[1]}
		if !bytes.Equal(got, want) {
			t.Fatalf("chroma %v, want %v", got, want)
		}
		return
	}
	if !bytes.Equal(f.U[:2], u) || !bytes.Equal(f.V[:2], v) {
		t.Fatalf("chroma %v %v, want %v %v", f.U[:2], f.V[:2], u, v)
	}
}

var lumaWant = []byte{10, 11, 12, 13, 14, 15, 16, 17}

func TestConvertPacked422(t *testing.T) {
	var pool gaal.FramePool
	// Two rows of YUYV; row 1 chroma differs so that averaging shows.
	yuyv := []byte{
		10, 100, 11, 200, 12, 102, 13, 202,
		14, 104, 15, 204, 16, 106, 17, 206,
	}
	order := map[uint32][4]int{pixYUYV: {0, 1, 2, 3}, pixUYVY: {1, 0, 3, 2}, pixYVYU: {0, 3, 2, 1}}
	for format, o := range order {
		// Lay the same samples out in this format's order.
		data := make([]byte, len(yuyv))
		for m := 0; m < len(yuyv); m += 4 {
			data[m+o[0]], data[m+o[1]], data[m+o[2]], data[m+o[3]] = yuyv[m], yuyv[m+1], yuyv[m+2], yuyv[m+3]
		}
		f, err := convert(&rawFrame{format: format, width: 4, height: 2, planes: [][]byte{data}}, &pool)
		if err != nil {
			t.Fatal(fourccString(format), err)
		}
		checkFrame(t, f, gaal.I420, lumaWant, []byte{102, 104}, []byte{202, 204})
		f.Release()
	}
}

func TestConvertSemiPlanar(t *testing.T) {
	var pool gaal.FramePool
	nv12 := append(append([]byte{}, lumaWant...), 100, 200, 101, 201)
	nv21 := append(append([]byte{}, lumaWant...), 200, 100, 201, 101)
	for _, c := range []struct {
		format uint32
		planes [][]byte
	}{
		{pixNV12, [][]byte{nv12}},
		{pixNV21, [][]byte{nv21}},
		{pixNV12M, [][]byte{lumaWant, nv12[8:]}},
		{pixNV21M, [][]byte{lumaWant, nv21[8:]}},
	} {
		f, err := convert(&rawFrame{format: c.format, width: 4, height: 2, planes: c.planes}, &pool)
		if err != nil {
			t.Fatal(fourccString(c.format), err)
		}
		checkFrame(t, f, gaal.NV12, lumaWant, []byte{100, 101}, []byte{200, 201})
		f.Release()
	}
}

func TestConvertPlanar(t *testing.T) {
	var pool gaal.FramePool
	i420 := append(append([]byte{}, lumaWant...), 100, 101, 200, 201)
	yv12 := append(append([]byte{}, lumaWant...), 200, 201, 100, 101)
	for _, c := range []struct {
		format uint32
		planes [][]byte
	}{
		{pixYUV420, [][]byte{i420}},
		{pixYVU420, [][]byte{yv12}},
		{pixYUV42M, [][]byte{lumaWant, {100, 101}, {200, 201}}},
	} {
		f, err := convert(&rawFrame{format: c.format, width: 4, height: 2, planes: c.planes}, &pool)
		if err != nil {
			t.Fatal(fourccString(c.format), err)
		}
		checkFrame(t, f, gaal.I420, lumaWant, []byte{100, 101}, []byte{200, 201})
		f.Release()
	}
}

func TestConvertGrey(t *testing.T) {
	var pool gaal.FramePool
	f, err := convert(&rawFrame{format: pixGREY, width: 4, height: 2, planes: [][]byte{lumaWant}}, &pool)
	if err != nil {
		t.Fatal(err)
	}
	checkFrame(t, f, gaal.I420, lumaWant, []byte{128, 128}, []byte{128, 128})
	f.Release()
}

// Strides wider than the picture are honoured, and short buffers refused.
func TestConvertStrideAndShort(t *testing.T) {
	var pool gaal.FramePool
	padded := []byte{10, 11, 12, 13, 0, 0, 14, 15, 16, 17, 0, 0, 100, 200, 101, 201, 0, 0}
	f, err := convert(&rawFrame{format: pixNV12, width: 4, height: 2, planes: [][]byte{padded}, strides: []int{6}}, &pool)
	if err != nil {
		t.Fatal(err)
	}
	checkFrame(t, f, gaal.NV12, lumaWant, []byte{100, 101}, []byte{200, 201})
	f.Release()
	for _, format := range []uint32{pixYUYV, pixNV12, pixYUV420, pixGREY} {
		if _, err := convert(&rawFrame{format: format, width: 4, height: 2, planes: [][]byte{{1, 2, 3}}}, &pool); err == nil {
			t.Fatalf("%s: short frame accepted", fourccString(format))
		}
	}
}

func TestConvertAllocs(t *testing.T) {
	var pool gaal.FramePool
	data := make([]byte, 640*480*2)
	raw := &rawFrame{format: pixYUYV, width: 640, height: 480, planes: [][]byte{data}}
	n := testing.AllocsPerRun(50, func() {
		f, err := convert(raw, &pool)
		if err != nil {
			t.Fatal(err)
		}
		f.Release()
	})
	if n != 0 {
		t.Fatalf("%v allocations per frame", n)
	}
}

func TestJPEGWhole(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	whole := buf.Bytes()
	if !jpegWhole(whole) {
		t.Fatal("whole image refused")
	}
	if !jpegWhole(append(append([]byte{}, whole...), 0, 0, 0)) {
		t.Fatal("zero padding refused")
	}
	if jpegWhole(whole[:len(whole)-10]) {
		t.Fatal("cut image accepted")
	}
	if jpegWhole([]byte{0xFF, 0xD9}) {
		t.Fatal("marker alone accepted")
	}
}
