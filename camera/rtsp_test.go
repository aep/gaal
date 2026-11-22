package camera

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/rtsp"
	"github.com/aep/gaal/rtsp/rtsptest"
)

// jpegScan returns the entropy coded part of a JPEG and its quantization tables.
func jpegScan(t *testing.T, b []byte) (scan, tables []byte) {
	t.Helper()
	for i := 2; i+4 <= len(b); {
		m := b[i+1]
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		seg := b[i+4 : i+2+n]
		if m == 0xDB {
			for len(seg) >= 65 {
				tables = append(tables, seg[1:65]...)
				seg = seg[65:]
			}
		}
		if m == 0xDA {
			return b[i+2+n : len(b)-2], tables
		}
		i += 2 + n
	}
	t.Fatal("no scan data")
	return nil, nil
}

// TestRTSPCameraDecodesMJPEG runs the whole path over loopback with the pure Go
// JPEG decoder, including recovery after a lost packet.
func TestRTSPCameraDecodesMJPEG(t *testing.T) {
	img := image.NewYCbCr(image.Rect(0, 0, 64, 48), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = byte(40 + i%150)
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 128, 128
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	scan, tables := jpegScan(t, enc.Bytes())
	packets := func(seq *uint16, ts uint32, skipSecond bool) [][]byte {
		var out [][]byte
		for off, i := 0, 0; off < len(scan); i++ {
			n := min(200, len(scan)-off)
			p := make([]byte, 12, 300+n)
			p[0], p[1] = 0x80, 26
			if off+n == len(scan) {
				p[1] |= 0x80
			}
			binary.BigEndian.PutUint16(p[2:], *seq)
			binary.BigEndian.PutUint32(p[4:], ts)
			*seq++
			p = append(p, 0, byte(off>>16), byte(off>>8), byte(off), 1, 255, 8, 6)
			if off == 0 {
				p = append(p, 0, 0, 0, 128)
				p = append(p, tables...)
			}
			p = append(p, scan[off:off+n]...)
			off += n
			if !(skipSecond && i == 1) {
				out = append(out, p)
			}
		}
		return out
	}
	srv := &rtsptest.Server{SDP: "v=0\r\ns=x\r\nt=0 0\r\nm=video 0 RTP/AVP 26\r\na=control:t1\r\n"}
	srv.OnPlay = func(st *rtsptest.Stream) {
		seq, ts := uint16(0), uint32(0)
		for n := 0; ; n++ {
			for _, p := range packets(&seq, ts, n == 3) {
				if st.WriteRTP(0, p) != nil {
					return
				}
			}
			ts += 3600
			select {
			case <-st.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cam, err := OpenRTSP(ctx, RTSPConfig{
		RTSP:    rtsp.Config{URL: srv.URL(), Transport: rtsp.TransportTCP},
		Library: gaal.New(gaal.Options{Backend: "go-image-jpeg"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	for i := 0; i < 10; i++ {
		f, tm, err := cam.NextTimed(5 * time.Second)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Width != 64 || f.Height != 48 {
			t.Fatalf("frame size %dx%d", f.Width, f.Height)
		}
		// Latency can be zero where the clock is coarser than the decode.
		if tm.Arrival.IsZero() || tm.Latency < 0 || tm.Latency > 2*time.Second {
			t.Fatalf("timing %+v", tm)
		}
		f.Release()
	}
	s := cam.Stats()
	if s.Frames != 10 || s.Client.Tracks[0].PacketsLost != 1 {
		t.Fatalf("%+v", s)
	}
	if s.Client.Tracks[0].UnitsDropped != 1 {
		t.Fatalf("dropped units: %+v", s.Client.Tracks[0])
	}
}
