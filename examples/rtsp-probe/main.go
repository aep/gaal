// Command rtsp-probe connects to an RTSP stream, directly or through ONVIF, and
// prints stream details, frame rate, jitter, loss and decode latency.
//
// The password is read from the environment only (GAAL_RTSP_PASSWORD, or
// GAAL_TEST_ONVIF_PASSWORD), so that it does not appear in process listings:
//
//	GAAL_RTSP_PASSWORD=... rtsp-probe -onvif http://host:8000/onvif/device_service -user admin
//	GAAL_RTSP_PASSWORD=... rtsp-probe -url rtsp://host/stream -user admin -transport tcp
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/camera"
	"github.com/aep/gaal/onvif"
	"github.com/aep/gaal/rtsp"
)

func main() {
	streamURL := flag.String("url", "", "RTSP URL")
	service := flag.String("onvif", os.Getenv("GAAL_TEST_ONVIF_URL"), "ONVIF device service URL")
	user := flag.String("user", os.Getenv("GAAL_TEST_ONVIF_USER"), "user name")
	profile := flag.String("profile", "all", "ONVIF profile name or token, or all")
	duration := flag.Duration("duration", 20*time.Second, "measuring time per stream")
	transport := flag.String("transport", "auto", "auto, udp or tcp")
	backend := flag.String("backend", "", "decoder backend, empty picks the best")
	flag.Parse()

	pass := os.Getenv("GAAL_RTSP_PASSWORD")
	if pass == "" {
		pass = os.Getenv("GAAL_TEST_ONVIF_PASSWORD")
	}
	tr := rtsp.TransportAuto
	switch *transport {
	case "udp":
		tr = rtsp.TransportUDP
	case "tcp":
		tr = rtsp.TransportTCP
	case "auto":
	default:
		fatal(fmt.Errorf("unknown transport %q", *transport))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	type target struct{ name, uri string }
	var targets []target
	switch {
	case *streamURL != "":
		targets = append(targets, target{"stream", *streamURL})
	case *service != "":
		dev, err := onvif.New(onvif.Config{URL: *service, Username: *user, Password: pass})
		if err != nil {
			fatal(err)
		}
		if info, err := dev.DeviceInformation(ctx); err == nil {
			fmt.Printf("device: %s %s, firmware %s, clock offset %v\n", info.Manufacturer, info.Model, info.FirmwareVersion, dev.ClockOffset().Round(time.Second))
		} else {
			fatal(err)
		}
		profiles, err := dev.Profiles(ctx)
		if err != nil {
			fatal(err)
		}
		su, _ := url.Parse(*service)
		for _, p := range profiles {
			if *profile != "all" && !strings.EqualFold(*profile, p.Name) && *profile != p.Token {
				continue
			}
			uri, err := dev.StreamURI(ctx, p)
			if err != nil {
				fatal(err)
			}
			desc := ""
			if v := p.Video; v != nil {
				desc = fmt.Sprintf("%s %dx%d %.0f fps %d kbit/s", v.Encoding, v.Width, v.Height, v.FrameRate, v.BitrateKbps)
			}
			fmt.Printf("profile %q (Media%d): %s, %s\n", p.Name, p.Media, desc, rtsp.RedactURL(uri))
			if u, err := url.Parse(uri); err != nil || su == nil || u.Hostname() != su.Hostname() {
				fmt.Println("  skipped: the stream is not on the device host")
				continue
			}
			targets = append(targets, target{p.Name, uri})
		}
	default:
		fatal(fmt.Errorf("give -url or -onvif"))
	}

	lib := gaal.New(gaal.Options{Backend: *backend})
	for _, t := range targets {
		fmt.Printf("\n== %s\n", t.name)
		r, err := camera.ProbeRTSP(ctx, camera.RTSPConfig{
			RTSP:    rtsp.Config{URL: t.uri, Username: *user, Password: pass, Transport: tr},
			Library: lib,
		}, *duration)
		if r != nil {
			fmt.Print(r)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", rtsp.RedactText(err.Error()))
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", rtsp.RedactText(err.Error()))
	os.Exit(1)
}
