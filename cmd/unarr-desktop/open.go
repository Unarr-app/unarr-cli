package main

// Protocol-handler mode: the web emits deep-links of the form
//
//	unarr://play?url=<stream>&start=<sec>&title=<t>&alang=<csv>&slang=<csv>&sub=<url>…
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
//
// COMPATIBILITY: known parameters are read with q.Get / q[...], never by
// enumerating what the link happens to carry, so a NEWER web that adds a
// parameter (as `sub=` was added) is played correctly by an OLDER binary — it
// simply ignores what it doesn't know. That property is load-bearing: the
// scheme launch is fire-and-forget, so the web can never learn which handler
// version is installed and cannot gate features on it.

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
	// WebURL is the unarr web-player page for this stream (`web=`), when the
	// link carries one. Optional: older web builds omit it. Used by the "web"
	// player choice and by the no-player-installed fallback, both of which
	// otherwise dump the raw .mkv into the browser.
	WebURL string
	// Playlist is a served, signed sting+feature `.m3u` URL (`playlist=`), when
	// the link carries one. When present, the player opens THIS as its media
	// (ident → feature back-to-back) instead of URL — so unarr Desktop shows the
	// same brand sting the other external players get via the served playlist.
	// Optional and backward-safe: older web builds omit it (URL alone plays);
	// a player dialect that can't consume a multi-entry .m3u still gets URL (see
	// mediaArg). Same http(s) whitelist as URL/WebURL — a file:// playlist would
	// make the player read local files, exactly what the scheme gate exists for.
	Playlist string
	// SubFiles are EXTERNAL subtitle files to side-load (`sub=`, repeatable) —
	// http(s) URLs of the web's WebVTT proxy carrying AI translations and the
	// shared provider-subtitle cache. Distinct from SLang, which only ranks the
	// tracks ALREADY inside the container: these are files the player fetches.
	// Same http(s) whitelist as URL/WebURL, capped at maxSubFiles.
	SubFiles []string
}

// maxSubFiles bounds how many `sub=` entries one link can carry. Each becomes a
// player flag, and argv is finite (Windows caps a command line at ~32 KiB), so
// an unbounded list is a way for a hostile page to make the spawn fail. The web
// caps at the same number, but this is the cap that MATTERS: the link is
// attacker-controlled input.
const maxSubFiles = 8

// browserURL is where "open this in the browser" should go: the web player
// page when the link carried one, else the stream itself (a browser can still
// play it natively, just without controls/subtitles).
func (r playRequest) browserURL() string {
	if r.WebURL != "" {
		return r.WebURL
	}
	return r.URL
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
	normalized, err := validateHTTPURL(stream, "stream url")
	if err != nil {
		return req, err
	}
	req.URL = normalized

	// web= is a convenience, never a requirement: a link that carries a broken
	// one still plays. Same validation as the stream URL — it is handed to the
	// browser, so file:/javascript: must not survive here either.
	if web := strings.TrimSpace(q.Get("web")); web != "" {
		if normalizedWeb, werr := validateHTTPURL(web, "web url"); werr == nil {
			req.WebURL = normalizedWeb
		} else {
			fmt.Fprintln(os.Stderr, "unarr-desktop: ignoring web url:", werr)
		}
	}

	// playlist= is a convenience like web=: a link carrying a broken one still
	// plays the feature via url=. Same http(s) gate — it becomes the player's
	// media argument, so file:/javascript: must not survive here either.
	if pl := strings.TrimSpace(q.Get("playlist")); pl != "" {
		if normalizedPl, plerr := validateHTTPURL(pl, "playlist url"); plerr == nil {
			req.Playlist = normalizedPl
		} else {
			fmt.Fprintln(os.Stderr, "unarr-desktop: ignoring playlist url:", plerr)
		}
	}

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
	req.SubFiles = parseSubFiles(q["sub"])
	return req, nil
}

// parseSubFiles validates the repeated `sub=` parameters into side-loadable
// subtitle URLs. Each goes through the SAME http(s) gate as the stream url —
// file:// would turn a media player into a local-file reader for any web page.
// An invalid entry is DROPPED, never fatal: subtitles are an enhancement, and
// refusing to play a movie over one bad sub link would be the worse failure.
// Order is preserved (the web sends them ranked) and the list is capped.
//
// SECURITY — DO NOT "clean up" the '#' rejection below. VLC chains several MRLs
// inside ONE --input-slave option using '#' as the separator (see vlcArgv), and
// a URL fragment is NOT part of what validateHTTPURL's scheme whitelist covers:
// `https://ok.example/a#file:///etc/passwd` is a perfectly valid https URL, so
// the whitelist passes it, and VLC then splits on '#' and opens the SECOND MRL
// (file://, smb://, anything) as a slave input. That smuggles the exact schemes
// the whitelist exists to keep out — an arbitrary local-file read driven by any
// web page. Fragments carry no meaning for a subtitle file anyway (the server
// never mints one), so we REJECT rather than strip: a link carrying one is
// either a bug or an attack, and dropping the entry keeps the failure loud in
// stderr instead of silently rewriting attacker input into something we then
// hand to a player. Rejecting also covers players other than VLC that might
// give '#' its own meaning, which stripping-per-player would not.
func parseSubFiles(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "#") {
			fmt.Fprintf(os.Stderr, "unarr-desktop: ignoring subtitle: %q contains '#' (VLC MRL separator — chained inputs are not allowed)\n", s)
			continue
		}
		normalized, err := validateHTTPURL(s, "subtitle url")
		if err != nil {
			fmt.Fprintln(os.Stderr, "unarr-desktop: ignoring subtitle:", err)
			continue
		}
		// Belt and braces: validateHTTPURL re-serializes, and a fragment could
		// in principle re-appear from a decoded form. Nothing downstream wants
		// one, so refuse anything that still carries it after normalization.
		if strings.Contains(normalized, "#") {
			fmt.Fprintf(os.Stderr, "unarr-desktop: ignoring subtitle: %q normalizes to a URL with a fragment\n", s)
			continue
		}
		out = append(out, normalized)
		if len(out) == maxSubFiles {
			break
		}
	}
	return out
}

// validateHTTPURL enforces the scheme whitelist that IS the security model
// here: file:// would let a web page play (and with a hostile player config,
// read) local files; javascript:/data: have no business near a media player or
// a browser tab we open ourselves. Returns the re-serialized URL, so callers
// use the normalized form of exactly what was validated. Shared by url= and
// web= — one gate, no second spelling to keep in sync.
// label names the field in the error text ("stream url", "web url") — these
// errors reach the user in a notification.
func validateHTTPURL(raw, label string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("unparseable %s: %v", label, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%s must be http(s), got scheme %q", label, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s has no host", label)
	}
	return u.String(), nil
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
