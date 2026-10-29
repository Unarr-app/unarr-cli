package main

// "Change download folder…" in the tray: pick a folder, write [downloads] dir,
// restart the agent. It exists because the only other routes were `unarr config
// downloads` in a terminal or hand-editing config.toml — a user whose C: drive
// was filling up could not find either (support ticket, Windows desktop user).

import (
	"errors"
	"fmt"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/dialog"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

// startDownloadFolderWatcher runs the click handler on its own goroutine: the
// picker blocks on a human, and clickLoop must keep selecting.
func (ui *trayUI) startDownloadFolderWatcher() {
	go func() {
		for range ui.mChangeDir.ClickedCh {
			ui.changeDownloadFolder()
		}
	}()
}

func (ui *trayUI) changeDownloadFolder() {
	// The daemon inherits our environment and the variable outranks the file:
	// saving would report a change that never takes effect.
	if env := os.Getenv("UNARR_DOWNLOAD_DIR"); env != "" {
		changeFolderFailed(fmt.Errorf("it is set by the UNARR_DOWNLOAD_DIR environment variable (%s) — change or remove that variable", env))
		return
	}
	path := config.FilePath()
	cfg, err := config.Load(path)
	if err != nil {
		changeFolderFailed(err)
		return
	}
	dir, err := dialog.PickFolder("Choose where new downloads are saved", cfg.Download.Dir)
	switch {
	case errors.Is(err, dialog.ErrCancelled):
		return
	case errors.Is(err, dialog.ErrUnavailable):
		// No picker program (bare Linux): fall back to the file, as before.
		notify.Send("No folder picker found", "Set [downloads] dir in config.toml, then restart the agent.")
		openFile(path)
		return
	case err != nil:
		changeFolderFailed(err)
		return
	}
	// Re-read: the picker can stay open for minutes while the daemon persists a
	// key/hash or the Player menu writes the file; saving the pre-picker copy
	// would silently revert those writes.
	if cfg, err = config.Load(path); err != nil {
		changeFolderFailed(err)
		return
	}
	if dir == cfg.Download.Dir {
		return
	}
	if err := config.DriveRootError("downloads.dir", dir, hostGOOS); err != nil {
		changeFolderFailed(err)
		return
	}
	cfg.SetDownloadDir(dir)
	// The same gate the daemon applies at startup (it refuses `D:\` or system
	// folders): failing here, with the picker's answer still on screen, beats a
	// daemon that will not start.
	if err := cfg.ValidatePaths(); err != nil {
		changeFolderFailed(err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		changeFolderFailed(err)
		return
	}
	if err := config.Save(cfg, path); err != nil {
		changeFolderFailed(err)
		return
	}
	ui.applyDownloadFolder(dir)
}

// applyDownloadFolder tells the user what changed and restarts a running agent
// (it snapshots config at startup; a reload would not pick the folder up). A
// stopped or paused agent is left alone so the change never starts it.
func (ui *trayUI) applyDownloadFolder(dir string) {
	body := "New downloads go to " + dir + ". Files already downloaded are not moved."
	if readStatus().running {
		notify.Send("Download folder changed", body+" Restarting the agent…")
		markPaused(false)
		ui.control("restart", "daemon", "restart")
		return
	}
	notify.Send("Download folder changed", body+" It applies when the agent starts.")
}

func changeFolderFailed(err error) {
	fmt.Fprintln(os.Stderr, "unarr-desktop: change download folder:", err)
	notify.Send("Could not change download folder", err.Error())
}
