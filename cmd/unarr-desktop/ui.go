package main

// trayUI owns the menu items and the three loops that drive them (status
// ticker, account refresher, click handler). Split out of onReady so each
// responsibility stays a small, gate-passing unit — onReady itself only
// constructs the UI and starts the loops (the Linux DBus backend exports the
// menu only after onReady returns, so nothing here may block it).

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

type trayUI struct {
	icons map[trayState][]byte
	shown trayState

	mStatus, mAccount, mVersion, mUpgrade *systray.MenuItem
	mPause, mResume, mRestart             *systray.MenuItem
	mEnableDownloads                      *systray.MenuItem
	mOpen, mLibrary, mDownloads           *systray.MenuItem
	mConfigure, mEdit, mPlayer            *systray.MenuItem
	mLogs, mSendLogs, mDocs               *systray.MenuItem
	mAutostart, mUpdate, mQuit            *systray.MenuItem

	// playerChoices are the Player submenu entries (playerpicker.go), each
	// bound to the config value it writes.
	playerChoices []playerChoice

	// hasCLI-mode tracking: the tray adapts between the full daemon UI and a
	// player-only menu (no `unarr` installed). applyMode toggles the mode-
	// specific items only on transition; cliModeInit forces the first apply.
	prevHasCLI  bool
	cliModeInit bool

	// shownTooltip diffs the app-level tooltip so a task-count change updates it
	// (SetTooltip only when the text actually changed — the DBus/Cocoa spam guard
	// applyState uses for the icon, applied here to the tooltip).
	shownTooltip string

	// Crash-watcher state — touched only from refresh()/control(); systray
	// delivers those on independent goroutines but never concurrently in
	// practice (ticker + click loop), and the worst race is a duplicate
	// notification, so plain fields are fine.
	suppressCrashUntil time.Time
	reportedCrashPID   int
	// lastStopPID: the daemon PID a tray-initiated stop/restart targeted. If
	// that exact PID later shows up as a stale "running" state (old CLIs don't
	// always clean up on stop), it is OUR stop, not a crash — reap, don't report.
	lastStopPID int
	// prevRunning tracks daemon liveness transitions so a fresh start (the tail
	// end of a sign-in flow) kicks an account refresh instead of waiting out
	// the 30-min ticker.
	prevRunning bool

	// accountShown: a successful fetch has populated the account row, so
	// transient errors keep the last good title instead of flickering to
	// "unavailable". Touched only on the account goroutine.
	accountShown bool
	accountKick  chan struct{}
	// upgradeTarget: pricing URL derived from the SAME base the account was
	// fetched from, so the CTA never sends the user to buy on a different
	// server than the one whose "Free" status triggered it. atomic.Value
	// because the click loop reads it while the account goroutine writes it.
	upgradeTarget atomic.Value // string
}

// newTrayUI builds the menu. Order matters: it is the visual layout.
func newTrayUI() *trayUI {
	ui := &trayUI{
		icons:       buildStateIcons(trayIcon),
		shown:       stateUnknown,
		accountKick: make(chan struct{}, 1),
	}
	ui.upgradeTarget.Store(upgradeURL(webBase()))

	systray.SetIcon(trayIcon)
	systray.SetTitle("unarr")
	systray.SetTooltip("unarr agent")

	ui.mStatus = systray.AddMenuItem("Checking…", "Agent status")
	ui.mStatus.Disable()
	ui.mAccount = systray.AddMenuItem("Account: …", "Signed-in unarr account")
	ui.mAccount.Disable()
	ui.mVersion = systray.AddMenuItem("Version: …", "Agent and app versions")
	ui.mVersion.Disable()
	ui.mUpgrade = systray.AddMenuItem("Upgrade to unarr+", "See unarr+ plans")
	ui.mUpgrade.Hide() // hidden until a successful fetch proves the account is not pro
	systray.AddSeparator()
	ui.mPause = systray.AddMenuItem("Pause agent", "Stop the agent (downloads and streams halt)")
	ui.mResume = systray.AddMenuItem("Resume agent", "Start the agent")
	ui.mRestart = systray.AddMenuItem("Restart agent", "Restart the agent")
	// Shown only in player-only mode (no CLI): the upgrade path from "just a
	// player" to the full downloads+library agent, handled entirely on the web.
	ui.mEnableDownloads = systray.AddMenuItem("Enable downloads & library…",
		"Install the unarr agent to download and build your library")
	ui.mEnableDownloads.Hide()
	systray.AddSeparator()
	ui.mOpen = systray.AddMenuItem("Open unarr.app", "Open the unarr web app")
	ui.mLibrary = systray.AddMenuItem("Open library (web)", "Your unarr library on the web")
	ui.mDownloads = systray.AddMenuItem("Open downloads folder", "Open the agent's download directory")
	ui.mConfigure = systray.AddMenuItem("Configure agent (web)", "Paths, codecs, hardware — on the web")
	ui.mEdit = systray.AddMenuItem("Edit config.toml", "Open the agent config file")
	// Player submenu: pick which local player unarr:// links open in (writes
	// [desktop] player). Checkmark reflects the current config value.
	ui.mPlayer = systray.AddMenuItem("Player", "Which player unarr:// links open in")
	current := configuredPlayer()
	for _, opt := range playerMenuOptions() {
		it := ui.mPlayer.AddSubMenuItemCheckbox(opt.label, "Use "+opt.label, opt.value == current)
		ui.playerChoices = append(ui.playerChoices, playerChoice{it, opt.value})
	}
	systray.AddSeparator()
	ui.mLogs = systray.AddMenuItem("View logs", "Open the agent log file")
	ui.mSendLogs = systray.AddMenuItem("Send logs to support", "Send agent logs to the developers")
	ui.mDocs = systray.AddMenuItem("Documentation", "Open the unarr docs")
	systray.AddSeparator()
	ui.mAutostart = systray.AddMenuItemCheckbox("Start at login",
		"Launch unarr-desktop when you sign in", false)
	// Reflect the real on-disk state at startup; a probe error leaves the box
	// unchecked (the safe default) but is surfaced, never swallowed.
	if enabled, err := autostartEnabled(); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: autostart status:", err)
	} else if enabled {
		ui.mAutostart.Check()
	}
	systray.AddSeparator()
	// Slots pattern: systray menus are append-only, so the update item is
	// created up-front and Hidden — updateLoop Show()s it only when a newer
	// release is actually known. Never auto-applies.
	ui.mUpdate = systray.AddMenuItem("Update desktop app", "Install the latest unarr-desktop version")
	ui.mUpdate.Hide()
	ui.mQuit = systray.AddMenuItem("Quit", "Close the tray (the agent keeps running)")
	ui.applyMenuIcons()
	return ui
}

// applyState swaps the tray icon only on transitions (SetIcon on every 5s tick
// would spam DBus/Cocoa for nothing). The tooltip is driven separately by
// setTooltip so it can also reflect the download count, which changes without a
// state transition.
func (ui *trayUI) applyState(st trayState) {
	if st == ui.shown {
		return
	}
	ui.shown = st
	systray.SetIcon(ui.icons[st])
}

// setTooltip updates the tray tooltip only when the text changed — same
// transition-only discipline as applyState, so a 5s tick with nothing new is a
// no-op instead of a DBus/Cocoa write.
func (ui *trayUI) setTooltip(s string) {
	if s == ui.shownTooltip {
		return
	}
	ui.shownTooltip = s
	systray.SetTooltip(s)
}

// trayTooltip is the icon hover text: the download count when the agent is
// actively working, else the plain state label.
func trayTooltip(st trayState, s agentStatus) string {
	if st == stateDownloading {
		return fmt.Sprintf("unarr — %d download(s) active", s.tasks)
	}
	return "unarr agent — " + st.label()
}

// refresh picks the mode (full daemon UI vs player-only) each tick and renders
// it. Installing/removing the `unarr` CLI while the tray runs flips the menu
// without a restart. applyMode does the one-time Show/Hide on transition.
func (ui *trayUI) refresh() {
	cli := hasCLI()
	if !ui.cliModeInit || cli != ui.prevHasCLI {
		ui.applyMode(cli)
		ui.prevHasCLI = cli
		ui.cliModeInit = true
		if cli {
			ui.kickAccount() // CLI just appeared — fetch account for the full UI
		}
	}
	if cli {
		ui.renderDaemonStatus()
	} else {
		ui.renderPlayerStatus()
	}
}

// applyMode Show/Hides the mode-specific menu items. Called only on the
// full<->player transition (systray Show/Hide every tick would spam the host).
func (ui *trayUI) applyMode(cli bool) {
	daemon := []*systray.MenuItem{ui.mPause, ui.mResume, ui.mRestart, ui.mAccount, ui.mVersion}
	for _, it := range daemon {
		if cli {
			it.Show()
		} else {
			it.Hide()
		}
	}
	if cli {
		ui.mEnableDownloads.Hide()
	} else {
		ui.mEnableDownloads.Show()
		ui.mUpgrade.Hide() // no account fetched in player-only mode
	}
}

// renderPlayerStatus is the player-only status row: no daemon to report on, so
// the icon is the plain (stopped) logo and the row states the handler is live.
func (ui *trayUI) renderPlayerStatus() {
	ui.applyState(stateStopped)
	ui.setTooltip("unarr — player handler active")
	ui.mStatus.SetTitle("Player handler active")
	ui.mStatus.SetTooltip("unarr:// links open in your local player")
}

// renderDaemonStatus reflects daemon state into the status row + pause/resume/
// restart enablement. Read from the same state file `unarr status` uses.
func (ui *trayUI) renderDaemonStatus() {
	s := readStatus()
	if s.running && isPausedMarker() {
		markPaused(false) // resumed outside the tray (CLI/web) — self-heal
	}
	if s.running && !ui.prevRunning {
		ui.kickAccount() // daemon just came up (e.g. post-sign-in) — re-fetch
	}
	ui.prevRunning = s.running
	if s.crashed && s.pid == ui.lastStopPID {
		// Tray-initiated stop that the (old) CLI didn't clean up after —
		// reap the orphan and re-read: renders paused/stopped, never crash.
		reapStaleState(s.pid)
		s = readStatus()
	}
	st := displayState(s, isPausedMarker())
	ui.applyState(st)
	ui.setTooltip(trayTooltip(st, s))
	switch st {
	case stateRunning, stateDownloading:
		// Downloads are what the user cares about; the PID is diagnostic, so it
		// moves to the row tooltip. No active tasks → the plain "running" line.
		if s.tasks > 0 {
			ui.mStatus.SetTitle(fmt.Sprintf("Downloading %d item(s)", s.tasks))
		} else {
			ui.mStatus.SetTitle("Agent: running")
		}
		ui.mStatus.SetTooltip(fmt.Sprintf("Agent running · PID %d", s.pid))
		ui.mPause.Enable()
		ui.mResume.Disable()
		ui.mRestart.Enable()
	case stateCrashed:
		ui.mStatus.SetTitle("Agent: crashed")
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
		if s.pid != ui.reportedCrashPID && time.Now().After(ui.suppressCrashUntil) {
			ui.reportedCrashPID = s.pid
			go handleCrash(s)
		}
	case statePaused:
		ui.mStatus.SetTitle("Agent: paused")
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
	default:
		ui.mStatus.SetTitle("Agent: stopped")
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
	}
}

// control execs a daemon command, surfaces spawn errors, and nudges a refresh
// shortly after (the state file updates asynchronously) on top of the ticker.
// Stops/restarts remember the targeted PID so a state file the old daemon
// failed to clean up is recognized as our stop, not a crash.
func (ui *trayUI) control(args ...string) {
	if args[0] == "stop" || args[0] == "daemon" {
		if s := readStatus(); s.running {
			ui.lastStopPID = s.pid
		}
	}
	ui.suppressCrashUntil = time.Now().Add(crashSuppressWindow)
	if err := runUnarr(args...); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: control:", err)
	}
	time.AfterFunc(1500*time.Millisecond, ui.refresh)
}

func (ui *trayUI) statusLoop() {
	t := time.NewTicker(statusPeriod)
	defer t.Stop()
	for range t.C {
		ui.refresh()
	}
}

// updateLoop is the daily desktop-update check — same on-disk throttle state
// the --open mode uses, so tray + ephemeral invocations share one 24h probe
// and one notification budget. Re-evaluated every 6h in case the tray outlives
// the throttle window. Dev builds skip it (version compare meaningless).
// The appliedVersion guard keeps the item from re-appearing after an update
// was applied: this process still runs the OLD compiled-in `version` until
// restarted, so IsNewer alone would re-Show() forever.
func (ui *trayUI) updateLoop() {
	if version == "dev" {
		return
	}
	for {
		latest, st, dirty := refreshLatestVersion(trayUpdateHTTPBudget)
		persistUpdateCheckState(st, dirty)
		if latest != "" && upgrade.IsNewer(version, latest) && latest != loadAppliedVersion() {
			ui.mUpdate.Show()
		}
		time.Sleep(6 * time.Hour)
	}
}

// kickAccount schedules an out-of-band account refresh (non-blocking; the
// 1-slot channel coalesces bursts).
func (ui *trayUI) kickAccount() {
	select {
	case ui.accountKick <- struct{}{}:
	default:
	}
}

func (ui *trayUI) accountLoop() {
	ui.refreshAccount()
	t := time.NewTicker(accountPeriod)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-ui.accountKick:
		}
		ui.refreshAccount()
	}
}

// refreshAccount reflects the signed-in account + versions into their rows and
// shows the upgrade CTA only for a fetched non-pro account. Permanent auth
// failures (401 invalid key / 410 agent_revoked) render as signed-out — NOT as
// a transient error — or a revoked key would freeze the old email/plan/CTA on
// screen forever. Transient errors keep the last good title.
func (ui *trayUI) refreshAccount() {
	if !hasCLI() {
		return // player-only: no daemon, no credential — account rows stay hidden
	}
	ui.mVersion.SetTitle(versionTitle(resolveAgentVersion(), version))
	info, base, err := fetchAccount()
	var httpErr *agent.HTTPError
	authDead := errors.As(err, &httpErr) &&
		(httpErr.StatusCode == 401 || httpErr.StatusCode == 410)
	switch {
	case errors.Is(err, errNotSignedIn) || authDead:
		ui.mAccount.SetTitle("Account: not signed in")
		ui.mUpgrade.Hide()
		ui.accountShown = false
	case err != nil:
		fmt.Fprintln(os.Stderr, "unarr-desktop: account:", err)
		if !ui.accountShown {
			ui.mAccount.SetTitle("Account: unavailable")
		}
	default:
		ui.accountShown = true
		ui.upgradeTarget.Store(upgradeURL(base))
		ui.mAccount.SetTitle(accountTitle(info))
		if info.IsPro {
			ui.mUpgrade.Hide()
		} else {
			ui.mUpgrade.Show()
		}
	}
}

// upgradeCTA opens the pricing page and re-checks the account shortly after —
// a user who completes the purchase should see the row flip without waiting
// out the 30-min ticker.
func (ui *trayUI) upgradeCTA() {
	target, _ := ui.upgradeTarget.Load().(string)
	openURL(target)
	time.AfterFunc(time.Minute, ui.kickAccount)
	time.AfterFunc(5*time.Minute, ui.kickAccount)
}

// clickLoop handles the lifecycle/control items (agent start/stop, plan CTA,
// autostart, self-update, quit). The pure "open a URL/file" items run in
// navLoop so neither select grows past the complexity gate — each item has its
// own channel, so splitting across goroutines is safe.
func (ui *trayUI) clickLoop() {
	for {
		select {
		case <-ui.mPause.ClickedCh:
			markPaused(true)
			ui.control("stop")
		case <-ui.mResume.ClickedCh:
			markPaused(false)
			ui.control("start")
		case <-ui.mRestart.ClickedCh:
			markPaused(false)
			ui.control("daemon", "restart")
		case <-ui.mUpgrade.ClickedCh:
			ui.upgradeCTA()
		case <-ui.mAutostart.ClickedCh:
			toggleAutostart(ui.mAutostart)
		case <-ui.mUpdate.ClickedCh:
			go traySelfUpdate(ui.mUpdate)
		case <-ui.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// navLoop handles the navigation items — each just opens a URL, file, or the
// downloads folder. Split out of clickLoop to keep both selects small.
func (ui *trayUI) navLoop() {
	for {
		select {
		case <-ui.mOpen.ClickedCh:
			openURL(webBase())
		case <-ui.mLibrary.ClickedCh:
			openURL(libraryURL())
		case <-ui.mDownloads.ClickedCh:
			openDownloadsFolder()
		case <-ui.mEnableDownloads.ClickedCh:
			openURL(hubURL()) // player-only → the web installs the full agent
		case <-ui.mConfigure.ClickedCh:
			openURL(hubURL())
		case <-ui.mEdit.ClickedCh:
			openFile(configPath())
		case <-ui.mLogs.ClickedCh:
			openLogs()
		case <-ui.mSendLogs.ClickedCh:
			go sendLogsToSupport()
		case <-ui.mDocs.ClickedCh:
			openURL(docsURL())
		}
	}
}
