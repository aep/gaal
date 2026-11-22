package rtsp

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

type challenge struct {
	scheme string // "basic" or "digest"
	params map[string]string
}

func (c challenge) param(k string) string { return c.params[k] }

func (c challenge) stale() bool { return strings.EqualFold(c.params["stale"], "true") }

func (c challenge) algorithm() string {
	a := strings.ToUpper(c.params["algorithm"])
	if a == "" {
		return "MD5"
	}
	return a
}

func isTokenByte(b byte) bool {
	return b > 32 && b < 127 && !strings.ContainsRune(`"(),/:;<=>?@[\]{}`, rune(b))
}

// parseChallenges reads WWW-Authenticate values. One value may hold several
// comma separated challenges.
func parseChallenges(values []string) []challenge {
	var out []challenge
	for _, v := range values {
		var cur *challenge
		for i := 0; i < len(v); {
			for i < len(v) && (v[i] == ' ' || v[i] == '\t' || v[i] == ',') {
				i++
			}
			start := i
			for i < len(v) && isTokenByte(v[i]) {
				i++
			}
			if i == start {
				if i < len(v) {
					i++ // skip a byte that cannot start a token
				}
				continue
			}
			tok := v[start:i]
			j := i
			for j < len(v) && v[j] == ' ' {
				j++
			}
			if j >= len(v) || v[j] != '=' {
				if len(out) >= 16 {
					return out
				}
				out = append(out, challenge{scheme: strings.ToLower(tok), params: map[string]string{}})
				cur = &out[len(out)-1]
				continue
			}
			i = j + 1
			for i < len(v) && v[i] == ' ' {
				i++
			}
			var val strings.Builder
			if i < len(v) && v[i] == '"' {
				for i++; i < len(v) && v[i] != '"'; i++ {
					if v[i] == '\\' && i+1 < len(v) {
						i++
					}
					val.WriteByte(v[i])
				}
				i++
			} else {
				s := i
				for i < len(v) && v[i] != ',' && v[i] != ' ' {
					i++
				}
				val.WriteString(v[s:i])
			}
			if cur != nil && len(cur.params) < 32 {
				cur.params[strings.ToLower(tok)] = val.String()
			}
		}
	}
	return out
}

func digestHash(alg string) func() hash.Hash {
	switch strings.TrimSuffix(alg, "-SESS") {
	case "MD5":
		return md5.New
	case "SHA-256":
		return sha256.New
	}
	return nil
}

func qopAuth(c challenge) (usable, offered bool) {
	q := c.param("qop")
	if q == "" {
		return true, false
	}
	for _, o := range strings.Split(q, ",") {
		if strings.EqualFold(strings.TrimSpace(o), "auth") {
			return true, true
		}
	}
	return false, true
}

// chooseChallenge picks the strongest challenge we can answer.
func chooseChallenge(cs []challenge, allowBasic bool) (challenge, bool) {
	best, score := challenge{}, 0
	for _, c := range cs {
		s := 0
		switch c.scheme {
		case "basic":
			if allowBasic {
				s = 1
			}
		case "digest":
			usable, _ := qopAuth(c)
			if !usable || c.param("nonce") == "" || digestHash(c.algorithm()) == nil {
				continue
			}
			s = 2
			if strings.HasPrefix(c.algorithm(), "SHA-256") {
				s = 3
			}
		}
		if s > score {
			best, score = c, s
		}
	}
	return best, score > 0
}

type authenticator struct {
	user, pass string
	ch         challenge
	nc         uint32
}

func hexHash(h func() hash.Hash, s string) string {
	d := h()
	d.Write([]byte(s))
	return hex.EncodeToString(d.Sum(nil))
}

func quoteParam(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "").Replace(v)
}

func (a *authenticator) header(method, uri string) string {
	if a.ch.scheme == "basic" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.user+":"+a.pass))
	}
	var raw [8]byte
	rand.Read(raw[:])
	return a.digest(method, uri, hex.EncodeToString(raw[:]))
}

func (a *authenticator) digest(method, uri, cnonce string) string {
	c := a.ch
	alg := c.algorithm()
	h := digestHash(alg)
	realm, nonce := c.param("realm"), c.param("nonce")
	ha1 := hexHash(h, a.user+":"+realm+":"+a.pass)
	if strings.HasSuffix(alg, "-SESS") {
		ha1 = hexHash(h, ha1+":"+nonce+":"+cnonce)
	}
	ha2 := hexHash(h, method+":"+uri)
	_, hasQop := qopAuth(c)
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s"`,
		quoteParam(a.user), quoteParam(realm), quoteParam(nonce), quoteParam(uri))
	if hasQop {
		a.nc++
		nc := fmt.Sprintf("%08x", a.nc)
		resp := hexHash(h, ha1+":"+nonce+":"+nc+":"+cnonce+":auth:"+ha2)
		fmt.Fprintf(&b, `, response="%s", qop=auth, nc=%s, cnonce="%s"`, resp, nc, cnonce)
	} else {
		fmt.Fprintf(&b, `, response="%s"`, hexHash(h, ha1+":"+nonce+":"+ha2))
	}
	if c.params["algorithm"] != "" {
		fmt.Fprintf(&b, ", algorithm=%s", alg)
	}
	if o := c.param("opaque"); o != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, quoteParam(o))
	}
	return b.String()
}
