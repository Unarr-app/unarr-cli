package engine

import (
	"math/rand"
	"net/url"
	"strings"
	"testing"
)

// secretSpellings is every way a tool may echo secret: decoded, path/query
// escaped (either hex case) and as userinfo.
func secretSpellings(s string) map[string]string {
	return map[string]string{
		"plain":      s,
		"pathesc":    url.PathEscape(s),
		"queryesc":   url.QueryEscape(s),
		"pathescLow": lowerHexEscapes(url.PathEscape(s)),
		"queryLow":   lowerHexEscapes(url.QueryEscape(s)),
		"userinfo":   strings.TrimPrefix(url.UserPassword("x", s).String(), "x:"),
	}
}

func lowerHexEscapes(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1] = strings.ToLower(string(b[i+1]))[0]
			b[i+2] = strings.ToLower(string(b[i+2]))[0]
		}
	}
	return string(b)
}

// sourceURLsFor builds the three URL shapes that carry a secret: Xtream path
// credentials, a query token and a userinfo password.
func sourceURLsFor(user, secret string) map[string]string {
	return map[string]string{
		"xtream":   "http://panel.example:8080/movie/" + url.PathEscape(user) + "/" + url.PathEscape(secret) + "/123.mkv",
		"query":    "https://cdn.example/f.mkv?token=" + url.QueryEscape(secret) + "&e=1",
		"userinfo": "http://" + url.UserPassword(user, secret).String() + "@cdn.example/v/1.mkv",
	}
}

// leaks reports every spelling of secret that survives redaction of text
// built around it, with and without the full URL in the same text.
func leaks(secret, user string) []string {
	var out []string
	for kind, raw := range sourceURLsFor(user, secret) {
		for name, form := range secretSpellings(secret) {
			for _, text := range []string{
				"err " + form + " end",
				"GET " + raw + " -> 403 (" + form + ")",
				form,
			} {
				if strings.Contains(RedactSourceText(text, raw), form) {
					out = append(out, kind+"/"+name+": "+form)
				}
			}
		}
	}
	return out
}

// Seeded sweep over secrets with every URL-special character, including literal
// "%XX" sequences (which the previous double-unescape leaked: 32 in 12 000 in
// the round-2 review).
func TestRedactSourceTextNoSecretSpellingLeaks(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@:/+%& =?#~!$'()*,;[]"
	rng := rand.New(rand.NewSource(42))
	gen := func() string {
		n := 6 + rng.Intn(8)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	total := 0
	for i := 0; i < 700; i++ {
		secret, user := gen(), gen()
		for _, l := range leaks(secret, user) {
			total++
			if total <= 10 {
				t.Errorf("secret %q leaked as %s", secret, l)
			}
		}
	}
	// Literal escapes in the secret itself, the exact round-2 shape.
	for _, secret := range []string{"&?%A06W9xc=B", "ab%41cd%2Fef", "%%zz%25%41x", "p%40ssw0rd!"} {
		for _, l := range leaks(secret, "alice99") {
			total++
			t.Errorf("secret %q leaked as %s", secret, l)
		}
	}
	if total > 0 {
		t.Fatalf("%d leaked spellings", total)
	}
}

func FuzzRedactSourceText(f *testing.F) {
	for _, s := range []string{"s3cr3tPass", "&?%A06W9xc=B", "p%40ss w0rd", "%%zz%25%41x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, secret string) {
		if len(secret) < 6 || len(secret) > 40 || !isPrintableASCII(secret) {
			t.Skip()
		}
		if l := leaks(secret, "alice99"); len(l) > 0 {
			t.Fatalf("secret %q leaked: %v", secret, l)
		}
	})
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 && s[i] != ' ' || s[i] > 0x7e {
			return false
		}
	}
	return true
}
