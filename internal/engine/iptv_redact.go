package engine

import (
	"net/url"
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
	out := msg
	for _, secret := range xtreamSecrets(rawURL) {
		out = strings.ReplaceAll(out, secret, "***")
	}
	if out == msg {
		return err
	}
	return &redactedError{msg: out, err: err}
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
	creds := append([]string(nil), parts[1:3]...)
	for _, seg := range parts[1:3] {
		if dec, derr := url.PathUnescape(seg); derr == nil && dec != seg {
			creds = append(creds, dec)
		}
	}
	secrets := []string{parts[1] + "/" + parts[2]}
	if user, perr := url.PathUnescape(parts[1]); perr == nil {
		if pass, perr := url.PathUnescape(parts[2]); perr == nil {
			secrets = append(secrets, user+"/"+pass)
		}
	}
	for _, c := range creds {
		if len(c) >= 4 {
			secrets = append(secrets, c)
		}
	}
	return secrets
}
