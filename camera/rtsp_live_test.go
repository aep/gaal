package camera_test

import (
	"context"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/camera"
	"github.com/aep/gaal/onvif"
	"github.com/aep/gaal/rtsp"
)

// TestLiveCamera needs GAAL_TEST_ONVIF_URL, GAAL_TEST_ONVIF_USER and
// GAAL_TEST_ONVIF_PASSWORD. It makes read-only calls, opens one stream at a
// time, and tears each one down. GAAL_TEST_RTSP_SECONDS overrides the 20 s per
// stream, and GAAL_TEST_RTSP_TRANSPORT selects udp or tcp.
func TestLiveCamera(t *testing.T) {
	service, user, pass := os.Getenv("GAAL_TEST_ONVIF_URL"), os.Getenv("GAAL_TEST_ONVIF_USER"), os.Getenv("GAAL_TEST_ONVIF_PASSWORD")
	if service == "" || user == "" || pass == "" {
		t.Skip("GAAL_TEST_ONVIF_URL, GAAL_TEST_ONVIF_USER and GAAL_TEST_ONVIF_PASSWORD are not set")
	}
	seconds := 20 * time.Second
	if v, err := time.ParseDuration(os.Getenv("GAAL_TEST_RTSP_SECONDS") + "s"); err == nil && v > 0 {
		seconds = v
	}
	transport := rtsp.TransportAuto
	switch os.Getenv("GAAL_TEST_RTSP_TRANSPORT") {
	case "udp":
		transport = rtsp.TransportUDP
	case "tcp":
		transport = rtsp.TransportTCP
	}
	su, err := url.Parse(service)
	if err != nil {
		t.Fatal("cannot parse the service URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Discover from the interface that reaches the camera.
	probe, err := net.Dial("udp4", net.JoinHostPort(su.Hostname(), "9"))
	if err != nil {
		t.Fatal(err)
	}
	local := probe.LocalAddr().(*net.UDPAddr).IP
	probe.Close()
	found, err := onvif.Discover(ctx, onvif.DiscoverOptions{Timeout: 3 * time.Second, LocalAddrs: []net.IP{local}})
	if err != nil {
		t.Fatal(err)
	}
	discovered := false
	for _, d := range found {
		discovered = discovered || d.From == su.Hostname()
	}
	if !discovered {
		t.Error("the camera did not answer WS-Discovery")
	}
	dev, err := onvif.New(onvif.Config{URL: service, Username: user, Password: pass})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := dev.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 0 {
		t.Fatal("device has no profiles")
	}
	lib := gaal.New(gaal.Options{})
	tested := 0
	for _, p := range profiles {
		if tested == 2 {
			break
		}
		if p.Video == nil {
			continue
		}
		tested++
		t.Run(p.Name, func(t *testing.T) {
			uri, err := dev.StreamURI(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(uri)
			if err != nil {
				t.Fatal("cannot parse the stream URI")
			}
			if u.Hostname() != su.Hostname() {
				t.Fatalf("stream is on another host (%s), refusing to contact it", u.Hostname())
			}
			t.Logf("profile %q (Media%d): %s %dx%d, stream %s", p.Name, p.Media, p.Video.Encoding, p.Video.Width, p.Video.Height, rtsp.RedactURL(uri))
			r, err := camera.ProbeRTSP(ctx, camera.RTSPConfig{
				RTSP:    rtsp.Config{URL: uri, Username: user, Password: pass, Transport: transport},
				Library: lib,
			}, seconds)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("\n%s", r)
			if r.Frames < int(seconds.Seconds()) {
				t.Errorf("only %d frames", r.Frames)
			}
			if p.Video.Width != 0 && (r.Width != p.Video.Width || r.Height != p.Video.Height) {
				t.Errorf("decoded %dx%d, profile says %dx%d", r.Width, r.Height, p.Video.Width, p.Video.Height)
			}
			if r.VaryingFrames < r.Frames*9/10 {
				t.Errorf("only %d of %d frames have varying luma", r.VaryingFrames, r.Frames)
			}
			if r.ChangingFrames == 0 {
				t.Error("no frame differs from its predecessor")
			}
			if r.LatencyP50 <= 0 {
				t.Error("no latency measured")
			}
		})
	}
	if tested == 0 {
		t.Fatal("no profile has video")
	}
	t.Run("audio", func(t *testing.T) { liveAudio(ctx, t, dev, profiles, user, pass, transport, lib) })
}

// liveAudio receives the first profile with audio and checks the frame rhythm.
func liveAudio(ctx context.Context, t *testing.T, dev *onvif.Client, profiles []onvif.Profile, user, pass string, tr rtsp.Transport, lib *gaal.Library) {
	var p *onvif.Profile
	for i := range profiles {
		if profiles[i].Audio != nil {
			p = &profiles[i]
			break
		}
	}
	if p == nil {
		t.Skip("no profile has audio")
	}
	uri, err := dev.StreamURI(ctx, *p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: uri, Username: user, Password: pass, Transport: tr,
		Select: func(t rtsp.Track) bool { return t.Media == rtsp.Audio }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	track := c.Tracks()[0]
	t.Logf("audio track: %s, config %x", track, track.Extra)
	var dec gaal.AudioDecoder
	if d, err := lib.NewAudioDecoder(track.AudioConfig()); err == nil {
		dec = d
		defer d.Close()
		t.Logf("decoding with %s", d.Backend().Name)
	} else {
		t.Logf("no audio decoder here: %v", err)
	}
	const span = 5 * time.Second
	start := time.Now()
	frames, samples, nonSilent := 0, 0, 0
	var last time.Duration
	var firstArrival, lastArrival time.Time // Dial may return before the first frame
	for time.Since(start) < span {
		pk, err := c.ReadTimeout(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if frames > 0 && pk.PTS <= last {
			t.Errorf("audio PTS not increasing: %v after %v", pk.PTS, last)
		}
		last = pk.PTS
		if frames == 0 {
			firstArrival = pk.Arrival
		}
		lastArrival = pk.Arrival
		frames++
		if dec != nil && dec.Send(pk.Packet) == nil {
			for {
				f, err := dec.Receive()
				if err != nil {
					break
				}
				samples += len(f.Samples)
				for _, v := range f.Samples {
					if v > 16 || v < -16 {
						nonSilent++
						break
					}
				}
				f.Release()
			}
		}
		pk.Release()
	}
	rate := float64(frames-1) / lastArrival.Sub(firstArrival).Seconds()
	s := c.Stats().Tracks[0]
	t.Logf("audio: %d frames, %.1f per second, %d decoded samples, %d frames above silence, lost %d, jitter %v", frames, rate, samples, nonSilent, s.PacketsLost, s.Jitter.Round(10*time.Microsecond))
	if want := float64(track.ClockRate) / 1024; track.Codec == gaal.AAC && (rate < want*0.8 || rate > want*1.2) {
		t.Errorf("%.1f frames per second, expected about %.1f", rate, want)
	}
}
