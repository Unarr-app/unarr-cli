package main

// Custom player command — the escape hatch that makes the built-in player list
// a convenience rather than a requirement.
//
// The dialect table (player.go) can only ever know the players someone thought
// to add: a Flatpak/Snap/AppImage install exposes no binary on PATH, mpv.net
// and SMPlayer are their own spellings, and a user may simply want flags we
// don't emit. Rather than grow that list forever, `[desktop] player_command`
// lets the user state the exact command line, with placeholders for what only
// we know (the stream URL, resume position, title, language prefs).
//
// SECURITY: the template is tokenized HERE and handed to exec as an argv array
// — never to a shell. There is no interpolation into a command string, so
// nothing carried by the unarr:// link (URL, title, languages — all already
// validated/sanitized by open.go) can turn into a separate argument or a shell
// operator. Placeholders always substitute INSIDE one token.

import (
	"strconv"
	"strings"
)

// playerPlaceholders maps each supported placeholder to its value for a
// request. An empty value drops the whole token containing it, so a template
// like `--start={start}` simply vanishes when there is nothing to resume —
// no player has to understand an empty flag.
func playerPlaceholders(req playRequest) map[string]string {
	start := ""
	if req.Start > 0 {
		start = strconv.Itoa(req.Start)
	}
	return map[string]string{
		"{url}":   req.URL,
		"{web}":   req.browserURL(),
		"{start}": start,
		"{title}": req.Title,
		"{alang}": strings.Join(req.ALang, ","),
		"{slang}": strings.Join(req.SLang, ","),
	}
}

// expandPlayerCommand turns a tokenized template into the argv to spawn.
// Tokens whose placeholders are all empty are dropped; {url} is appended when
// the template never mentions it, so a bare `player_command = "myplayer"`
// still plays something.
func expandPlayerCommand(tmpl []string, req playRequest) []string {
	values := playerPlaceholders(req)
	argv := make([]string, 0, len(tmpl)+1)
	mentionsURL := false
	for _, tok := range tmpl {
		expanded, ok := expandToken(tok, values)
		if !ok {
			continue // placeholder had no value → the flag is not applicable
		}
		if strings.Contains(tok, "{url}") || strings.Contains(tok, "{web}") {
			mentionsURL = true
		}
		argv = append(argv, expanded)
	}
	if !mentionsURL {
		argv = append(argv, req.URL)
	}
	return argv
}

// expandToken substitutes every placeholder in one token. ok=false means the
// token contained a placeholder that has no value for this request and must
// therefore be dropped entirely (flag and value travel together).
func expandToken(tok string, values map[string]string) (string, bool) {
	out := tok
	for ph, val := range values {
		if !strings.Contains(out, ph) {
			continue
		}
		if val == "" {
			return "", false
		}
		out = strings.ReplaceAll(out, ph, val)
	}
	return out, true
}

// splitCommand tokenizes a command line the way a user expects to write one —
// single and double quotes group a token, a backslash escapes the next
// character — WITHOUT any of the rest of shell semantics: no variable
// expansion, no globbing, no pipes/redirection/substitution. Those would be
// the interesting part for an attacker and are simply not implemented.
//
// Returns nil for an empty/whitespace-only template.
func splitCommand(s string) []string {
	var (
		tokens  []string
		cur     strings.Builder
		quote   rune
		escaped bool
		started bool // distinguishes an empty quoted token "" from no token
	)
	flush := func() {
		if started || cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			// Inside single quotes a backslash is literal, as in a shell.
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}
