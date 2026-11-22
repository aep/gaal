package onvif

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	discoveryAddr = "239.255.255.250:3702"
	maxDevices    = 256
)

// Device is a discovery result.
type Device struct {
	Endpoint string   // stable identifier, usually a urn:uuid
	Types    []string // such as tdn:NetworkVideoTransmitter
	Scopes   []string
	XAddrs   []string // device service URLs
	From     string   // IP address the answer came from
}

// Name returns the name scope, if the device has one.
func (d Device) Name() string { return d.scope("name") }

// Hardware returns the hardware scope, if the device has one.
func (d Device) Hardware() string { return d.scope("hardware") }

func (d Device) scope(kind string) string {
	marker := "onvif.org/" + kind + "/"
	for _, s := range d.Scopes {
		if i := strings.Index(strings.ToLower(s), marker); i >= 0 {
			v := s[i+len(marker):]
			if u, err := url.PathUnescape(v); err == nil {
				return u
			}
			return v
		}
	}
	return ""
}

// DiscoverOptions configures Discover.
type DiscoverOptions struct {
	// Timeout is how long to collect answers. Default 2 s.
	Timeout time.Duration
	// LocalAddrs lists the IPv4 addresses to probe from. Empty means every
	// multicast capable interface.
	LocalAddrs []net.IP
	// Target overrides the multicast address, for tests.
	Target string
}

func probeMessage(id string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing" ` +
		`xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<e:Header><w:MessageID>` + id + `</w:MessageID><w:To e:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To>` +
		`<w:Action e:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action></e:Header>` +
		`<e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body></e:Envelope>`)
}

type probeMatches struct {
	Relates string `xml:"Header>RelatesTo"`
	Matches []struct {
		Endpoint string `xml:"EndpointReference>Address"`
		Types    string `xml:"Types"`
		Scopes   string `xml:"Scopes"`
		XAddrs   string `xml:"XAddrs"`
	} `xml:"Body>ProbeMatches>ProbeMatch"`
}

// parseProbeMatch decodes one answer. Answers to other probes are rejected.
func parseProbeMatch(b []byte, id string) ([]Device, bool) {
	var m probeMatches
	if err := xml.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	if strings.TrimSpace(m.Relates) != id {
		return nil, false
	}
	var out []Device
	for _, x := range m.Matches {
		d := Device{Endpoint: strings.TrimSpace(x.Endpoint), Types: limited(x.Types), Scopes: limited(x.Scopes), XAddrs: limited(x.XAddrs)}
		if d.Endpoint != "" || len(d.XAddrs) > 0 {
			out = append(out, d)
		}
	}
	return out, true
}

func limited(s string) []string {
	f := strings.Fields(s)
	return f[:min(len(f), 64)]
}

func localIPv4s() []net.IP {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip := n.IP.To4(); ip != nil {
					out = append(out, ip)
				}
			}
		}
	}
	return out
}

// Discover multicasts one WS-Discovery probe per local address and collects the
// answers. Only the probe is sent; found devices are not contacted.
func Discover(ctx context.Context, opts DiscoverOptions) ([]Device, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Second
	}
	target := opts.Target
	if target == "" {
		target = discoveryAddr
	}
	to, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return nil, err
	}
	locals := opts.LocalAddrs
	if len(locals) == 0 {
		locals = localIPv4s()
	}
	if len(locals) == 0 {
		return nil, errors.New("onvif: no usable network interface")
	}
	var raw [16]byte
	rand.Read(raw[:])
	h := hex.EncodeToString(raw[:])
	id := fmt.Sprintf("urn:uuid:%s-%s-4%s-a%s-%s", h[:8], h[8:12], h[13:16], h[17:20], h[20:])
	msg := probeMessage(id)

	type answer struct {
		d []Device
	}
	results := make(chan answer, 64)
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	var conns []*net.UDPConn
	var lastErr error
	for _, ip := range locals {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := c.WriteToUDP(msg, to); err != nil {
			lastErr = err
			c.Close()
			continue
		}
		conns = append(conns, c)
	}
	if len(conns) == 0 {
		return nil, fmt.Errorf("onvif: probe failed: %w", lastErr)
	}
	stop := context.AfterFunc(ctx, func() {
		for _, c := range conns {
			c.Close()
		}
	})
	defer stop()
	done := make(chan struct{})
	remaining := len(conns)
	for _, c := range conns {
		go func(c *net.UDPConn) {
			defer func() { done <- struct{}{} }()
			buf := make([]byte, 65536)
			for {
				n, from, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if devs, ok := parseProbeMatch(buf[:n], id); ok {
					for i := range devs {
						devs[i].From = from.IP.String()
					}
					select {
					case results <- answer{devs}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(c)
	}
	var out []Device
	seen := map[string]bool{}
	for remaining > 0 {
		select {
		case a := <-results:
			for _, d := range a.d {
				key := d.Endpoint + "|" + strings.Join(d.XAddrs, " ")
				if !seen[key] && len(out) < maxDevices {
					seen[key] = true
					out = append(out, d)
				}
			}
		case <-done:
			remaining--
		}
	}
	return out, nil
}
