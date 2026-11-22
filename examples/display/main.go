// Command display opens a window and shows a moving test pattern.
//
//	go run ./examples/display -seconds 5 -size 1280x720
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/display"
)

func main() {
	seconds := flag.Float64("seconds", 5, "how long to run")
	size := flag.String("size", "1280x720", "frame size")
	backend := flag.String("backend", "", "force a display backend")
	flag.Parse()
	var w, h int
	if _, err := fmt.Sscanf(*size, "%dx%d", &w, &h); err != nil {
		log.Fatalf("bad size %q", *size)
	}
	// Main keeps the main thread for the window system, which macOS requires.
	display.Main(func() { run(w, h, *backend, time.Duration(*seconds*float64(time.Second))) })
}

func run(w, h int, backend string, length time.Duration) {
	d, err := display.Open(display.Options{Title: "gaal display", Width: w, Height: h, Backend: backend})
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	f := gaal.NewVideoFrame(w, h, gaal.NV12)
	f.Range = gaal.RangeLimited
	start := time.Now()
	tick := time.NewTicker(time.Second / 60)
	defer tick.Stop()
	for n := 0; time.Since(start) < length && !d.CloseRequested(); n++ {
		draw(f, n)
		if err := d.Show(f); err != nil {
			log.Fatal(err)
		}
		for {
			ev, ok := d.PollEvent()
			if !ok {
				break
			}
			if ev.Kind == display.EventResize {
				log.Printf("window is now %dx%d", ev.Width, ev.Height)
			}
		}
		<-tick.C
	}
	s := d.Stats()
	in := d.Info()
	log.Printf("%s/%s: shown %d, presented %d, dropped %d, Show avg %v, latency (%s) avg %v",
		in.Backend, in.Path, s.Shown, s.Presented, s.Dropped, s.ShowAvg, in.LatencySource, s.LatencyAvg)
}

func draw(f *gaal.VideoFrame, n int) {
	for y := 0; y < f.Height; y++ {
		row := f.Y[y*f.YStride : y*f.YStride+f.Width]
		for x := range row {
			row[x] = byte(16 + (x+y+4*n)%200)
		}
	}
	for y := 0; y < f.Height/2; y++ {
		row := f.U[y*f.UVStride : y*f.UVStride+f.Width&^1]
		for x := 0; x+1 < len(row); x += 2 {
			row[x], row[x+1] = byte(64+(x+n)%128), byte(64+(y+n)%128)
		}
	}
}
