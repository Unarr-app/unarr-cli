package engine

import (
	"strings"
	"testing"
)

// M1: a secret echoed WITHOUT the full URL, in any spelling a tool may print —
// decoded, path/query/userinfo-escaped, either hex case — is masked.
func TestRedactSourceTextMasksEverySpellingOfASecret(t *testing.T) {
	cases := []struct {
		name, rawURL, text, secret string
	}{
		{"userinfo password escaped", "http://bob:p%40ssw0rd@cdn.example/v/1.mkv",
			"auth bob:p%40ssw0rd rejected", "p%40ssw0rd"},
		{"userinfo password lower-hex", "http://bob:p%40ssw0rd@cdn.example/v/1.mkv",
			"auth bob:p%40ssw0rd rejected (p%40ssw0rd)", "ssw0rd"},
		{"userinfo password decoded", "http://bob:p%40ssw0rd@cdn.example/v/1.mkv",
			"password p@ssw0rd refused", "p@ssw0rd"},
		{"userinfo password upper vs lower hex", "http://bob:a%2fb%2Fcdef@cdn.example/v/1.mkv",
			"user bob pass a%2Fb%2fcdef", "cdef"},
		{"xtream password path-escaped", "http://panel.example:8080/movie/alice/s%3Acret99/42.mkv",
			"401 for s%3acret99", "cret99"},
		{"xtream password decoded", "http://panel.example:8080/movie/alice/s%3Acret99/42.mkv",
			"login alice / s:cret99 failed", "cret99"},
		{"xtream password query-escaped", "http://panel.example:8080/movie/alice/p%20w%2Bxyz/42.mkv",
			"echo p+w%2Bxyz", "xyz"},
		{"query token escaped", "https://cdn.example/f.mkv?token=ab%2Fcd%2Bef12",
			"denied token ab%2fcd%2bef12", "ef12"},
		{"query token decoded", "https://cdn.example/f.mkv?token=ab%2Fcd%2Bef12",
			"denied token ab/cd+ef12", "ef12"},
		{"query token plus-for-space", "https://cdn.example/f.mkv?sig=abc+def+ghi",
			"sig=abc+def+ghi and abc def ghi", "def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSourceText(tc.text, tc.rawURL)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("secret %q survived: %q", tc.secret, got)
			}
			if !strings.Contains(got, "***") {
				t.Fatalf("nothing masked: %q", got)
			}
		})
	}
}

// The host stays (it is what makes a report actionable) and unrelated text is
// not mangled by the extra spellings.
func TestRedactSourceTextKeepsTheRest(t *testing.T) {
	raw := "http://bob:p%40ssw0rd@cdn.example/v/film.mkv"
	got := RedactSourceText("GET "+raw+" -> 403 Forbidden (retrying)", raw)
	for _, keep := range []string{"cdn.example", "film.mkv", "403 Forbidden (retrying)"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("%q lost from %q", keep, got)
		}
	}
}

func TestPercentEscapePattern(t *testing.T) {
	re := percentEscapePattern("a%2Fb.c%3a")
	for _, s := range []string{"a%2Fb.c%3a", "a%2fb.c%3A", "a%2fb.c%3a"} {
		if !re.MatchString(s) {
			t.Fatalf("%q not matched", s)
		}
	}
	if re.MatchString("a%2Fbxc%3a") {
		t.Fatal("the dot must match literally")
	}
	if !percentEscapePattern("100%").MatchString("100%") {
		t.Fatal("a trailing percent is literal")
	}
}
