// Package onvif is a small read-only ONVIF client: WS-Discovery, device
// capabilities and services, media profiles and stream URIs, for Media1 and
// Media2 devices.
//
// Only read operations exist here. Requests use a WS-Security UsernameToken
// digest, so the password itself is never sent, and they are stamped with the
// device's clock. All requests go to the host given in Config.URL, even when
// the device advertises other addresses.
package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aep/gaal/rtsp"
)

// Errors returned by this package.
var (
	ErrUnauthorized = errors.New("onvif: not authorized")
	ErrNoMedia      = errors.New("onvif: device has no media service")
)

// FaultError is a SOAP fault from the device.
type FaultError struct {
	Action, Code, Reason string
}

func (e *FaultError) Error() string {
	return fmt.Sprintf("onvif: %s: %s: %s", e.Action, e.Code, e.Reason)
}

const maxResponse = 4 << 20

const (
	nsDevice = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia1 = "http://www.onvif.org/ver10/media/wsdl"
	nsMedia2 = "http://www.onvif.org/ver20/media/wsdl"
	nsSchema = "http://www.onvif.org/ver10/schema"
)

// Config configures a Client.
type Config struct {
	// URL is the device service, such as http://host/onvif/device_service.
	URL                string
	Username, Password string
	Timeout            time.Duration // per request, default 5 s
	HTTPClient         *http.Client  // optional
}

// Client talks to one device. It is safe for concurrent use.
type Client struct {
	device     *url.URL
	user, pass string
	http       *http.Client
	timeout    time.Duration
	secrets    []string

	mu       sync.Mutex
	offset   time.Duration
	synced   bool
	media1   string
	media2   string
	resolved bool
}

// New prepares a client. It does not contact the device.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return nil, errors.New("onvif: invalid device URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("onvif: device URL must be http or https")
	}
	user, pass := cfg.Username, cfg.Password
	if u.User != nil {
		if user == "" && pass == "" {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		u.User = nil
	}
	c := &Client{device: u, user: user, pass: pass, http: cfg.HTTPClient, timeout: cfg.Timeout}
	if c.timeout <= 0 {
		c.timeout = 5 * time.Second
	}
	if c.http == nil {
		c.http = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	for _, s := range []string{pass, url.QueryEscape(pass)} {
		if s != "" {
			c.secrets = append(c.secrets, s)
		}
	}
	return c, nil
}

func (c *Client) scrub(s string) string {
	s = rtsp.RedactText(s)
	for _, x := range c.secrets {
		s = strings.ReplaceAll(s, x, "***")
	}
	return s
}

// safeErr hides anything the original message may quote.
type safeErr struct {
	msg string
	err error
}

func (e *safeErr) Error() string        { return e.msg }
func (e *safeErr) Is(target error) bool { return errors.Is(e.err, target) }
func (e *safeErr) As(target any) bool {
	if t, ok := target.(**FaultError); ok {
		return errors.As(e.err, t)
	}
	return false
}

func (c *Client) wrap(err error) error {
	if err == nil {
		return nil
	}
	return &safeErr{msg: c.scrub(err.Error()), err: err}
}

// ClockOffset returns device time minus local time, once known.
func (c *Client) ClockOffset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

func (c *Client) securityHeader(now time.Time) string {
	if c.user == "" && c.pass == "" {
		return ""
	}
	return usernameToken(c.user, c.pass, now.Add(c.ClockOffset()))
}

func usernameToken(user, pass string, created time.Time) string {
	var nonce [16]byte
	rand.Read(nonce[:])
	return buildToken(user, pass, nonce[:], created.UTC().Format("2006-01-02T15:04:05.000Z"))
}

func passwordDigest(pass string, nonce []byte, created string) string {
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func buildToken(user, pass string, nonce []byte, created string) string {
	return `<s:Header><wsse:Security s:mustUnderstand="1" xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" ` +
		`xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"><wsse:UsernameToken>` +
		`<wsse:Username>` + escape(user) + `</wsse:Username>` +
		`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + passwordDigest(pass, nonce, created) + `</wsse:Password>` +
		`<wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + base64.StdEncoding.EncodeToString(nonce) + `</wsse:Nonce>` +
		`<wsu:Created>` + created + `</wsu:Created></wsse:UsernameToken></wsse:Security></s:Header>`
}

func escape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

type envelope struct {
	Body struct {
		Inner []byte `xml:",innerxml"`
		Fault *struct {
			Code    string `xml:"faultcode"`
			Reason  string `xml:"faultstring"`
			Code12  string `xml:"Code>Value"`
			Sub     string `xml:"Code>Subcode>Value"`
			Reason2 string `xml:"Reason>Text"`
		} `xml:"Fault"`
	} `xml:"Body"`
}

func isAuthFault(code string) bool {
	c := strings.ToLower(code)
	return strings.Contains(c, "notauthorized") || strings.Contains(c, "failedauthentication") ||
		strings.Contains(c, "invalidsecurity") || strings.Contains(c, "failedcheck")
}

// call posts a request and decodes the body into out. A rejected request is
// repeated once after synchronizing with the device clock, since a wrong clock
// looks exactly like a wrong password. It is never repeated beyond that.
func (c *Client) call(ctx context.Context, endpoint, action, body string, out any) error {
	err := c.post(ctx, endpoint, action, body, out, true)
	if errors.Is(err, ErrUnauthorized) && c.user != "" && c.resync(ctx) {
		err = c.post(ctx, endpoint, action, body, out, true)
	}
	return err
}

// resync refreshes the clock offset and reports whether it changed noticeably.
func (c *Client) resync(ctx context.Context) bool {
	before := c.ClockOffset()
	if _, err := c.SyncClock(ctx); err != nil {
		return false
	}
	d := c.ClockOffset() - before
	return d > 2*time.Second || d < -2*time.Second
}

func (c *Client) post(ctx context.Context, endpoint, action, body string, out any, auth bool) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	header := ""
	if auth {
		header = c.securityHeader(time.Now())
	}
	doc := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">` +
		header + `<s:Body xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">` + body + `</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(doc))
	if err != nil {
		return errors.New("onvif: invalid service URL")
	}
	req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8; action="`+action+`"`)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the wrapper quotes the URL
		}
		return fmt.Errorf("onvif: %s: %w", shortAction(action), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("onvif: %s: reading response: %w", shortAction(action), err)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("onvif: %s: response too large", shortAction(action))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s: HTTP %d", ErrUnauthorized, shortAction(action), resp.StatusCode)
	}
	var env envelope
	if err := xml.Unmarshal(data, &env); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("onvif: %s: HTTP %d", shortAction(action), resp.StatusCode)
		}
		return fmt.Errorf("onvif: %s: invalid XML response", shortAction(action))
	}
	if f := env.Body.Fault; f != nil {
		code := firstNonEmpty(f.Sub, f.Code12, f.Code)
		reason := strings.TrimSpace(firstNonEmpty(f.Reason2, f.Reason))
		fe := &FaultError{Action: shortAction(action), Code: code, Reason: c.scrub(reason)}
		if isAuthFault(code) || isAuthFault(reason) {
			return fmt.Errorf("%w: %w", ErrUnauthorized, fe)
		}
		return fe
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("onvif: %s: HTTP %d", shortAction(action), resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(wrapInner(env.Body.Inner), out); err != nil {
		return fmt.Errorf("onvif: %s: unexpected response", shortAction(action))
	}
	return nil
}

func wrapInner(b []byte) []byte {
	out := make([]byte, 0, len(b)+13)
	out = append(out, "<r>"...)
	out = append(out, b...)
	return append(out, "</r>"...)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func shortAction(a string) string {
	if i := strings.LastIndexByte(a, '/'); i >= 0 {
		return a[i+1:]
	}
	return a
}

// localURL moves a service address the device reported onto the configured
// host, so that a wrong address never sends requests elsewhere.
func (c *Client) localURL(addr string) string {
	u, err := url.Parse(strings.TrimSpace(addr))
	if err != nil {
		return ""
	}
	out := *c.device
	out.Path, out.RawPath, out.RawQuery = u.Path, u.RawPath, u.RawQuery
	return out.String()
}

// SyncClock reads the device clock, which needs no authentication, and returns
// the offset used for later requests.
func (c *Client) SyncClock(ctx context.Context) (time.Duration, error) {
	var r struct {
		T struct {
			UTC struct {
				Time struct{ Hour, Minute, Second int } `xml:"Time"`
				Date struct{ Year, Month, Day int }     `xml:"Date"`
			} `xml:"UTCDateTime"`
		} `xml:"GetSystemDateAndTimeResponse>SystemDateAndTime"`
	}
	start := time.Now()
	const action = nsDevice + "/GetSystemDateAndTime"
	if err := c.post(ctx, c.device.String(), action, `<GetSystemDateAndTime xmlns="`+nsDevice+`"/>`, &r, false); err != nil {
		return 0, c.wrap(err)
	}
	end := time.Now()
	u := r.T.UTC
	if u.Date.Year < 2000 || u.Date.Month < 1 || u.Date.Month > 12 {
		return 0, errors.New("onvif: device reported no valid UTC time")
	}
	dev := time.Date(u.Date.Year, time.Month(u.Date.Month), u.Date.Day, u.Time.Hour, u.Time.Minute, u.Time.Second, 0, time.UTC)
	offset := dev.Sub(start.Add(end.Sub(start) / 2))
	c.mu.Lock()
	c.offset, c.synced = offset, true
	c.mu.Unlock()
	return offset, nil
}

func (c *Client) ensureSynced(ctx context.Context) {
	c.mu.Lock()
	done := c.synced
	c.mu.Unlock()
	if !done {
		c.SyncClock(ctx) // a device without a clock can still accept requests
	}
}

// DeviceInfo identifies a device.
type DeviceInfo struct {
	Manufacturer    string `xml:"Manufacturer"`
	Model           string `xml:"Model"`
	FirmwareVersion string `xml:"FirmwareVersion"`
	SerialNumber    string `xml:"SerialNumber"`
	HardwareID      string `xml:"HardwareId"`
}

// DeviceInformation returns identification data.
func (c *Client) DeviceInformation(ctx context.Context) (*DeviceInfo, error) {
	c.ensureSynced(ctx)
	var r struct {
		I DeviceInfo `xml:"GetDeviceInformationResponse"`
	}
	err := c.call(ctx, c.device.String(), nsDevice+"/GetDeviceInformation", `<GetDeviceInformation xmlns="`+nsDevice+`"/>`, &r)
	if err != nil {
		return nil, c.wrap(err)
	}
	return &r.I, nil
}

// Capabilities lists service addresses from GetCapabilities, moved to the
// configured host.
type Capabilities struct {
	Device, Media, Events, PTZ, Imaging, Analytics string
	RTPMulticast, RTPTCP, RTPRTSPTCP               bool
}

// Capabilities calls GetCapabilities.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	c.ensureSynced(ctx)
	var r struct {
		C struct {
			Device struct {
				XAddr string `xml:"XAddr"`
			} `xml:"Device"`
			Media struct {
				XAddr string `xml:"XAddr"`
				Caps  struct {
					Multicast bool `xml:"RTPMulticast"`
					TCP       bool `xml:"RTP_TCP"`
					RTSP      bool `xml:"RTP_RTSP_TCP"`
				} `xml:"StreamingCapabilities"`
			} `xml:"Media"`
			Events struct {
				XAddr string `xml:"XAddr"`
			} `xml:"Events"`
			PTZ struct {
				XAddr string `xml:"XAddr"`
			} `xml:"PTZ"`
			Imaging struct {
				XAddr string `xml:"XAddr"`
			} `xml:"Imaging"`
			Analytics struct {
				XAddr string `xml:"XAddr"`
			} `xml:"Analytics"`
		} `xml:"GetCapabilitiesResponse>Capabilities"`
	}
	body := `<GetCapabilities xmlns="` + nsDevice + `"><Category>All</Category></GetCapabilities>`
	if err := c.call(ctx, c.device.String(), nsDevice+"/GetCapabilities", body, &r); err != nil {
		return nil, c.wrap(err)
	}
	mapped := func(a string) string {
		if a == "" {
			return ""
		}
		return c.localURL(a)
	}
	out := &Capabilities{
		Device: mapped(r.C.Device.XAddr), Media: mapped(r.C.Media.XAddr), Events: mapped(r.C.Events.XAddr),
		PTZ: mapped(r.C.PTZ.XAddr), Imaging: mapped(r.C.Imaging.XAddr), Analytics: mapped(r.C.Analytics.XAddr),
		RTPMulticast: r.C.Media.Caps.Multicast, RTPTCP: r.C.Media.Caps.TCP, RTPRTSPTCP: r.C.Media.Caps.RTSP,
	}
	return out, nil
}

// Service is one entry of GetServices.
type Service struct {
	Namespace    string
	XAddr        string // moved to the configured host
	Major, Minor int
}

// Services calls GetServices. Older devices may not implement it.
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	c.ensureSynced(ctx)
	var r struct {
		S []struct {
			Namespace string `xml:"Namespace"`
			XAddr     string `xml:"XAddr"`
			Version   struct {
				Major int `xml:"Major"`
				Minor int `xml:"Minor"`
			} `xml:"Version"`
		} `xml:"GetServicesResponse>Service"`
	}
	body := `<GetServices xmlns="` + nsDevice + `"><IncludeCapability>false</IncludeCapability></GetServices>`
	if err := c.call(ctx, c.device.String(), nsDevice+"/GetServices", body, &r); err != nil {
		return nil, c.wrap(err)
	}
	out := make([]Service, 0, len(r.S))
	for _, s := range r.S {
		out = append(out, Service{Namespace: strings.TrimSpace(s.Namespace), XAddr: c.localURL(s.XAddr), Major: s.Version.Major, Minor: s.Version.Minor})
	}
	return out, nil
}

// mediaServices finds the Media1 and Media2 endpoints once.
func (c *Client) mediaServices(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	m1, m2, done := c.media1, c.media2, c.resolved
	c.mu.Unlock()
	if done {
		return m1, m2, nil
	}
	svcs, err := c.Services(ctx)
	if errors.Is(err, ErrUnauthorized) {
		return "", "", err
	}
	if err == nil {
		for _, s := range svcs {
			switch s.Namespace {
			case nsMedia1:
				m1 = s.XAddr
			case nsMedia2:
				m2 = s.XAddr
			}
		}
	}
	if m1 == "" && m2 == "" {
		caps, err := c.Capabilities(ctx)
		if err != nil {
			return "", "", err
		}
		m1 = caps.Media
	}
	if m1 == "" && m2 == "" {
		return "", "", ErrNoMedia
	}
	c.mu.Lock()
	c.media1, c.media2, c.resolved = m1, m2, true
	c.mu.Unlock()
	return m1, m2, nil
}

// MediaVersion selects the service behind a Profile.
type MediaVersion int

// Media service versions.
const (
	Media1 MediaVersion = 1
	Media2 MediaVersion = 2
)

// VideoEncoder describes the encoder of a profile.
type VideoEncoder struct {
	Encoding      string // such as H264 or H265
	Width, Height int
	FrameRate     float64
	BitrateKbps   int
	GOV           int
}

// AudioEncoder describes the audio encoder of a profile.
type AudioEncoder struct {
	Encoding    string
	BitrateKbps int
	SampleRate  int
}

// Profile is a media profile.
type Profile struct {
	Token string
	Name  string
	Fixed bool
	Media MediaVersion
	Video *VideoEncoder
	Audio *AudioEncoder
}

// Profiles lists media profiles, from Media2 when available, otherwise Media1.
func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	m1, m2, err := c.mediaServices(ctx)
	if err != nil {
		return nil, c.wrap(err)
	}
	var lastErr error
	if m2 != "" {
		p, err := c.profiles2(ctx, m2)
		if err == nil && len(p) > 0 {
			return p, nil
		}
		lastErr = err
	}
	if m1 != "" {
		p, err := c.profiles1(ctx, m1)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	return nil, c.wrap(lastErr)
}

// ProfilesFrom lists the profiles of one media service version.
func (c *Client) ProfilesFrom(ctx context.Context, v MediaVersion) ([]Profile, error) {
	m1, m2, err := c.mediaServices(ctx)
	if err != nil {
		return nil, c.wrap(err)
	}
	var out []Profile
	switch {
	case v == Media1 && m1 != "":
		out, err = c.profiles1(ctx, m1)
	case v == Media2 && m2 != "":
		out, err = c.profiles2(ctx, m2)
	default:
		return nil, ErrNoMedia
	}
	return out, c.wrap(err)
}

func (c *Client) profiles1(ctx context.Context, ep string) ([]Profile, error) {
	var r struct {
		P []struct {
			Token string `xml:"token,attr"`
			Fixed bool   `xml:"fixed,attr"`
			Name  string `xml:"Name"`
			V     *struct {
				Encoding string  `xml:"Encoding"`
				W        int     `xml:"Resolution>Width"`
				H        int     `xml:"Resolution>Height"`
				FPS      float64 `xml:"RateControl>FrameRateLimit"`
				Bitrate  int     `xml:"RateControl>BitrateLimit"`
				GOV      int     `xml:"H264>GovLength"`
			} `xml:"VideoEncoderConfiguration"`
			A *struct {
				Encoding   string `xml:"Encoding"`
				Bitrate    int    `xml:"Bitrate"`
				SampleRate int    `xml:"SampleRate"`
			} `xml:"AudioEncoderConfiguration"`
		} `xml:"GetProfilesResponse>Profiles"`
	}
	if err := c.call(ctx, ep, nsMedia1+"/GetProfiles", `<GetProfiles xmlns="`+nsMedia1+`"/>`, &r); err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(r.P))
	for _, p := range r.P {
		o := Profile{Token: p.Token, Name: p.Name, Fixed: p.Fixed, Media: Media1}
		if v := p.V; v != nil {
			o.Video = &VideoEncoder{Encoding: v.Encoding, Width: v.W, Height: v.H, FrameRate: v.FPS, BitrateKbps: v.Bitrate, GOV: v.GOV}
		}
		if a := p.A; a != nil {
			o.Audio = &AudioEncoder{Encoding: a.Encoding, BitrateKbps: a.Bitrate, SampleRate: a.SampleRate}
		}
		out = append(out, o)
	}
	return out, nil
}

func (c *Client) profiles2(ctx context.Context, ep string) ([]Profile, error) {
	var r struct {
		P []struct {
			Token string `xml:"token,attr"`
			Fixed bool   `xml:"fixed,attr"`
			Name  string `xml:"Name"`
			V     *struct {
				EncAttr string  `xml:"Encoding,attr"`
				GOVAttr int     `xml:"GovLength,attr"`
				Enc     string  `xml:"Encoding"`
				W       int     `xml:"Resolution>Width"`
				H       int     `xml:"Resolution>Height"`
				FPS     float64 `xml:"RateControl>FrameRateLimit"`
				Bitrate int     `xml:"RateControl>BitrateLimit"`
			} `xml:"Configurations>VideoEncoder"`
			A *struct {
				Encoding   string `xml:"Encoding"`
				Bitrate    int    `xml:"Bitrate"`
				SampleRate int    `xml:"SampleRate"`
			} `xml:"Configurations>AudioEncoder"`
		} `xml:"GetProfilesResponse>Profiles"`
	}
	body := `<GetProfiles xmlns="` + nsMedia2 + `"><Type>VideoEncoder</Type><Type>AudioEncoder</Type></GetProfiles>`
	if err := c.call(ctx, ep, nsMedia2+"/GetProfiles", body, &r); err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(r.P))
	for _, p := range r.P {
		o := Profile{Token: p.Token, Name: p.Name, Fixed: p.Fixed, Media: Media2}
		if v := p.V; v != nil {
			o.Video = &VideoEncoder{Encoding: firstNonEmpty(v.EncAttr, v.Enc), Width: v.W, Height: v.H, FrameRate: v.FPS, BitrateKbps: v.Bitrate, GOV: v.GOVAttr}
		}
		if a := p.A; a != nil {
			o.Audio = &AudioEncoder{Encoding: a.Encoding, BitrateKbps: a.Bitrate, SampleRate: a.SampleRate}
		}
		out = append(out, o)
	}
	return out, nil
}

// StreamURI returns the RTSP URI of a profile. It may carry credentials if the
// device put them there, so pass it through rtsp.RedactURL before printing it.
func (c *Client) StreamURI(ctx context.Context, p Profile) (string, error) {
	m1, m2, err := c.mediaServices(ctx)
	if err != nil {
		return "", c.wrap(err)
	}
	var r struct {
		URI1 string `xml:"GetStreamUriResponse>MediaUri>Uri"`
		URI2 string `xml:"GetStreamUriResponse>Uri"`
	}
	token := escape(p.Token)
	if p.Media == Media2 && m2 != "" {
		body := `<GetStreamUri xmlns="` + nsMedia2 + `"><Protocol>RTSP</Protocol><ProfileToken>` + token + `</ProfileToken></GetStreamUri>`
		err = c.call(ctx, m2, nsMedia2+"/GetStreamUri", body, &r)
	} else if m1 != "" {
		body := `<GetStreamUri xmlns="` + nsMedia1 + `"><StreamSetup><Stream xmlns="` + nsSchema + `">RTP-Unicast</Stream>` +
			`<Transport xmlns="` + nsSchema + `"><Protocol>RTSP</Protocol></Transport></StreamSetup><ProfileToken>` + token + `</ProfileToken></GetStreamUri>`
		err = c.call(ctx, m1, nsMedia1+"/GetStreamUri", body, &r)
	} else {
		return "", ErrNoMedia
	}
	if err != nil {
		return "", c.wrap(err)
	}
	uri := strings.TrimSpace(firstNonEmpty(r.URI1, r.URI2))
	if uri == "" {
		return "", errors.New("onvif: device returned no stream URI")
	}
	return uri, nil
}
