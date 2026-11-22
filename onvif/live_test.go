package onvif

import (
	"context"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aep/gaal/rtsp"
)

// TestLiveDevice needs GAAL_TEST_ONVIF_URL, GAAL_TEST_ONVIF_USER and
// GAAL_TEST_ONVIF_PASSWORD. Every call is read-only.
func TestLiveDevice(t *testing.T) {
	service, user, pass := os.Getenv("GAAL_TEST_ONVIF_URL"), os.Getenv("GAAL_TEST_ONVIF_USER"), os.Getenv("GAAL_TEST_ONVIF_PASSWORD")
	if service == "" || user == "" || pass == "" {
		t.Skip("GAAL_TEST_ONVIF_URL, GAAL_TEST_ONVIF_USER and GAAL_TEST_ONVIF_PASSWORD are not set")
	}
	su, err := url.Parse(service)
	if err != nil {
		t.Fatal("cannot parse the service URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Probe only from the interface that reaches the device.
	probe, err := net.Dial("udp4", net.JoinHostPort(su.Hostname(), "9"))
	if err != nil {
		t.Fatal(err)
	}
	local := probe.LocalAddr().(*net.UDPAddr).IP
	probe.Close()
	found, err := Discover(ctx, DiscoverOptions{Timeout: 3 * time.Second, LocalAddrs: []net.IP{local}})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, d := range found {
		if d.From == su.Hostname() {
			seen = true
			t.Logf("discovered: name=%q hardware=%q xaddrs=%v", d.Name(), d.Hardware(), d.XAddrs)
		}
	}
	t.Logf("discovery: %d answers on the local network", len(found))
	if !seen {
		t.Error("the device did not answer the probe")
	}

	c, err := New(Config{URL: service, Username: user, Password: pass})
	if err != nil {
		t.Fatal(err)
	}
	off, err := c.SyncClock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("device clock offset: %v", off.Round(time.Second))
	info, err := c.DeviceInformation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("device: %s %s firmware %s", info.Manufacturer, info.Model, info.FirmwareVersion)
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("capabilities: media=%s rtp_tcp=%v rtp_rtsp_tcp=%v multicast=%v ptz=%v", caps.Media, caps.RTPTCP, caps.RTPRTSPTCP, caps.RTPMulticast, caps.PTZ != "")
	if svcs, err := c.Services(ctx); err != nil {
		t.Logf("GetServices: %v", err)
	} else {
		for _, s := range svcs {
			t.Logf("service: %s v%d.%d", s.Namespace, s.Major, s.Minor)
		}
	}
	ps, err := c.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("no profiles")
	}
	if m1, err := c.ProfilesFrom(ctx, Media1); err != nil {
		t.Errorf("Media1 profiles: %v", err)
	} else {
		for _, p := range m1 {
			uri, err := c.StreamURI(ctx, p)
			if err != nil {
				t.Errorf("Media1 stream URI of %s: %v", p.Token, err)
				continue
			}
			t.Logf("media1 profile %q token=%s: %s", p.Name, p.Token, rtsp.RedactURL(uri))
		}
	}
	for _, p := range ps {
		uri, err := c.StreamURI(ctx, p)
		if err != nil {
			t.Errorf("profile %s: %v", p.Token, err)
			continue
		}
		line := rtsp.RedactURL(uri)
		if v := p.Video; v != nil {
			t.Logf("profile %q token=%s media%d: %s %dx%d %.0f fps %d kbit/s gov %d: %s", p.Name, p.Token, p.Media, v.Encoding, v.Width, v.Height, v.FrameRate, v.BitrateKbps, v.GOV, line)
		} else {
			t.Logf("profile %q token=%s media%d: %s", p.Name, p.Token, p.Media, line)
		}
		if a := p.Audio; a != nil {
			t.Logf("  audio %s %d kbit/s %d kHz", a.Encoding, a.BitrateKbps, a.SampleRate)
		}
	}
}
