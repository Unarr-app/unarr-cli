package engine

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// An Xtream stream URL carries the account's username and password as path
// segments (/movie/<user>/<pass>/<id>.<ext>), and Go's *url.Error prints the
// request URL verbatim. Every error the IPTV downloader returns is reported to
// the web (download_task.error_message), shown in the UI and attached to
// support reports — so the credentials are scrubbed from it first.

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err } // errors.Is/As see the original

// redactURL returns err with the credentials of rawURL masked. The original
// error stays reachable through Unwrap, so classification is unaffected.
func redactURL(err error, rawURL string) error {
	if err == nil || rawURL == "" {
		return err
	}
	msg := err.Error()
	out := RedactSourceText(msg, rawURL)
	if out == msg {
		return err
	}
	return &redactedError{msg: out, err: err}
}

// RedactSourceText masks the secrets of a source URL wherever they appear in
// text (an ffprobe/ffmpeg error, a stderr line, a log message): the whole URL
// (raw and decoded) becomes scheme://host/***/<file name>, then any echo of its parts is
// masked on its own — the Xtream user/pass path pair, a userinfo password and
// query values (debrid/CDN tokens). The host stays: it is what makes a
// "provider unreachable" report actionable.
func RedactSourceText(text, rawURL string) string {
	if text == "" || rawURL == "" {
		return text
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return strings.ReplaceAll(text, rawURL, "***")
	}
	label := u.Scheme + "://" + u.Host + "/***"
	if base := path.Base(u.Path); base != "." && base != "/" {
		label += "/" + base // the file name: no secret, and it says which title failed
	}
	text = strings.ReplaceAll(text, rawURL, label)
	if dec, derr := url.PathUnescape(rawURL); derr == nil && dec != rawURL {
		text = strings.ReplaceAll(text, dec, label)
	}
	for _, secret := range sourceSecrets(u, rawURL) {
		if strings.Contains(secret, "%") {
			// A tool may print the escapes with either hex case, even mixed.
			text = percentEscapePattern(secret).ReplaceAllLiteralString(text, "***")
			continue
		}
		text = strings.ReplaceAll(text, secret, "***")
	}
	return text
}

// percentEscapePattern matches s literally except that the hex digits of each
// %XX escape match in either case.
func percentEscapePattern(s string) *regexp.Regexp {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteString("%(?i:" + s[i+1:i+3] + ")")
			i += 2
			continue
		}
		b.WriteString(regexp.QuoteMeta(s[i : i+1]))
	}
	return regexp.MustCompile(b.String())
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// sourceSecrets lists the parts of a source URL that must never be echoed, in
// every spelling (secretForms): the Xtream credentials, the userinfo password
// and every query value long enough to be a token (short ones like "1" would
// mangle unrelated text). Longest first, so a spelling that contains another is
// masked whole.
func sourceSecrets(u *url.URL, rawURL string) []string {
	secrets := xtreamSecrets(rawURL)
	if u.User != nil {
		if pass, ok := u.User.Password(); ok && len(pass) >= 4 {
			secrets = append(secrets, secretForms(pass, true)...)
		}
	}
	for _, vals := range u.Query() {
		for _, v := range vals {
			if len(v) >= 6 {
				secrets = append(secrets, secretForms(v, true)...)
			}
		}
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

// secretForms returns the spellings a secret can take when a tool echoes it:
// decoded, and percent-encoded as a path segment, a query value or userinfo
// (RedactSourceText matches the escapes' hex digits in either case).
//
// decoded says whether secret is already the plain value (a query value or a
// userinfo password, which net/url hands back decoded) or still an escaped
// path segment. Unescaping a plain value again would turn a literal "%A0" in
// the secret into another byte and miss every real spelling of it.
func secretForms(secret string, decoded bool) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	plain := secret
	if !decoded {
		if dec, err := url.PathUnescape(secret); err == nil {
			plain = dec
		}
	}
	userinfo := strings.TrimPrefix(url.UserPassword("x", plain).String(), "x:")
	for _, f := range []string{secret, plain, url.PathEscape(plain), url.QueryEscape(plain), userinfo} {
		add(f)
	}
	return out
}

// xtreamSecrets returns what to mask for an Xtream stream URL, most specific
// first: the "user/pass" path pair (raw and decoded — a redirect or a log may
// carry either), then each credential on its own when it is long enough to be
// masked without mangling unrelated words ("u" would eat every u).
func xtreamSecrets(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[1] == "" || parts[2] == "" {
		return nil
	}
	secrets := []string{parts[1] + "/" + parts[2]}
	if user, perr := url.PathUnescape(parts[1]); perr == nil {
		if pass, perr := url.PathUnescape(parts[2]); perr == nil {
			secrets = append(secrets, user+"/"+pass)
		}
	}
	// Each escaped segment yields its decoded value and every re-encoding of it.
	for _, seg := range parts[1:3] {
		plain := seg
		if dec, derr := url.PathUnescape(seg); derr == nil {
			plain = dec
		}
		if len(plain) >= 4 {
			secrets = append(secrets, secretForms(seg, false)...)
		}
	}
	return secrets
}
