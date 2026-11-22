// Package e2e runs a live camera through two encode and decode generations with
// a drawing step between them, then shows the result.
//
//	camera -> encode -> decode -> draw line -> encode -> decode -> display
package e2e

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/camera"
)

// Display shows frames.
type Display interface {
	Show(*gaal.VideoFrame) error
	// Verify returns the fraction of sampled pixels that match the last frame,
	// read back from the display, or an error if it cannot be read back.
	Verify() (float64, error)
	Close() error
}

// Config controls a run.
type Config struct {
	Width, Height int
	Duration      time.Duration
	Codec         gaal.Codec
	Bitrate       int
	Library       *gaal.Library
}

// Stage is the average time of one pipeline step.
type Stage struct {
	Name string
	Avg  time.Duration
}

// Report is the outcome of a run.
type Report struct {
	Camera, Encoder1, Decoder1, Encoder2, Decoder2 string
	Width, Height                                  int
	Frames, Timeouts                               int
	FPS                                            float64
	LinePass                                       float64 // frames whose drawn line survived
	DisplayMatch                                   float64 // -1 when not verified
	DisplayError                                   string
	Stages                                         []Stage
	Latency, Worst                                 time.Duration
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "camera: %s\n%s %s -> %s, draw, %s -> %s\n", r.Camera, "pipeline:", r.Encoder1, r.Decoder1, r.Encoder2, r.Decoder2)
	fmt.Fprintf(&b, "frames: %d at %dx%d, %.1f fps, %d timeouts\n", r.Frames, r.Width, r.Height, r.FPS, r.Timeouts)
	fmt.Fprintf(&b, "drawn line survived in %.1f%% of frames\n", 100*r.LinePass)
	if r.DisplayMatch >= 0 {
		fmt.Fprintf(&b, "display readback matched %.1f%% of sampled pixels\n", 100*r.DisplayMatch)
	} else {
		fmt.Fprintf(&b, "display readback not verified: %s\n", r.DisplayError)
	}
	fmt.Fprintf(&b, "latency after capture: avg %v, worst %v\n", r.Latency.Round(10*time.Microsecond), r.Worst.Round(10*time.Microsecond))
	for _, s := range r.Stages {
		fmt.Fprintf(&b, "  %-10s %v\n", s.Name, s.Avg.Round(10*time.Microsecond))
	}
	return b.String()
}

const (
	lineWidth = 6
	lineY     = 250
	lineU     = 90
	lineV     = 240
)

// lineAt returns the y of the moving line at column x for frame n.
func lineAt(f *gaal.VideoFrame, n, x int) int {
	start := (n * 3) % f.Height
	end := f.Height - 1 - start
	return start + (end-start)*x/max(1, f.Width-1)
}

// DrawLine draws the moving test line into f in place.
func DrawLine(f *gaal.VideoFrame, n int) {
	for x := 0; x < f.Width; x++ {
		c := lineAt(f, n, x)
		for y := max(0, c-lineWidth/2); y < min(f.Height, c+lineWidth/2); y++ {
			f.Y[y*f.YStride+x] = lineY
			cy, cx := (y/2)*f.UVStride, x/2
			if f.Format == gaal.NV12 {
				f.U[cy+2*cx], f.U[cy+2*cx+1] = lineU, lineV
			} else {
				f.U[cy+cx], f.V[cy+cx] = lineU, lineV
			}
		}
	}
}

// LineVisible reports whether the line for frame n survived compression. It
// checks chroma, because a bright scene could pass a luma test by accident.
func LineVisible(f *gaal.VideoFrame, n int) bool {
	const samples = 64
	hits := 0
	for i := 0; i < samples; i++ {
		x := 8 + i*(f.Width-16)/samples
		y := min(max(lineAt(f, n, x), 0), f.Height-1)
		cy, cx := (y/2)*f.UVStride, x/2
		var v byte
		if f.Format == gaal.NV12 {
			v = f.U[cy+2*cx+1]
		} else {
			v = f.V[cy+cx]
		}
		if f.Y[y*f.YStride+x] > 150 && v > 170 {
			hits++
		}
	}
	return hits*10 >= samples*8
}

type timer struct {
	name  string
	total time.Duration
}

// Run executes the pipeline until cfg.Duration passes.
func Run(cfg Config, disp Display) (Report, error) {
	r := Report{DisplayMatch: -1}
	if cfg.Duration == 0 {
		cfg.Duration = 5 * time.Second
	}
	if cfg.Bitrate == 0 {
		cfg.Bitrate = 6_000_000
	}
	if cfg.Library == nil {
		cfg.Library = gaal.New(gaal.Options{})
	}
	cam, err := camera.Open(camera.Config{Width: cfg.Width, Height: cfg.Height, FPS: 30, Library: cfg.Library})
	if err != nil {
		return r, err
	}
	defer cam.Close()
	r.Camera = cam.Name()

	const wait = 2 * time.Second
	first, err := cam.Next(10 * time.Second)
	if err != nil {
		return r, fmt.Errorf("first camera frame: %w", err)
	}
	r.Width, r.Height = first.Width&^1, first.Height&^1
	first.Release()
	vc := gaal.VideoConfig{Codec: cfg.Codec, Width: r.Width, Height: r.Height, FPS: 30, Bitrate: cfg.Bitrate, KeyframeEvery: 30}
	enc1, err := cfg.Library.NewVideoEncoder(vc)
	if err != nil {
		return r, err
	}
	defer enc1.Close()
	enc2, err := cfg.Library.NewVideoEncoder(vc)
	if err != nil {
		return r, err
	}
	defer enc2.Close()
	dec1, err := cfg.Library.NewVideoDecoder(vc)
	if err != nil {
		return r, err
	}
	defer dec1.Close()
	dec2, err := cfg.Library.NewVideoDecoder(vc)
	if err != nil {
		return r, err
	}
	defer dec2.Close()

	stages := []*timer{{name: "encode 1"}, {name: "decode 1"}, {name: "draw"}, {name: "encode 2"}, {name: "decode 2"}, {name: "display"}}
	timed := func(i int, start time.Time) time.Time {
		now := time.Now()
		stages[i].total += now.Sub(start)
		return now
	}
	code := func(send func() error, recv func() error) error {
		if err := send(); err != nil {
			return err
		}
		return recv()
	}
	var total, worst time.Duration
	passed := 0
	begin := time.Now()
	for n := 0; time.Since(begin) < cfg.Duration; n++ {
		f, err := cam.Next(wait)
		if err != nil {
			return r, err
		}
		f.Width, f.Height = r.Width, r.Height
		f.ForceKeyframe = n == 0
		captured := time.Now()
		t := captured
		var p1, p2 *gaal.EncodedPacket
		var d1, d2 *gaal.VideoFrame
		err = code(func() error { return enc1.Send(f) }, func() (e error) { p1, e = gaal.ReceiveWait(enc1, wait); return })
		f.Release()
		if errors.Is(err, gaal.ErrTimeout) {
			r.Timeouts++
			continue
		}
		if err != nil {
			return r, fmt.Errorf("encode 1: %w", err)
		}
		t = timed(0, t)
		err = code(func() error { return dec1.Send(p1.Packet) }, func() (e error) { d1, e = gaal.ReceiveWait(dec1, wait); return })
		p1.Release()
		if errors.Is(err, gaal.ErrTimeout) {
			r.Timeouts++
			continue
		}
		if err != nil {
			return r, fmt.Errorf("decode 1: %w", err)
		}
		t = timed(1, t)
		DrawLine(d1, n)
		t = timed(2, t)
		d1.ForceKeyframe = n == 0
		err = code(func() error { return enc2.Send(d1) }, func() (e error) { p2, e = gaal.ReceiveWait(enc2, wait); return })
		d1.Release()
		if errors.Is(err, gaal.ErrTimeout) {
			r.Timeouts++
			continue
		}
		if err != nil {
			return r, fmt.Errorf("encode 2: %w", err)
		}
		t = timed(3, t)
		err = code(func() error { return dec2.Send(p2.Packet) }, func() (e error) { d2, e = gaal.ReceiveWait(dec2, wait); return })
		p2.Release()
		if errors.Is(err, gaal.ErrTimeout) {
			r.Timeouts++
			continue
		}
		if err != nil {
			return r, fmt.Errorf("decode 2: %w", err)
		}
		t = timed(4, t)
		if LineVisible(d2, n) {
			passed++
		}
		err = disp.Show(d2)
		d2.Release()
		if err != nil {
			return r, fmt.Errorf("display: %w", err)
		}
		timed(5, t)
		d := time.Since(captured)
		total += d
		worst = max(worst, d)
		r.Frames++
	}
	elapsed := time.Since(begin)
	r.Encoder1, r.Decoder1 = enc1.Backend().Name, dec1.Backend().Name
	r.Encoder2, r.Decoder2 = enc2.Backend().Name, dec2.Backend().Name
	if r.Frames == 0 {
		return r, errors.New("no frames completed the pipeline")
	}
	r.FPS = float64(r.Frames) / elapsed.Seconds()
	r.LinePass = float64(passed) / float64(r.Frames)
	r.Latency, r.Worst = total/time.Duration(r.Frames), worst
	for _, s := range stages {
		r.Stages = append(r.Stages, Stage{s.name, s.total / time.Duration(r.Frames)})
	}
	if m, err := disp.Verify(); err != nil {
		r.DisplayError = err.Error()
	} else {
		r.DisplayMatch = m
	}
	return r, nil
}

// ToBGRX converts f to 32-bit BGRX using BT.601 limited range.
func ToBGRX(f *gaal.VideoFrame, dst []byte, stride int) {
	clamp := func(v int) byte {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return byte(v)
	}
	for y := 0; y < f.Height; y++ {
		row := dst[y*stride : y*stride+4*f.Width]
		ys := f.Y[y*f.YStride : y*f.YStride+f.Width]
		cy := (y / 2) * f.UVStride
		for x, l := range ys {
			var u, v int
			if f.Format == gaal.NV12 {
				u, v = int(f.U[cy+x&^1]), int(f.U[cy+x&^1+1])
			} else {
				u, v = int(f.U[cy+x/2]), int(f.V[cy+x/2])
			}
			c := 298 * (int(l) - 16)
			d, e := u-128, v-128
			o := row[4*x : 4*x+4 : 4*x+4]
			o[0] = clamp((c + 516*d + 128) >> 8)
			o[1] = clamp((c - 100*d - 208*e + 128) >> 8)
			o[2] = clamp((c + 409*e + 128) >> 8)
			o[3] = 255
		}
	}
}
