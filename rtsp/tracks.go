package rtsp

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/aep/gaal"
)

// trackPlan is a track found in SDP, before any receive state exists.
type trackPlan struct {
	info  Track
	build func(*track) depacketizer
}

func decodeSprop(v string) [][]byte {
	var out [][]byte
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" || len(out) >= 16 {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(part, "="))
		}
		if err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// planFormat maps one SDP format to a codec, or reports it unsupported.
func planFormat(m *sdpMedia, f *sdpFormat) (trackPlan, bool) {
	p := trackPlan{info: Track{PayloadType: f.pt, ClockRate: f.rate, Channels: f.channels, FrameRate: m.framerate, Format: f.encoding}}
	enc := f.encoding
	if enc == "" {
		switch f.pt {
		case 0:
			enc = "PCMU"
		case 8:
			enc = "PCMA"
		case 26:
			enc = "JPEG"
		default:
			return p, false
		}
		p.info.Format = enc
	}
	video := func(c gaal.Codec) {
		p.info.Media, p.info.Codec = Video, c
		if p.info.ClockRate == 0 {
			p.info.ClockRate = 90000
		}
	}
	audio := func(c gaal.Codec, rate, ch int) {
		p.info.Media, p.info.Codec = Audio, c
		if p.info.ClockRate == 0 {
			p.info.ClockRate = rate
		}
		if p.info.Channels == 0 {
			p.info.Channels = ch
		}
	}
	switch enc {
	case "H264":
		if m := fmtpInt(f.fmtp, "packetization-mode"); m > 1 {
			return p, false
		}
		video(gaal.H264)
		p.info.ParameterSets = decodeSprop(f.fmtp["sprop-parameter-sets"])
		p.build = func(t *track) depacketizer { return newNALDepacketizer(t, false) }
	case "H265", "HEVC":
		if fmtpInt(f.fmtp, "sprop-max-don-diff") > 0 {
			return p, false
		}
		video(gaal.H265)
		for _, k := range []string{"sprop-vps", "sprop-sps", "sprop-pps"} {
			p.info.ParameterSets = append(p.info.ParameterSets, decodeSprop(f.fmtp[k])...)
		}
		p.build = func(t *track) depacketizer { return newNALDepacketizer(t, true) }
	case "JPEG":
		video(gaal.MJPEG)
		p.build = func(t *track) depacketizer { return &jpegDepacketizer{t: t, b: unitBuilder{t: t}} }
	case "PCMU", "PCMA":
		c := gaal.PCMU
		if enc == "PCMA" {
			c = gaal.PCMA
		}
		audio(c, 8000, 1)
		p.build = newFrameDepacketizer
	case "OPUS":
		audio(gaal.Opus, 48000, 2)
		p.build = newFrameDepacketizer
	case "MPEG4-GENERIC":
		return planAAC(&p, f, audio)
	case "MP4A-LATM":
		return planLATM(&p, f, audio)
	default:
		return p, false
	}
	if p.info.ClockRate <= 0 {
		return p, false
	}
	return p, true
}

func newFrameDepacketizer(t *track) depacketizer {
	return &frameDepacketizer{t: t, b: unitBuilder{t: t}}
}

func planAAC(p *trackPlan, f *sdpFormat, audio func(gaal.Codec, int, int)) (trackPlan, bool) {
	mode := strings.ToLower(f.fmtp["mode"])
	if !strings.HasPrefix(mode, "aac-") {
		return *p, false
	}
	cfg := aacParams{
		sizeLength:       fmtpInt(f.fmtp, "sizelength"),
		indexLength:      fmtpInt(f.fmtp, "indexlength"),
		indexDeltaLength: fmtpInt(f.fmtp, "indexdeltalength"),
		frameTicks:       uint32(fmtpInt(f.fmtp, "constantduration")),
	}
	if cfg.sizeLength < 1 || cfg.sizeLength > 32 || cfg.indexLength > 32 || cfg.indexDeltaLength > 32 {
		return *p, false
	}
	extra := decodeHex(f.fmtp["config"])
	rate, ch, frame, _, ok := parseASC(extra)
	if !ok {
		return *p, false
	}
	if cfg.frameTicks == 0 {
		cfg.frameTicks = uint32(frame)
	}
	audio(gaal.AAC, rate, ch)
	p.info.Extra = extra
	p.build = func(t *track) depacketizer { return &aacDepacketizer{t: t, cfg: cfg, b: unitBuilder{t: t}} }
	return *p, p.info.ClockRate > 0
}

func planLATM(p *trackPlan, f *sdpFormat, audio func(gaal.Codec, int, int)) (trackPlan, bool) {
	if v, ok := f.fmtp["cpresent"]; ok && strings.TrimSpace(v) != "0" {
		return *p, false
	}
	extra, ok := latmConfig(decodeHex(f.fmtp["config"]))
	if !ok {
		return *p, false
	}
	rate, ch, _, _, ok := parseASC(extra)
	if !ok {
		return *p, false
	}
	audio(gaal.AAC, rate, ch)
	p.info.Extra = extra
	p.build = func(t *track) depacketizer { return &latmDepacketizer{t: t, b: unitBuilder{t: t}} }
	return *p, p.info.ClockRate > 0
}

// planTracks lists the supported tracks of a session description.
func planTracks(s *sdpSession, base *url.URL) []trackPlan {
	var out []trackPlan
	for _, m := range s.media {
		if (m.kind != "video" && m.kind != "audio") || !strings.HasPrefix(m.proto, "RTP/AVP") {
			continue
		}
		for _, f := range m.formats {
			if p, ok := planFormat(m, f); ok {
				if (p.info.Media == Video) == (m.kind == "video") {
					p.info.Control = resolveControl(base, m.control)
					out = append(out, p)
					break
				}
			}
		}
	}
	return out
}

// resolveControl returns an absolute control URL without credentials. A
// relative control is appended to the base as a string, after its query if
// it has one ("rtsp://h/x?a=1" and "trackID=0" give
// "rtsp://h/x?a=1/trackID=0"), as FFmpeg, live555 and gortsplib do and the
// servers made for them expect.
func resolveControl(base *url.URL, control string) string {
	control = strings.TrimSpace(control)
	if control == "" || control == "*" {
		return base.String()
	}
	if len(control) > 8 && (strings.EqualFold(control[:7], "rtsp://") || strings.EqualFold(control[:8], "rtsps://")) {
		if u, err := url.Parse(control); err == nil {
			u.User = nil
			return u.String()
		}
		return base.String()
	}
	b := *base
	b.User = nil
	s := b.String()
	if control[0] != '?' && control[0] != '/' && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s + control
}

// String summarizes the track.
func (t Track) String() string {
	s := t.Media.String() + " " + t.Codec.String()
	if t.Width > 0 {
		s += " " + strconv.Itoa(t.Width) + "x" + strconv.Itoa(t.Height)
	}
	if t.Media == Audio {
		s += " " + strconv.Itoa(t.Channels) + "ch"
	}
	return fmt.Sprintf("%s pt=%d clock=%d", s, t.PayloadType, t.ClockRate)
}
