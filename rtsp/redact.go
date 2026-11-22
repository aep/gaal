package rtsp

import (
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const redacted = "***"

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>]+`)

// RedactURL returns raw without credentials: user information is removed and
// the values of query parameters that look like credentials are replaced. It
// works on malformed URLs too, and prefers removing too much.
func RedactURL(raw string) string {
	rest, scheme := raw, ""
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = raw[:i+3], raw[i+3:]
	}
	query, fragment := "", ""
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest, fragment = rest[:i], rest[i:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, query = rest[:i], rest[i+1:]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	out := scheme + rest
	if query != "" || strings.Contains(raw, "?") {
		out += "?" + redactQuery(query)
	}
	if fragment != "" {
		out += "#" + redacted
	}
	return out
}

func redactQuery(q string) string {
	parts := strings.FieldsFunc(q, func(r rune) bool { return r == '&' || r == ';' })
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if ok && sensitiveKey(k) {
			parts[i] = k + "=" + redacted
		}
	}
	return strings.Join(parts, "&")
}

func sensitiveKey(k string) bool {
	if u, err := url.QueryUnescape(k); err == nil {
		k = u
	}
	k = strings.ToLower(k)
	for _, s := range []string{"pass", "pwd", "user", "login", "token", "secret", "auth", "key", "sig", "session"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// RedactText redacts every URL inside s.
func RedactText(s string) string { return urlInText.ReplaceAllStringFunc(s, RedactURL) }

// scrubber removes known credentials from text.
type scrubber struct{ secrets []string }

func newScrubber(user, pass string) *scrubber {
	s := &scrubber{}
	add := func(v string, minLen int) {
		if len(v) >= minLen {
			s.secrets = append(s.secrets, v)
		}
	}
	if pass != "" {
		add(pass, 1)
		add(url.QueryEscape(pass), 1)
		add(url.PathEscape(pass), 1)
		add(base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), 1)
	}
	add(user, 3)
	add(url.QueryEscape(user), 3)
	// Longest first, so an encoded form is not left half replaced.
	for i := range s.secrets {
		for j := i + 1; j < len(s.secrets); j++ {
			if len(s.secrets[j]) > len(s.secrets[i]) {
				s.secrets[i], s.secrets[j] = s.secrets[j], s.secrets[i]
			}
		}
	}
	return s
}

func (s *scrubber) text(v string) string {
	v = RedactText(v)
	for _, x := range s.secrets {
		v = strings.ReplaceAll(v, x, redacted)
	}
	return v
}

// wrap returns err with credentials removed from its message. The original is
// not reachable through Unwrap, because it may still contain them, but
// errors.Is works and errors.As finds a *StatusError.
func (s *scrubber) wrap(err error) error {
	if err == nil {
		return nil
	}
	var safe *safeError
	if errors.As(err, &safe) {
		return err
	}
	return &safeError{msg: s.text(err.Error()), err: err}
}

type safeError struct {
	msg string
	err error
}

func (e *safeError) Error() string        { return e.msg }
func (e *safeError) Is(target error) bool { return errors.Is(e.err, target) }

func (e *safeError) As(target any) bool {
	if t, ok := target.(**StatusError); ok {
		return errors.As(e.err, t)
	}
	return false
}

// Timeout reports whether the underlying failure was a timeout.
func (e *safeError) Timeout() bool {
	var t interface{ Timeout() bool }
	return errors.As(e.err, &t) && t.Timeout()
}
