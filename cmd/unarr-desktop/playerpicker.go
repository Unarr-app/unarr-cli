package main

// The Player submenu lets the user pick which local player unarr:// links open
// in, without hand-editing config.toml. It writes the same [desktop] player key
// the dispatcher reads (player.go) via the config package's Load/Save round-trip
// — the exact path `unarr init` and the interactive config menu already use.

import (
	"fmt"
	"os"
	"strings"

	"fyne.io/systray"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

// playerOption is one Player-submenu entry: a label and the config value it
// writes ("" = autodetect).
type playerOption struct{ label, value string }

// playerChoice binds a submenu item to the value it selects.
type playerChoice struct {
	item  *systray.MenuItem
	value string
}

// playerMenuOptions returns the OS-relevant choices in menu order. mpv/VLC run
// everywhere; IINA is macOS-only and MPC-HC Windows-only — resolvePlayer would
// reject a wrong-OS pick anyway, so only offering the ones that can work keeps
// the menu honest. "Auto-detect" (empty value) is always first.
//
// The web player is deliberately NOT offered here. This submenu picks which
// LOCAL player unarr:// links open in; playing in the browser is already one
// click away on the web itself ("Web player" in the stream picker), and two
// routes to the same destination meant two code paths to keep working — the
// desktop one silently regressed to dumping the raw agent stream url into a
// tab whenever a link arrived without `web=`. The browser remains the
// last-resort fallback in dispatchPlayer when no local player exists; it is
// just no longer something the user can deliberately select here.
//
// Labels are annotated with what is actually on this machine: picking "mpv"
// without mpv installed silently played through VLC and read as a bug. The
// annotation is computed when the menu is built, so installing a player after
// the fact shows up on the next tray start.
func playerMenuOptions() []playerOption {
	opts := []playerOption{{"Auto-detect", ""}, {"mpv", "mpv"}, {"VLC", "vlc"}}
	switch hostGOOS {
	case "darwin":
		opts = append(opts, playerOption{"IINA", "iina"})
	case "windows":
		opts = append(opts, playerOption{"MPC-HC", "mpc"})
	}
	opts = append(opts, playerOption{"System default", "system"})
	for i, opt := range opts {
		opts[i].label = annotatePlayerLabel(opt)
	}
	return opts
}

// annotatePlayerLabel appends what this machine can actually do with the
// choice: nothing for auto-detect/system, the resolved variant when it differs
// from the name (mpv → Celluloid, the GTK front-end that embeds mpv and
// installs no `mpv` binary), or "not installed".
func annotatePlayerLabel(opt playerOption) string {
	if opt.value == "" || opt.value == "system" {
		return opt.label
	}
	p, ok := resolvePlayer(opt.value)
	if !ok {
		return opt.label + " — not installed"
	}
	if string(p.kind) != opt.value {
		return fmt.Sprintf("%s (%s)", opt.label, p.kind)
	}
	return opt.label
}

// buildPlayerMenu creates the Player submenu and its entries.
//
// When `player_command` is set the picker is shown DISABLED rather than
// hidden: that command outranks every entry here, so letting the user tick one
// would promise a change that never happens — while hiding the menu would hide
// why. The title says where the setting actually lives.
func (ui *trayUI) buildPlayerMenu() {
	custom := playerCommandTemplate() != ""
	title, tooltip := "Player", "Which player unarr:// links open in"
	if custom {
		title = "Player: custom command"
		tooltip = "Set by [desktop] player_command in config.toml — edit the file to change it"
	}
	ui.mPlayer = systray.AddMenuItem(title, tooltip)

	current := configuredPlayer()
	for _, opt := range playerMenuOptions() {
		it := ui.mPlayer.AddSubMenuItemCheckbox(opt.label, "Use "+opt.label, !custom && opt.value == current)
		if custom {
			it.Disable()
		}
		ui.playerChoices = append(ui.playerChoices, playerChoice{it, opt.value})
	}
}

// configuredPlayer reads the player set in config.toml ("" = auto). Unlike
// playerOverride it ignores UNARR_DESKTOP_PLAYER: the picker edits the file, so
// the checkmark must reflect the file, not a one-off env override.
func configuredPlayer() string {
	cfg, err := config.Load(config.FilePath())
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Desktop.Player))
}

// startPlayerWatchers wires each submenu entry to setPlayer. One goroutine per
// item (each has its own ClickedCh) keeps the picks out of clickLoop/navLoop,
// whose selects are already at the complexity ceiling.
func (ui *trayUI) startPlayerWatchers() {
	for _, pc := range ui.playerChoices {
		go func() {
			for range pc.item.ClickedCh {
				ui.setPlayer(pc.value)
			}
		}()
	}
}

// setPlayer persists the chosen player into config.toml [desktop] and reflects
// it in the checkmarks. An empty value clears the key (omitempty) → autodetect.
// A load/save failure surfaces on stderr AND a desktop notification (the menu is
// closed by the time it runs, so stderr alone would be invisible).
func (ui *trayUI) setPlayer(value string) {
	path := config.FilePath()
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: set player:", err)
		notify.Send("Could not change player", err.Error())
		return
	}
	cfg.Desktop.Player = value
	if err := config.Save(cfg, path); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: set player:", err)
		notify.Send("Could not change player", err.Error())
		return
	}
	ui.checkPlayer(value)
}

// checkPlayer ticks the selected entry and unticks the rest.
func (ui *trayUI) checkPlayer(value string) {
	for _, pc := range ui.playerChoices {
		if pc.value == value {
			pc.item.Check()
		} else {
			pc.item.Uncheck()
		}
	}
}
