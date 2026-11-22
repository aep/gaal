package onvif

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUser = "viewer"
	testPass = "s3cret-Pw"
)

type fakeDevice struct {
	srv      *httptest.Server
	skew     time.Duration
	authed   atomic.Int32
	denied   atomic.Int32
	actions  atomic.Int32
	noMedia2 bool
	denyAll  bool
}

var (
	reDigest  = regexp.MustCompile(`<wsse:Password[^>]*>([^<]+)<`)
	reNonce   = regexp.MustCompile(`<wsse:Nonce[^>]*>([^<]+)<`)
	reCreated = regexp.MustCompile(`<wsu:Created>([^<]+)<`)
)

func (d *fakeDevice) check(body string) bool {
	dg, n, c := reDigest.FindStringSubmatch(body), reNonce.FindStringSubmatch(body), reCreated.FindStringSubmatch(body)
	if dg == nil || n == nil || c == nil {
		return false
	}
	nonce, _ := base64.StdEncoding.DecodeString(n[1])
	if passwordDigest(testPass, nonce, c[1]) != dg[1] {
		return false
	}
	created, err := time.Parse("2006-01-02T15:04:05.000Z", c[1])
	if err != nil {
		return false
	}
	diff := created.Sub(time.Now().Add(d.skew))
	return diff > -5*time.Second && diff < 5*time.Second
}

func envelopeOf(s string) string {
	return `<?xml version="1.0"?><SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="x" xmlns:trt="y" xmlns:tt="z" xmlns:tr2="q"><SOAP-ENV:Body>` + s + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func (d *fakeDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	d.actions.Add(1)
	w.Header().Set("Content-Type", "application/soap+xml")
	host := "http://192.0.2.77:9999" // the device reports an unreachable address
	if strings.Contains(body, "GetSystemDateAndTime") {
		t := time.Now().Add(d.skew).UTC()
		io.WriteString(w, envelopeOf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:UTCDateTime><tt:Time><tt:Hour>`+
			itoa(t.Hour())+`</tt:Hour><tt:Minute>`+itoa(t.Minute())+`</tt:Minute><tt:Second>`+itoa(t.Second())+`</tt:Second></tt:Time><tt:Date><tt:Year>`+
			itoa(t.Year())+`</tt:Year><tt:Month>`+itoa(int(t.Month()))+`</tt:Month><tt:Day>`+itoa(t.Day())+`</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`))
		return
	}
	if d.denyAll || !d.check(body) {
		d.denied.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, envelopeOf(`<SOAP-ENV:Fault><SOAP-ENV:Code><SOAP-ENV:Value>SOAP-ENV:Sender</SOAP-ENV:Value><SOAP-ENV:Subcode><SOAP-ENV:Value>ter:NotAuthorized</SOAP-ENV:Value></SOAP-ENV:Subcode></SOAP-ENV:Code><SOAP-ENV:Reason><SOAP-ENV:Text>denied for `+testPass+`</SOAP-ENV:Text></SOAP-ENV:Reason></SOAP-ENV:Fault>`))
		return
	}
	d.authed.Add(1)
	switch {
	case strings.Contains(body, "GetDeviceInformation"):
		io.WriteString(w, envelopeOf(`<tds:GetDeviceInformationResponse><tds:Manufacturer>Acme</tds:Manufacturer><tds:Model>Cam</tds:Model><tds:FirmwareVersion>1</tds:FirmwareVersion><tds:SerialNumber>S</tds:SerialNumber><tds:HardwareId>H</tds:HardwareId></tds:GetDeviceInformationResponse>`))
	case strings.Contains(body, "GetServices"):
		m2 := `<tds:Service><tds:Namespace>` + nsMedia2 + `</tds:Namespace><tds:XAddr>` + host + `/onvif/media2</tds:XAddr><tds:Version><tt:Major>2</tt:Major><tt:Minor>60</tt:Minor></tds:Version></tds:Service>`
		if d.noMedia2 {
			m2 = ""
		}
		io.WriteString(w, envelopeOf(`<tds:GetServicesResponse><tds:Service><tds:Namespace>`+nsMedia1+`</tds:Namespace><tds:XAddr>`+host+`/onvif/media</tds:XAddr><tds:Version><tt:Major>2</tt:Major><tt:Minor>6</tt:Minor></tds:Version></tds:Service>`+m2+`</tds:GetServicesResponse>`))
	case strings.Contains(body, "GetCapabilities"):
		io.WriteString(w, envelopeOf(`<tds:GetCapabilitiesResponse><tds:Capabilities><tt:Device><tt:XAddr>`+host+`/onvif/device_service</tt:XAddr></tt:Device><tt:Media><tt:XAddr>`+host+`/onvif/media</tt:XAddr><tt:StreamingCapabilities><tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP></tt:StreamingCapabilities></tt:Media></tds:Capabilities></tds:GetCapabilitiesResponse>`))
	case strings.Contains(body, "GetProfiles") && strings.Contains(r.URL.Path, "media2"):
		io.WriteString(w, envelopeOf(`<tr2:GetProfilesResponse><tr2:Profiles token="m2main" fixed="true"><tr2:Name>Main</tr2:Name><tr2:Configurations><tr2:VideoEncoder Encoding="H265" GovLength="50"><tt:Resolution><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl></tr2:VideoEncoder></tr2:Configurations></tr2:Profiles></tr2:GetProfilesResponse>`))
	case strings.Contains(body, "GetProfiles"):
		io.WriteString(w, envelopeOf(`<trt:GetProfilesResponse><trt:Profiles token="p1" fixed="true"><tt:Name>Sub</tt:Name><tt:VideoEncoderConfiguration><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>512</tt:BitrateLimit></tt:RateControl><tt:H264><tt:GovLength>30</tt:GovLength></tt:H264></tt:VideoEncoderConfiguration><tt:AudioEncoderConfiguration><tt:Encoding>AAC</tt:Encoding><tt:Bitrate>64</tt:Bitrate><tt:SampleRate>16</tt:SampleRate></tt:AudioEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse>`))
	case strings.Contains(body, "GetStreamUri") && strings.Contains(r.URL.Path, "media2"):
		io.WriteString(w, envelopeOf(`<tr2:GetStreamUriResponse><tr2:Uri>rtsp://192.0.2.77/main</tr2:Uri></tr2:GetStreamUriResponse>`))
	case strings.Contains(body, "GetStreamUri"):
		io.WriteString(w, envelopeOf(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://192.0.2.77/sub</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`))
	default:
		w.WriteHeader(500)
	}
}

func itoa(n int) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func newDevice(t *testing.T, d *fakeDevice) *Client {
	t.Helper()
	d.srv = httptest.NewServer(d)
	t.Cleanup(d.srv.Close)
	c, err := New(Config{URL: d.srv.URL + "/onvif/device_service", Username: testUser, Password: testPass})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPasswordDigestVector(t *testing.T) {
	// Example from the WS-Security UsernameToken profile, as used by ONVIF.
	nonce, _ := base64.StdEncoding.DecodeString("LKqI6G/AikKCQrN0zqZFlg==")
	if got := passwordDigest("userpassword", nonce, "2010-09-16T07:50:45Z"); got != "tuOSpGlFlIXsozq4HFNeeGeFLEI=" {
		t.Fatalf("digest %s", got)
	}
}

func TestDeviceFlowMedia2(t *testing.T) {
	d := &fakeDevice{skew: time.Hour}
	c := newDevice(t, d)
	ctx := context.Background()
	info, err := c.DeviceInformation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Manufacturer != "Acme" || info.Model != "Cam" {
		t.Fatalf("%+v", info)
	}
	if off := c.ClockOffset(); off < 59*time.Minute || off > 61*time.Minute {
		t.Fatalf("offset %v", off)
	}
	caps, err := c.Capabilities(ctx)
	if err != nil || !caps.RTPRTSPTCP {
		t.Fatalf("%+v %v", caps, err)
	}
	if !strings.HasPrefix(caps.Media, d.srv.URL) {
		t.Fatalf("service address not moved to the device host: %s", caps.Media)
	}
	svcs, err := c.Services(ctx)
	if err != nil || len(svcs) != 2 {
		t.Fatalf("%+v %v", svcs, err)
	}
	ps, err := c.Profiles(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("%+v %v", ps, err)
	}
	p := ps[0]
	if p.Media != Media2 || p.Video.Encoding != "H265" || p.Video.Width != 2560 || p.Video.FrameRate != 25 || p.Video.GOV != 50 {
		t.Fatalf("%+v %+v", p, p.Video)
	}
	if u, err := c.StreamURI(ctx, p); err != nil || u != "rtsp://192.0.2.77/main" {
		t.Fatalf("%q %v", u, err)
	}
}

func TestDeviceFlowMedia1(t *testing.T) {
	c := newDevice(t, &fakeDevice{noMedia2: true})
	ctx := context.Background()
	ps, err := c.Profiles(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("%+v %v", ps, err)
	}
	p := ps[0]
	if p.Media != Media1 || p.Token != "p1" || p.Video.Encoding != "H264" || p.Video.GOV != 30 || p.Audio.Encoding != "AAC" {
		t.Fatalf("%+v", p)
	}
	if u, err := c.StreamURI(ctx, p); err != nil || u != "rtsp://192.0.2.77/sub" {
		t.Fatalf("%q %v", u, err)
	}
}

func TestClockSkewRecovery(t *testing.T) {
	d := &fakeDevice{}
	c := newDevice(t, d)
	if _, err := c.DeviceInformation(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.skew = 2 * time.Hour // the device clock jumps
	if _, err := c.DeviceInformation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.denied.Load() != 1 {
		t.Fatalf("denied %d", d.denied.Load())
	}
}

func TestWrongPasswordNotRetried(t *testing.T) {
	d := &fakeDevice{}
	newDevice(t, d)
	c, _ := New(Config{URL: d.srv.URL, Username: testUser, Password: "wrong-" + testPass})
	_, err := c.DeviceInformation(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if d.denied.Load() != 1 {
		t.Fatalf("%d rejected attempts, want 1", d.denied.Load())
	}
	var fe *FaultError
	if !errors.As(err, &fe) {
		t.Fatalf("no FaultError in %v", err)
	}
}

func TestErrorsHideCredentials(t *testing.T) {
	d := &fakeDevice{denyAll: true}
	newDevice(t, d)
	// The fault text quotes the password; the client knows it and removes it.
	c, _ := New(Config{URL: strings.Replace(d.srv.URL, "http://", "http://"+testUser+":"+testPass+"@", 1), Timeout: time.Second})
	_, err := c.DeviceInformation(context.Background())
	if !errors.Is(err, ErrUnauthorized) || strings.Contains(err.Error(), testPass) {
		t.Fatalf("err = %v", err)
	}
	// Network errors must not quote a URL with credentials.
	c2, _ := New(Config{URL: "http://" + testUser + ":" + testPass + "@127.0.0.1:1/x", Timeout: 200 * time.Millisecond})
	_, err = c2.SyncClock(context.Background())
	if err == nil || strings.Contains(err.Error(), testPass) || strings.Contains(err.Error(), testUser) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(Config{URL: "http://u:" + testPass + "@bad host/"}); err == nil || strings.Contains(err.Error(), testPass) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestsCarryNoPlainPassword(t *testing.T) {
	tok := usernameToken(testUser, testPass, time.Now())
	if strings.Contains(tok, testPass) || !strings.Contains(tok, "PasswordDigest") {
		t.Fatal(tok)
	}
}

func TestDiscoverLoopback(t *testing.T) {
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 8192)
		n, from, err := srv.ReadFromUDP(buf)
		if err != nil {
			return
		}
		id := regexp.MustCompile(`MessageID>([^<]+)<`).FindSubmatch(buf[:n])
		reply := func(rel string) string {
			return `<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:a="x" xmlns:d="y"><e:Header><a:RelatesTo>` + rel + `</a:RelatesTo></e:Header><e:Body><d:ProbeMatches><d:ProbeMatch><a:EndpointReference><a:Address>urn:uuid:1</a:Address></a:EndpointReference><d:Types>dn:NetworkVideoTransmitter</d:Types><d:Scopes>onvif://www.onvif.org/name/My%20Cam onvif://www.onvif.org/hardware/E1</d:Scopes><d:XAddrs>http://127.0.0.1:8000/onvif/device_service</d:XAddrs></d:ProbeMatch></d:ProbeMatches></e:Body></e:Envelope>`
		}
		srv.WriteToUDP([]byte(reply("urn:uuid:not-ours")), from)
		srv.WriteToUDP([]byte("garbage"), from)
		srv.WriteToUDP([]byte(reply(string(id[1]))), from)
		srv.WriteToUDP([]byte(reply(string(id[1]))), from) // duplicates collapse
	}()
	ds, err := Discover(context.Background(), DiscoverOptions{Timeout: 200 * time.Millisecond, Target: srv.LocalAddr().String(), LocalAddrs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Name() != "My Cam" || ds[0].Hardware() != "E1" || ds[0].From != "127.0.0.1" {
		t.Fatalf("%+v", ds)
	}
}

func FuzzParseProbeMatch(f *testing.F) {
	f.Add([]byte(`<e><Header><RelatesTo>id</RelatesTo></Header><Body><ProbeMatches><ProbeMatch><XAddrs>a b</XAddrs></ProbeMatch></ProbeMatches></Body></e>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		ds, _ := parseProbeMatch(b, "id")
		for _, d := range ds {
			if len(d.XAddrs) > 64 {
				t.Fatal("unbounded list")
			}
			_ = d.Name()
		}
	})
}

func FuzzResponses(f *testing.F) {
	f.Add(envelopeOf(`<tds:GetServicesResponse><tds:Service><tds:Namespace>a</tds:Namespace></tds:Service></tds:GetServicesResponse>`))
	f.Add(`<Envelope><Body><Fault><faultcode>x</faultcode></Fault></Body></Envelope>`)
	f.Fuzz(func(t *testing.T, doc string) {
		srv := fuzzServer(doc)
		c, _ := New(Config{URL: "http://device.invalid/x", HTTPClient: srv})
		ctx := context.Background()
		c.SyncClock(ctx)
		c.Capabilities(ctx)
		c.Services(ctx)
		c.DeviceInformation(ctx)
		c.resolved, c.media1, c.media2 = true, "http://device.invalid/m1", "http://device.invalid/m2"
		c.Profiles(ctx)
		c.StreamURI(ctx, Profile{Token: "t", Media: Media2})
	})
}

type staticTransport string

func (s staticTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(s))), Header: http.Header{}}, nil
}

func fuzzServer(doc string) *http.Client { return &http.Client{Transport: staticTransport(doc)} }
