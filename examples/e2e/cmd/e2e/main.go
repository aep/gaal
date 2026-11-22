//go:build linux && !android

// Command e2e shows a camera through the full encode, draw and decode pipeline.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/examples/e2e"
)

func main() {
	w := flag.Int("width", 1280, "capture width")
	h := flag.Int("height", 720, "capture height")
	seconds := flag.Duration("duration", 5*time.Second, "run time")
	codec := flag.String("codec", "h264", "h264 or vp8")
	flag.Parse()
	c := gaal.H264
	if *codec == "vp8" {
		c = gaal.VP8
	}
	disp, err := e2e.NewX11(*w, *h, "gaal end-to-end")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer disp.Close()
	r, err := e2e.Run(e2e.Config{Width: *w, Height: *h, Duration: *seconds, Codec: c}, disp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed:", err)
		os.Exit(1)
	}
	fmt.Print(r)
}
