//go:build amd64 || arm64

package gaal

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ohFixture is the bzip2 form of ohFixtureRaw. It is not a codec.
const (
	ohFixture    = "QlpoOTFBWSZTWXQD1swAAAdZgAAQQAAVACbFzCAgACKaaPU0ephChppgAHZBZi5hEYxmpebcWlHxdyRThQkHQD1swA=="
	ohFixtureRaw = "gaal openh264 test payload\n"
)

func ohEncoderFor(t testing.TB, cfg VideoConfig) VideoEncoder {
	t.Helper()
	e, err := New(Options{Backend: "openh264"}).NewVideoEncoder(cfg)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func ohDecoderFor(t testing.TB) VideoDecoder {
	t.Helper()
	d, err := New(Options{Backend: "openh264"}).NewVideoDecoder(VideoConfig{Codec: H264})
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// ohNALTypes lists the NAL unit types of an Annex B packet.
func ohNALTypes(b []byte) []int {
	var out []int
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			out = append(out, int(b[i+3]&0x1f))
			i += 3
		}
	}
	return out
}

func TestOpenH264Info(t *testing.T) {
	e := ohEncoderFor(t, VideoConfig{Codec: H264, Width: 64, Height: 64})
	want := BackendInfo{Name: "openh264", Codec: H264, Dir: Encode}
	if got := e.Backend(); got != want {
		t.Errorf("backend %+v", got)
	}
}

func TestOpenH264VersionCheck(t *testing.T) {
	if err := ohCheckVersion([4]uint32{ohMajor, ohMinor, 9, 0}); err != nil {
		t.Error(err)
	}
	if err := ohCheckVersion([4]uint32{ohMajor, ohOldestMinor, 0, 0}); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][4]uint32{{ohMajor, ohMinor + 1}, {ohMajor, ohOldestMinor - 1}, {ohMajor + 1, ohMinor}, {}} {
		if ohCheckVersion(v) == nil {
			t.Errorf("version %v accepted", v)
		}
	}
}

// The pinned downloads must have the ABI of the generated layout.
func TestOpenH264PinsMatchLayout(t *testing.T) {
	prefix := strconv.Itoa(ohMajor) + "." + strconv.Itoa(ohMinor) + "."
	if !strings.HasPrefix(ohVersion, prefix) {
		t.Errorf("pinned version %s, layout is %s", ohVersion, prefix)
	}
	for key, pin := range ohPins {
		if !strings.Contains(pin.file, "-"+ohVersion+"-") {
			t.Errorf("%s: %s is not version %s", key, pin.file, ohVersion)
		}
		if len(pin.bzSHA256) != 64 || len(pin.rawSHA256) != 64 || pin.bzSize == 0 || pin.rawSize == 0 {
			t.Errorf("%s: incomplete pin", key)
		}
	}
}

func TestOpenH264SteadyStateAllocs(t *testing.T) {
	for _, size := range [][2]int{{320, 240}, {1280, 720}} {
		w, h := size[0], size[1]
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			enc := ohEncoderFor(t, VideoConfig{Codec: H264, Width: w, Height: h})
			dec := ohDecoderFor(t)
			src := testFrame(w, h, 0)
			out := 0
			onFrame := func(f *VideoFrame) { out++; f.Release() }
			onPacket := func(p *EncodedPacket) {
				if err := dec.Send(p.Packet); err != nil {
					t.Fatal(err)
				}
				p.Release()
				drain(t, dec, onFrame)
			}
			step := func() {
				src.PTS += time.Millisecond
				src.Y[out%len(src.Y)]++
				if err := enc.Send(src); err != nil {
					t.Fatal(err)
				}
				drain(t, enc, onPacket)
			}
			for range 60 {
				step()
			}
			if out == 0 {
				t.Fatal("no frames decoded")
			}
			if a := testing.AllocsPerRun(200, step); a != 0 {
				t.Errorf("%.2f allocations per frame", a)
			}
		})
	}
}

func TestOpenH264Keyframes(t *testing.T) {
	const w, h = 320, 240
	enc := ohEncoderFor(t, VideoConfig{Codec: H264, Width: w, Height: h, KeyframeEvery: 1000})
	var pkts []Packet
	for i := range 12 {
		f := testFrame(w, h, i)
		f.ForceKeyframe = i == 7
		if err := enc.Send(f); err != nil {
			t.Fatal(err)
		}
		// One packet per frame, straight away, shows there is no reordering.
		p, err := enc.Receive()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if p.PTS != f.PTS {
			t.Errorf("frame %d: PTS %v, want %v", i, p.PTS, f.PTS)
		}
		pkts = append(pkts, clonePacket(p.Packet))
		p.Release()
	}
	for i, p := range pkts {
		if !bytes.HasPrefix(p.Data, []byte{0, 0, 0, 1}) {
			t.Errorf("packet %d is not Annex B", i)
		}
		types := ohNALTypes(p.Data)
		wantKey := i == 0 || i == 7
		if p.Keyframe != wantKey {
			t.Errorf("packet %d: Keyframe %v", i, p.Keyframe)
		}
		if wantKey {
			sps, pps, idr := -1, -1, -1
			for j, n := range types {
				switch {
				case n == 7 && sps < 0:
					sps = j
				case n == 8 && pps < 0:
					pps = j
				case n == 5 && idr < 0:
					idr = j
				}
			}
			if sps < 0 || pps < sps || idr < pps {
				t.Errorf("packet %d: NAL types %v", i, types)
			}
		} else {
			for _, n := range types {
				if n == 5 || n == 7 || n == 8 {
					t.Errorf("packet %d: NAL types %v", i, types)
				}
			}
		}
	}
}

func TestOpenH264StreamContract(t *testing.T) {
	const w, h, frames = 320, 240, 8
	enc := ohEncoderFor(t, VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 3_000_000})
	dec := ohDecoderFor(t)
	if _, err := enc.Receive(); err != ErrAgain {
		t.Errorf("encoder Receive: %v", err)
	}
	if _, err := dec.Receive(); err != ErrAgain {
		t.Errorf("decoder Receive: %v", err)
	}
	var got []*VideoFrame
	for i := range frames {
		src := testFrame(w, h, i)
		nv := NewVideoFrame(w, h, NV12)
		copy(nv.Y, src.Y)
		for j := range src.U {
			nv.U[2*j], nv.U[2*j+1] = src.U[j], src.V[j]
		}
		nv.PTS = src.PTS
		if err := enc.Send(nv); err != nil {
			t.Fatal(err)
		}
		clear(nv.Y) // Send must not keep the frame
		clear(nv.U)
		drain(t, enc, func(p *EncodedPacket) {
			in := clonePacket(p.Packet)
			p.Release()
			if err := dec.Send(in); err != nil {
				t.Fatal(err)
			}
			clear(in.Data) // Send must not keep the packet
			drain(t, dec, func(f *VideoFrame) { got = append(got, f) })
		})
	}
	for _, s := range []interface{ Flush() error }{enc, dec} {
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	drain(t, dec, func(f *VideoFrame) { got = append(got, f) })
	if _, err := enc.Receive(); err != io.EOF {
		t.Errorf("encoder after Flush: %v", err)
	}
	if _, err := dec.Receive(); err != io.EOF {
		t.Errorf("decoder after Flush: %v", err)
	}
	if len(got) != frames {
		t.Fatalf("decoded %d frames", len(got))
	}
	for i, f := range got {
		want := testFrame(w, h, i)
		if q := psnrY(want, f); q < 28 {
			t.Errorf("frame %d: PSNR %.1f", i, q)
		}
		if f.PTS != want.PTS {
			t.Errorf("frame %d: PTS %v, want %v", i, f.PTS, want.PTS)
		}
		f.Release()
	}
}

func TestOpenH264Corrupt(t *testing.T) {
	const w, h = 320, 240
	pkts := encodeOH(t, w, h, 10)
	garbage := bytes.Repeat([]byte{0, 0, 0, 1, 0x65, 0xff, 0x13, 0x77, 0x42}, 40)
	noise := make([]byte, 2000)
	for i := range noise {
		noise[i] = byte(i*i*31 + i>>3)
	}
	truncated := append([]byte(nil), pkts[0].Data[:len(pkts[0].Data)/3]...)
	flipped := append([]byte(nil), pkts[0].Data...)
	for i := len(flipped) / 2; i < len(flipped); i += 7 {
		flipped[i] ^= 0x5a
	}
	for name, bad := range map[string][]byte{"slice without headers": garbage, "noise": noise, "truncated": truncated, "flipped": flipped, "P without IDR": pkts[3].Data} {
		t.Run(name, func(t *testing.T) {
			dec := ohDecoderFor(t)
			err := dec.Send(Packet{Data: bad})
			if err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
			t.Logf("Send: %v", err)
			drain(t, dec, func(f *VideoFrame) { f.Release() })
			n := 0
			for _, p := range pkts {
				if err := dec.Send(p); err != nil {
					t.Fatalf("valid stream after damage: %v", err)
				}
				drain(t, dec, func(f *VideoFrame) { n++; f.Release() })
			}
			if n != len(pkts) {
				t.Errorf("decoded %d of %d frames after damage", n, len(pkts))
			}
		})
	}
	t.Run("reported", func(t *testing.T) {
		dec := ohDecoderFor(t)
		if err := dec.Send(pkts[3]); !errors.Is(err, ErrCorrupt) {
			t.Errorf("P frame without parameter sets: %v", err)
		}
		if err := dec.Send(Packet{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("empty packet: %v", err)
		}
	})
}

func encodeOH(t *testing.T, w, h, frames int) []Packet {
	t.Helper()
	if _, err := New(Options{Backend: "openh264"}).NewVideoEncoder(VideoConfig{Codec: H264, Width: w, Height: h}); err != nil {
		t.Skip(err)
	}
	return encodeClip(t, "openh264", VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 3_000_000}, frames)
}

func TestOpenH264AfterClose(t *testing.T) {
	enc := ohEncoderFor(t, VideoConfig{Codec: H264, Width: 64, Height: 64})
	dec := ohDecoderFor(t)
	enc.Close()
	dec.Close()
	if err := enc.Close(); err != nil {
		t.Error(err)
	}
	if err := enc.Send(testFrame(64, 64, 0)); !errors.Is(err, ErrClosed) {
		t.Errorf("encoder: %v", err)
	}
	if err := dec.Send(Packet{Data: []byte{1}}); !errors.Is(err, ErrClosed) {
		t.Errorf("decoder: %v", err)
	}
}

// TestOpenH264ReorderedStream decodes a stream with B-frames, which the encoder
// here never makes, using ffmpeg as the source when it is installed.
func TestOpenH264ReorderedStream(t *testing.T) {
	const frames = 48
	dir := t.TempDir()
	path := filepath.Join(dir, "b.h264")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
		"-frames:v", strconv.Itoa(frames), "-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "high",
		"-bf", "3", "-x264-params", "aud=1", "-f", "h264", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg with libx264 is not usable: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	aud := []byte{0, 0, 0, 1, 9}
	dec := ohDecoderFor(t)
	var pts []time.Duration
	got := func(f *VideoFrame) { pts = append(pts, f.PTS); f.Release() }
	n := 0
	for len(data) > 0 {
		end := bytes.Index(data[len(aud):], aud)
		au := data
		if end >= 0 {
			au = data[:end+len(aud)]
		}
		data = data[len(au):]
		if err := dec.Send(Packet{Data: au, PTS: time.Duration(n+1) * time.Millisecond}); err != nil {
			t.Fatalf("access unit %d: %v", n, err)
		}
		n++
		drain(t, dec, got)
	}
	if err := dec.Flush(); err != nil {
		t.Fatal(err)
	}
	drain(t, dec, got)
	if n != frames || len(pts) != frames {
		t.Fatalf("sent %d access units, decoded %d frames, want %d", n, len(pts), frames)
	}
	seen := map[time.Duration]bool{}
	for _, p := range pts {
		seen[p] = true
	}
	if len(seen) != frames {
		t.Errorf("only %d distinct timestamps: %v", len(seen), pts)
	}
}

// ohFake serves one file and counts requests.
type ohFake struct {
	srv  *httptest.Server
	hits atomic.Int32
	body []byte
}

// ohUseFake points the download code at a local server with a matching pin.
func ohUseFake(t *testing.T) (*ohFake, ohPin) {
	t.Helper()
	bz, err := base64.StdEncoding.DecodeString(ohFixture)
	if err != nil {
		t.Fatal(err)
	}
	f := &ohFake{body: bz}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.Write(f.body)
	}))
	t.Cleanup(f.srv.Close)
	pin := ohPin{file: "fixture.so", bzSize: int64(len(bz)), bzSHA256: sha256Hex(bz),
		rawSize: int64(len(ohFixtureRaw)), rawSHA256: sha256Hex([]byte(ohFixtureRaw))}
	ohSetPin(t, pin)
	oldURL := ohBaseURL
	ohBaseURL = f.srv.URL
	t.Cleanup(func() { ohBaseURL = oldURL })
	return f, pin
}

func ohSetPin(t *testing.T, pin ohPin) {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	old, had := ohPins[key]
	ohPins[key] = pin
	t.Cleanup(func() {
		if had {
			ohPins[key] = old
		} else {
			delete(ohPins, key)
		}
	})
}

func ohDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left in the cache: %s", e.Name())
	}
}

func TestPrepareNeedsAllowDownload(t *testing.T) {
	f, _ := ohUseFake(t)
	dir := t.TempDir()
	err := New(Options{CacheDir: dir}).Prepare(context.Background(), H264)
	if err == nil || !strings.Contains(err.Error(), "AllowDownload") {
		t.Fatalf("got %v", err)
	}
	if err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(context.Background(), VP8); err == nil {
		t.Error("Prepare(VP8) succeeded")
	}
	if n := f.hits.Load(); n != 0 {
		t.Errorf("%d requests", n)
	}
	ohDirEmpty(t, dir)
}

// Only Prepare may reach the network, even when downloads are allowed.
func TestOnlyPrepareUsesNetwork(t *testing.T) {
	f, _ := ohUseFake(t)
	l := New(Options{CacheDir: t.TempDir(), AllowDownload: true})
	l.Backends(H264, Decode)
	l.Backends(H264, Encode)
	if d, err := l.NewVideoDecoder(VideoConfig{Codec: H264}); err == nil {
		d.Close()
	}
	if e, err := l.NewVideoEncoder(VideoConfig{Codec: H264, Width: 64, Height: 64}); err == nil {
		e.Close()
	}
	if n := f.hits.Load(); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestPrepareRejectsBadData(t *testing.T) {
	skipWithoutDownloads(t)
	for name, change := range map[string]func(*ohFake, *ohPin){
		"download hash": func(_ *ohFake, p *ohPin) { p.bzSHA256 = strings.Repeat("0", 64) },
		"library hash":  func(_ *ohFake, p *ohPin) { p.rawSHA256 = strings.Repeat("0", 64) },
		"tampered":      func(f *ohFake, _ *ohPin) { f.body = append([]byte(nil), f.body...); f.body[len(f.body)-1] ^= 1 },
		"too long":      func(f *ohFake, _ *ohPin) { f.body = append(append([]byte(nil), f.body...), 0) },
		"truncated":     func(f *ohFake, _ *ohPin) { f.body = f.body[:len(f.body)-1] },
	} {
		t.Run(name, func(t *testing.T) {
			f, pin := ohUseFake(t)
			change(f, &pin)
			ohSetPin(t, pin)
			dir := filepath.Join(t.TempDir(), "cache")
			if err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(context.Background(), H264); err == nil {
				t.Fatal("Prepare succeeded")
			}
			if f.hits.Load() != 1 {
				t.Errorf("%d requests", f.hits.Load())
			}
			ohDirEmpty(t, dir)
		})
	}
}

func TestPrepareRefusesRedirect(t *testing.T) {
	f, _ := ohUseFake(t)
	other := httptest.NewServer(http.RedirectHandler(f.srv.URL+"/elsewhere", http.StatusFound))
	defer other.Close()
	ohBaseURL = other.URL
	dir := t.TempDir()
	if err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(context.Background(), H264); err == nil {
		t.Fatal("Prepare followed a redirect")
	}
	if f.hits.Load() != 0 {
		t.Error("redirect target was contacted")
	}
	ohDirEmpty(t, dir)
}

func TestPrepareHonoursContext(t *testing.T) {
	skipWithoutDownloads(t)
	ohUseFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(ctx, H264)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	ohDirEmpty(t, dir)
}

func TestPrepareStoresVerifiedFile(t *testing.T) {
	skipWithoutDownloads(t)
	f, pin := ohUseFake(t)
	dir := filepath.Join(t.TempDir(), "cache")
	l := New(Options{CacheDir: dir, AllowDownload: true})
	if err := l.Prepare(context.Background(), H264); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, pin.file)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != ohFixtureRaw {
		t.Fatalf("cached %q, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d files in the cache", len(entries))
	}
	if p, err := ohCachedFile(l); err != nil || p != path {
		t.Errorf("ohCachedFile = %q, %v", p, err)
	}
	if err := l.Prepare(context.Background(), H264); err != nil || f.hits.Load() != 1 {
		t.Errorf("second Prepare: %v, %d requests", err, f.hits.Load())
	}
	// A damaged cache entry is refused, then replaced by the next Prepare.
	if err := os.WriteFile(path, bytes.ToUpper(got), 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := ohCachedFile(l); err == nil {
		t.Errorf("damaged copy accepted at %q", p)
	}
	if err := l.Prepare(context.Background(), H264); err != nil || f.hits.Load() != 2 {
		t.Fatalf("repair: %v, %d requests", err, f.hits.Load())
	}
	if got, _ := os.ReadFile(path); string(got) != ohFixtureRaw {
		t.Errorf("repaired copy is %q", got)
	}
}

const (
	ohHelperDir = "GAAL_OH_HELPER_DIR"
	ohHelperPin = "GAAL_OH_HELPER_PIN"
)

// ohRunHelper runs TestOpenH264CacheHelper in a new process, because a process
// keeps the first OpenH264 library it loads.
func ohRunHelper(t *testing.T, dir, pin string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenH264CacheHelper$", "-test.v")
	cmd.Env = append(os.Environ(), ohHelperDir+"="+dir, ohHelperPin+"="+pin)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("cached copy works")) {
		t.Fatal("helper did not run")
	}
}

// TestOpenH264CacheHelper checks that the cached library alone provides H.264.
func TestOpenH264CacheHelper(t *testing.T) {
	dir := os.Getenv(ohHelperDir)
	if dir == "" {
		t.Skip("runs as a subprocess of other tests")
	}
	ohSystemNames = func() []string { return nil }
	if s := os.Getenv(ohHelperPin); s != "" {
		name, sum, _ := strings.Cut(s, ":")
		sum, size, _ := strings.Cut(sum, ":")
		n, _ := strconv.ParseInt(size, 10, 64)
		ohSetPin(t, ohPin{file: name, rawSHA256: sum, rawSize: n})
	}
	if bs := New(Options{CacheDir: dir}).Backends(H264, Encode); hasOH(bs) {
		t.Fatal("cached copy used without AllowDownload")
	}
	l := New(Options{CacheDir: dir, AllowDownload: true, Backend: "openh264"})
	const w, h = 320, 240
	cfg := VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 3_000_000}
	enc, err := l.NewVideoEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := l.NewVideoDecoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	n := 0
	for i := range 5 {
		if err := enc.Send(testFrame(w, h, i)); err != nil {
			t.Fatal(err)
		}
		drain(t, enc, func(p *EncodedPacket) {
			if err := dec.Send(p.Packet); err != nil {
				t.Fatal(err)
			}
			p.Release()
			drain(t, dec, func(f *VideoFrame) { n++; f.Release() })
		})
	}
	if n != 5 {
		t.Fatalf("decoded %d frames", n)
	}
	if !ohLib.api.cached {
		t.Fatal("the library in use is not the cached copy")
	}
	if runtime.GOOS == "linux" {
		maps, err := os.ReadFile("/proc/self/maps")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(maps, []byte(dir)) {
			t.Fatalf("%s is not mapped", dir)
		}
	}
	if _, err := New(Options{CacheDir: dir, Backend: "openh264"}).NewVideoEncoder(cfg); err == nil {
		t.Fatal("cached copy available after AllowDownload was cleared")
	}
	t.Logf("cached copy works: version %v", ohLib.api.version)
}

func hasOH(bs []BackendInfo) bool {
	for _, b := range bs {
		if b.Name == "openh264" {
			return true
		}
	}
	return false
}

// ohSystemLibrary returns the bytes of an installed library to act as a download.
func ohSystemLibrary(t *testing.T) []byte {
	t.Helper()
	for _, p := range []string{"/usr/lib/libopenh264.so.8", "/usr/lib64/libopenh264.so.8", "/usr/lib/x86_64-linux-gnu/libopenh264.so.8", "/usr/lib/aarch64-linux-gnu/libopenh264.so.8"} {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	t.Skip("no system libopenh264.so.8")
	return nil
}

// A good download is stored, verified again and then used for real coding.
func TestPreparedLibraryIsUsed(t *testing.T) {
	raw := ohSystemLibrary(t)
	bzip, err := exec.LookPath("bzip2")
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(bzip, "-c")
	cmd.Stdin = bytes.NewReader(raw)
	bz, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := ohUseFake(t)
	f.body = bz
	pin := ohPin{file: "libopenh264-test.so", bzSize: int64(len(bz)), bzSHA256: sha256Hex(bz), rawSize: int64(len(raw)), rawSHA256: sha256Hex(raw)}
	ohSetPin(t, pin)
	dir := t.TempDir()
	if err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(context.Background(), H264); err != nil {
		t.Fatal(err)
	}
	spec := pin.file + ":" + pin.rawSHA256 + ":" + strconv.FormatInt(pin.rawSize, 10)
	ohRunHelper(t, dir, spec)

	// The same file with one changed byte must not load.
	path := filepath.Join(dir, pin.file)
	raw[len(raw)/2] ^= 1
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestOpenH264CacheHelper$")
	cmd.Env = append(os.Environ(), ohHelperDir+"="+dir, ohHelperPin+"="+spec)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("tampered library was accepted: %s", out)
	} else if !bytes.Contains(out, []byte("SHA-256")) {
		t.Fatalf("unexpected failure: %s", out)
	}
}

// TestRealCiscoDownload contacts Cisco's server, so it only runs on request:
// GAAL_TEST_OPENH264_DOWNLOAD=1 go test -run TestRealCiscoDownload -v
func TestRealCiscoDownload(t *testing.T) {
	if os.Getenv("GAAL_TEST_OPENH264_DOWNLOAD") == "" {
		t.Skip("set GAAL_TEST_OPENH264_DOWNLOAD=1 to contact Cisco's server")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	if err := New(Options{CacheDir: dir, AllowDownload: true}).Prepare(ctx, H264); err != nil {
		t.Fatal(err)
	}
	t.Logf("downloaded and verified in %v", time.Since(start))
	ohRunHelper(t, dir, "")
}

// ohMovingFrames makes textured frames that change, unlike a static test image.
func ohMovingFrames(w, h, n int) []*VideoFrame {
	frames := make([]*VideoFrame, n)
	noise := make([]byte, w+h+n*4+64)
	v := uint32(2463534242)
	for i := range noise {
		v ^= v << 13
		v ^= v >> 17
		v ^= v << 5
		noise[i] = byte(v) & 15
	}
	for i := range frames {
		f := testFrame(w, h, i*8)
		for y := 0; y < h; y++ {
			row := f.Y[y*f.YStride : y*f.YStride+w]
			for x := range row {
				row[x] += noise[(x+y*3+i*4)%len(noise)]
			}
		}
		f.PTS = time.Duration(i) * time.Second / 30
		frames[i] = f
	}
	return frames
}

func BenchmarkOpenH2641080p(b *testing.B) {
	const w, h = 1920, 1080
	cfg := VideoConfig{Codec: H264, Width: w, Height: h, Bitrate: 8_000_000, FPS: 30}
	enc := ohEncoderFor(b, cfg)
	dec := ohDecoderFor(b)
	frames := ohMovingFrames(w, h, 30)
	var encoding, decoding time.Duration
	var size int64
	onFrame := func(f *VideoFrame) { f.Release() }
	onPacket := func(p *EncodedPacket) {
		size += int64(len(p.Data))
		start := time.Now()
		if err := dec.Send(p.Packet); err != nil {
			b.Fatal(err)
		}
		drain(b, dec, onFrame)
		decoding += time.Since(start)
		p.Release()
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		f := frames[i%len(frames)]
		f.PTS = time.Duration(i) * time.Second / 30
		i++
		start := time.Now()
		if err := enc.Send(f); err != nil {
			b.Fatal(err)
		}
		encoding += time.Since(start)
		drain(b, enc, onPacket)
	}
	n := float64(b.N)
	b.ReportMetric(float64(encoding.Microseconds())/n, "enc-µs/frame")
	b.ReportMetric(float64(decoding.Microseconds())/n, "dec-µs/frame")
	b.ReportMetric(float64(size)*8*30/n/1e6, "Mbit/s@30fps")
}

// skipWithoutDownloads skips on systems where Cisco publishes no usable binary.
func skipWithoutDownloads(t *testing.T) {
	t.Helper()
	if !downloadableH264() {
		t.Skip("OpenH264 downloads are not supported on this platform")
	}
}
