package main

import (
	_ "embed"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

//go:embed bip39.txt
var bip39Text string

var bip39 = func() map[string]bool {
	m := make(map[string]bool, 2048)
	for _, w := range strings.Fields(bip39Text) {
		m[w] = true
	}
	return m
}()

const hidden = "[hidden]"

var (
	base58Run  = regexp.MustCompile(`[1-9A-HJ-NP-Za-km-z]{80,90}`)
	hexRun     = regexp.MustCompile(`(?:0x)?[0-9a-fA-F]{64,}`)
	byteArray  = regexp.MustCompile(`\[\s*\d{1,3}(?:\s*,\s*\d{1,3}){31,}\s*\]`)
	wordRun    = regexp.MustCompile(`[a-z]{3,8}(?:[ \n]+[a-z]{3,8}){11,}`)
	botInPath  = regexp.MustCompile(`/bot(\d+):[A-Za-z0-9_-]+`)
	tokenInAny = regexp.MustCompile(`\b(\d{6,12}):[A-Za-z0-9_-]{30,}\b`)
)

// scrubber hides what looks like a secret. Strings that are the same shape as a key but were seen as a
// transaction id (an RPC sendTransaction answer, a swap API's signature field) stay.
type scrubber struct {
	mu   sync.Mutex
	keep map[string]bool
}

func newScrubber() *scrubber { return &scrubber{keep: map[string]bool{}} }

func (s *scrubber) allow(tx string) {
	s.mu.Lock()
	s.keep[strings.ToLower(strings.TrimPrefix(tx, "0x"))] = true
	s.mu.Unlock()
}

func (s *scrubber) kept(v string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keep[strings.ToLower(strings.TrimPrefix(v, "0x"))]
}

func (s *scrubber) text(in string) string {
	if in == "" {
		return in
	}
	out := tokenInAny.ReplaceAllString(in, "$1:"+hidden)
	out = byteArray.ReplaceAllString(out, hidden)
	out = s.replaceOutsideURLs(out, base58Run, func(m string) bool { return base58Len(m) == 64 })
	out = s.replaceOutsideURLs(out, hexRun, func(m string) bool { return len(strings.TrimPrefix(m, "0x")) == 64 })
	out = wordRun.ReplaceAllStringFunc(out, func(m string) string {
		words := strings.Fields(m)
		run := 0
		for _, w := range words {
			if bip39[w] {
				run++
				if run >= 12 {
					return hidden
				}
			} else {
				run = 0
			}
		}
		return m
	})
	return out
}

func (s *scrubber) replaceOutsideURLs(in string, re *regexp.Regexp, secret func(string) bool) string {
	locs := re.FindAllStringIndex(in, -1)
	if locs == nil {
		return in
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		m := in[l[0]:l[1]]
		if !secret(m) || s.kept(m) || inURL(in, l[0]) {
			continue
		}
		b.WriteString(in[last:l[0]])
		b.WriteString(hidden)
		last = l[1]
	}
	b.WriteString(in[last:])
	return b.String()
}

// inURL says whether position i sits inside a URL: an explorer link carries the tx id in its path.
func inURL(s string, i int) bool {
	start := strings.LastIndexFunc(s[:i], unicode.IsSpace) + 1
	word := s[start:i]
	return strings.Contains(word, "://") && !strings.ContainsAny(word, "?&=")
}

func base58Len(s string) int {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int)
	radix := big.NewInt(58)
	for _, r := range s {
		i := strings.IndexRune(alphabet, r)
		if i < 0 {
			return -1
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(i)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return zeros + len(n.Bytes())
}

var secretNames = []string{"secret", "password", "passwd", "mnemonic", "seed", "privatekey", "private_key", "apikey", "api_key", "accesstoken", "access_token", "refreshtoken", "refresh_token", "authorization", "bearer", "credential"}

func secretName(name string) bool {
	n := strings.ToLower(name)
	for _, s := range secretNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// value scrubs a decoded JSON value in place and returns it.
func (s *scrubber) value(v any) any {
	switch t := v.(type) {
	case string:
		return s.text(t)
	case map[string]any:
		for k, x := range t {
			if secretName(k) {
				t[k] = hidden
				continue
			}
			t[k] = s.value(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = s.value(x)
		}
		return t
	}
	return v
}

// botPath hides the token in a Bot API path and keeps the bot's numeric id.
func botPath(p string) string { return botInPath.ReplaceAllString(p, "/bot$1:"+hidden) }

// safePath hides the parts of an outside call that carry a key: path segments that look like a key, and every
// query value.
func safePath(u *url.URL) string {
	segs := strings.Split(u.EscapedPath(), "/")
	for i, seg := range segs {
		if keyish(seg) {
			segs[i] = "…"
		}
	}
	out := strings.Join(segs, "/")
	if u.RawQuery != "" {
		q := u.Query()
		names := make([]string, 0, len(q))
		for k := range q {
			names = append(names, k+"=…")
		}
		out += "?" + strings.Join(names, "&")
	}
	return out
}

// safeHost hides a first label that looks like a key: some RPC providers put the customer key in the subdomain.
func safeHost(h string) string {
	label, rest, ok := strings.Cut(h, ".")
	if ok && keyish(label) {
		return "…." + rest
	}
	return h
}

func keyish(s string) bool {
	if len(s) < 16 {
		return false
	}
	var digit, letter bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case unicode.IsLetter(r):
			letter = true
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return digit && letter
}
