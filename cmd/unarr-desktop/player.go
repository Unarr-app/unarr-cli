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
	playerMPV playerKind = "mpv"
	// playerCelluloid is the GTK front-end that ships mpv embedded (it installs
	// NO `mpv` binary of its own — the distro package is just `celluloid`). It
	// speaks every mpv option as `--mpv-<opt>=<value>`, so it is treated as an
	// mpv variant rather than a player of its own: a user who configured "mpv"
	// gets it, instead of silently falling through to VLC.
	playerCelluloid playerKind = "celluloid"
	playerVLC       playerKind = "vlc"
	playerIINA      playerKind = "iina"
	playerMPC       playerKind = "mpc"
	// playerWeb is not a local player at all: it hands the stream back to the
	// browser (the unarr web player when the link carries one). Selectable only
	// explicitly — autodetect must always prefer a real player.
	playerWeb playerKind = "web"
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
		return resolveMPV()
	case "celluloid":
		return resolveOnPath(playerCelluloid, "celluloid")
	case "vlc":
		return resolveOnPath(playerVLC, "vlc")
	case "iina":
		return resolveIINA()
	case "mpc", "mpc-hc":
		return resolveMPC()
	case "web", "online", "browser":
		// Always "installed": every desktop that can open a URL has a browser,
		// and openFallback degrades on its own if it can't.
		return player{kind: playerWeb}, true
	}
	return player{}, false
}

// resolveMPV resolves mpv proper, then Celluloid — the GTK front-end that
// EMBEDS mpv and installs no `mpv` binary. Without this, a config asking for
// mpv on a Celluloid box resolved to nothing and playback fell through to VLC.
func resolveMPV() (player, bool) {
	if p, ok := resolveOnPath(playerMPV, "mpv"); ok {
		return p, true
	}
	return resolveOnPath(playerCelluloid, "celluloid")
}

// resolveOnPath resolves a player that is just a binary on PATH (mpv, vlc).
func resolveOnPath(kind playerKind, bin string) (player, bool) {
	if p, err := lookPath(bin); err == nil {
		return player{kind, p}, true
	}
	return player{}, false
}

// resolveIINA resolves IINA on macOS (the .app must exist; launched via `open`).
func resolveIINA() (player, bool) {
	if hostGOOS != "darwin" {
		return player{}, false
	}
	if _, err := statFile(iinaAppPath); err != nil {
		return player{}, false
	}
	if p, err := lookPath("open"); err == nil {
		return player{playerIINA, p}, true
	}
	return player{}, false
}

// resolveMPC resolves MPC-HC on Windows (either 64- or 32-bit executable name).
func resolveMPC() (player, bool) {
	if hostGOOS != "windows" {
		return player{}, false
	}
	for _, cand := range []string{"mpc-hc64.exe", "mpc-hc.exe"} {
		if p, err := lookPath(cand); err == nil {
			return player{playerMPC, p}, true
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
//
// The warning is a desktop NOTIFICATION, not just a stderr line: the
// dispatcher is spawned by the protocol handler with no terminal attached, so
// stderr goes nowhere a user will ever look. Configuring `player = "mpv"`
// without mpv installed silently played through VLC on every click and read as
// "the setting is ignored".
func selectPlayer() (player, bool) {
	if name := playerOverride(); name != "" {
		if p, ok := resolvePlayer(name); ok {
			return p, true
		}
		fmt.Fprintf(os.Stderr, "unarr-desktop: configured player %q not available, autodetecting\n", name)
		if p, ok := autodetectPlayer(); ok {
			notifySend(fmt.Sprintf("%s is not installed", name),
				fmt.Sprintf("Playing with %s instead. Install %s, or change [desktop] player in config.toml.",
					p.kind, name))
			return p, true
		}
		return player{}, false
	}
	return autodetectPlayer()
}

// autodetectPlayer returns the first player from autodetectOrder installed on
// this host.
func autodetectPlayer() (player, bool) {
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
		return mpvArgv(p, req), nil
	case playerCelluloid:
		return celluloidArgv(p, req), nil
	case playerVLC:
		return vlcArgv(p, req), nil
	case playerIINA:
		// v1: just hand the URL over. Extras would need `open --args --mpv-*`,
		// which replaces open's own URL handling — not worth it yet.
		return []string{p.bin, "-a", "IINA", req.URL}, nil
	case playerMPC:
		return mpcArgv(p, req)
	}
	return nil, fmt.Errorf("unknown player kind %q", p.kind)
}

// mpvArgv builds mpv's command line (native --flag=value options, `--` before
// the URL so it can never be parsed as a switch).
func mpvArgv(p player, req playRequest) []string {
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
	return append(argv, "--", req.URL)
}

// celluloidArgv builds Celluloid's command line. It exposes every mpv option
// as `--mpv-<option>=<value>` (GOption), so this is mpvArgv with the flags
// re-spelled — same values, same `--` terminator before the URL, same single
// token per flag so a title can never split into a switch.
func celluloidArgv(p player, req playRequest) []string {
	argv := []string{p.bin}
	if req.Start > 0 {
		argv = append(argv, "--mpv-start="+strconv.Itoa(req.Start))
	}
	if req.Title != "" {
		argv = append(argv, "--mpv-force-media-title="+req.Title)
	}
	if len(req.ALang) > 0 {
		argv = append(argv, "--mpv-alang="+strings.Join(req.ALang, ","))
	}
	if len(req.SLang) > 0 {
		argv = append(argv, "--mpv-slang="+strings.Join(req.SLang, ","))
	}
	return append(argv, "--", req.URL)
}

// vlcArgv builds VLC's command line (its own flag spellings, `--` before URL).
func vlcArgv(p player, req playRequest) []string {
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
	return append(argv, "--", req.URL)
}

// mpcArgv builds MPC-HC's command line. mpc-hc parses /switches and has no
// end-of-options terminator, so a URL that even LOOKS like a switch is refused.
func mpcArgv(p player, req playRequest) ([]string, error) {
	if strings.HasPrefix(req.URL, "-") || strings.HasPrefix(req.URL, "/") {
		return nil, fmt.Errorf("refusing stream url %q for mpc-hc (could be parsed as a switch)", req.URL)
	}
	argv := []string{p.bin, req.URL}
	if req.Start > 0 {
		argv = append(argv, "/start", strconv.Itoa(req.Start*1000)) // mpc-hc takes milliseconds
	}
	return argv, nil
}

// dispatchPlayer launches req in the best available local player, falling
// back to the system browser (the web player can always play the stream) when
// no player is installed or the spawn fails. Exit codes as in runOpen.
func dispatchPlayer(req playRequest) int {
	p, ok := selectPlayer()
	if !ok {
		fmt.Fprintln(os.Stderr, "unarr-desktop: no supported media player found (mpv/celluloid/vlc/iina/mpc-hc)")
		notifySend("No media player found",
			"Install mpv (recommended) or VLC for the best experience. Opening in your browser instead.")
		return openFallback(req.browserURL())
	}
	// The deliberate browser choice (`player = "web"`), not a fallback: open the
	// unarr web player when the link carries one, else the stream itself.
	if p.kind == playerWeb {
		return openFallback(req.browserURL())
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
		return openFallback(req.browserURL())
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
