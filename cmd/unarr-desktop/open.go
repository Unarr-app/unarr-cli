package main

// Protocol-handler mode: the web emits deep-links of the form
//
//	unarr://play?url=<stream>&start=<sec>&title=<t>&alang=<csv>&slang=<csv>
//
// and the OS hands them to `unarr-desktop --open <url>` (or as the bare argv —
// %u / %1 substitution). This file owns argument detection and the PARSER; the
// player selection/launch lives in player.go.
//
// SECURITY MODEL: a URL-scheme handler is a local, unauthenticated entry point
// — ANY web page can invoke unarr://play?url=... without user interaction
// beyond the browser's "open app?" prompt. Every query parameter is therefore
// attacker-controlled input: the inner URL is whitelisted to http/https (never
// file://, javascript:, data:, ...), start/alang/slang are parsed into strict
// types and invalid values are DROPPED (never forwarded raw), and the title is
// stripped of control characters and length-capped. The second half of the
// defense — `--` argv terminators and no shell — is in player.go.

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// playRequest is the fully validated result of parsing an unarr://play link.
// Every field is safe to embed into player argv as-is.
type playRequest struct {
	URL   string   // http(s) stream URL — the ONLY schemes the parser lets through
	Start int      // seconds; 0 = from the beginning (flag omitted)
	Title string   // display title, control-chars stripped, length-capped
	ALang []string // preferred audio languages, each validated against langTokenRE
	SLang []string // preferred subtitle languages, ditto
}

// langTokenRE matches a BCP-47-ish language tag: 2-3 letter primary subtag
// plus one optional region/script subtag ("es", "spa", "pt-BR", "zh-Hans").
// Anything that doesn't match is dropped rather than forwarded — these tokens
// end up inside player command lines.
var langTokenRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

// openArg reports whether argv requests protocol-handler mode and returns the
// raw unarr:// link. Three spellings are accepted because registration differs
// per OS: `--open <url>` (what we write into the Windows registry command and
// the Linux .desktop Exec line), `--open=<url>`, and a single bare argument
// that already starts with unarr:// (handlers that substitute %u/%1 directly).
// A bare `--open` with no URL still returns ok=true so the caller can exit
// with a usage error instead of silently starting a second tray.
func openArg(args []string) (raw string, ok bool) {
	if len(args) == 0 {
		return "", false
	}
	switch {
	case args[0] == "--open":
		if len(args) >= 2 {
			return args[1], true
		}
		return "", true
	case strings.HasPrefix(args[0], "--open="):
		return strings.TrimPrefix(args[0], "--open="), true
	case len(args) == 1 && strings.HasPrefix(strings.ToLower(args[0]), "unarr://"):
		return args[0], true
	}
	return "", false
}

// parsePlayURL validates an unarr:// link into a playRequest. Errors are
// user-facing (they end up in a desktop notification), so they say what was
// wrong rather than dumping internals.
func parsePlayURL(raw string) (playRequest, error) {
	var req playRequest
	u, err := url.Parse(raw)
	if err != nil {
		return req, fmt.Errorf("unparseable link: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "unarr") {
		return req, fmt.Errorf("unexpected scheme %q (want unarr://)", u.Scheme)
	}
	// Host is the action. Only "play" exists today; rejecting the rest loudly
	// (instead of best-effort guessing) keeps future actions backward-safe:
	// an old binary facing a newer web tells the user to update, not misplays.
	if !strings.EqualFold(u.Host, "play") {
		return req, fmt.Errorf("unsupported action %q — this unarr-desktop only understands unarr://play (update it?)", u.Host)
	}
	q := u.Query()

	stream := strings.TrimSpace(q.Get("url"))
	if stream == "" {
		return req, fmt.Errorf("missing url= parameter")
	}
	su, err := url.Parse(stream)
	if err != nil {
		return req, fmt.Errorf("unparseable stream url: %v", err)
	}
	// Scheme whitelist — the core of the security model. file:// would let a
	// web page play (and with a hostile player config, read) local files;
	// javascript:/data: and friends have no business near a media player.
	switch strings.ToLower(su.Scheme) {
	case "http", "https":
	default:
		return req, fmt.Errorf("stream url must be http(s), got scheme %q", su.Scheme)
	}
	if su.Host == "" {
		return req, fmt.Errorf("stream url has no host")
	}
	// Re-serialize rather than pass the raw query value through: url.String()
	// yields a normalized form of what we actually validated.
	req.URL = su.String()

	if s := q.Get("start"); s != "" {
		// Strict integer seconds, >= 0. Invalid values are dropped, not
		// forwarded: starting at 0:00 beats refusing to play over a typo.
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			req.Start = n
		}
	}
	req.Title = sanitizeTitle(q.Get("title"))
	req.ALang = parseLangCSV(q.Get("alang"))
	req.SLang = parseLangCSV(q.Get("slang"))
	return req, nil
}

// sanitizeTitle strips control characters (NUL would abort exec outright; ESC
// sequences could drive a terminal-attached player's tty) and caps the length
// so a hostile page can't stuff kilobytes into the player's window title.
func sanitizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	t := strings.TrimSpace(b.String())
	const maxTitleRunes = 200
	if runes := []rune(t); len(runes) > maxTitleRunes {
		t = string(runes[:maxTitleRunes])
	}
	return t
}

// parseLangCSV splits a comma-separated language list, keeping only tokens
// that match langTokenRE. Bad tokens (including anything shaped like a player
// flag) are silently dropped — never forwarded raw.
func parseLangCSV(csv string) []string {
	if csv == "" {
		return nil
	}
	var out []string
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || !langTokenRE.MatchString(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// runOpen is the whole protocol-handler mode: parse the link, hand the stream
// to a local player (player.go), return an exit code. It runs in an ephemeral
// process with no UI attached, so feedback goes to stderr + desktop
// notifications. Exit codes: 0 = something is playing (player or browser
// fallback), 1 = even the fallback failed, 2 = the link was rejected.
func runOpen(raw string) int {
	if raw == "" {
		fmt.Fprintln(os.Stderr, "unarr-desktop: usage: unarr-desktop --open 'unarr://play?url=...'")
		return 2
	}
	req, err := parsePlayURL(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: open:", err)
		notifySend("unarr could not play this link", err.Error())
		return 2
	}
	code := dispatchPlayer(req)
	if code == 0 {
		// Post-dispatch only, never before: the player is already spawned
		// (Start(), not Wait()), so the throttled version check can only
		// delay this process's exit (≤~3s), never the playback itself.
		maybeNotifyDesktopUpdate()
	}
	return code
}
