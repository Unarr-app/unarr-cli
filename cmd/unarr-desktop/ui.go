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
	"github.com/Unarr-app/unarr-cli/internal/dialog"
	"github.com/Unarr-app/unarr-cli/internal/notify"
	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

type trayUI struct {
	icons map[trayState][]byte
	shown trayState

	mStatus, mAccount, mVersion, mUpgrade *systray.MenuItem
	mPause, mResume, mRestart             *systray.MenuItem
	mLogin                                *systray.MenuItem
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

	// controlFail is the last daemon control that failed, kept so the status
	// row keeps explaining it after the notification is gone (zero value =
	// none). atomic because the supervising goroutine stores it while refresh
	// reads it.
	controlFail atomic.Value // controlFailure

	// accountOK: the last account fetch actually produced an account. False
	// covers both "not signed in" and "could not be read", because in either
	// case the user has no confirmed account and signing in is a way forward.
	// atomic because the account goroutine writes it while refresh reads it.
	accountOK atomic.Bool

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
	// crashes tracks recent crashes so a supervisor's restart loop is reported
	// once rather than once per restart, and named as a loop in the status row.
	crashes crashTracker
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
	ui.controlFail.Store(controlFailure{})

	systray.SetIcon(trayIcon)
	systray.SetTitle("unarr")
	systray.SetTooltip("unarr agent")

	// Left button opens the web app, right button opens this menu — everywhere
	// but macOS, which keeps its native "click opens the menu" gesture. See
	// traytap_other.go / traytap_darwin.go for why the split exists.
	installTapHandler()

	ui.mStatus = systray.AddMenuItem("Checking…", "Agent status")
	ui.mStatus.Disable()
	ui.mAccount = systray.AddMenuItem("Account: …", "Signed-in unarr account")
	ui.mAccount.Disable()
	// Signing in belongs to the account, not to the daemon controls: it sits
	// under the account row it fixes. Hidden until there is no confirmed
	// account — see signInNeeded.
	ui.mLogin = systray.AddMenuItem("Sign in…", "Reconnect this machine to your unarr account")
	ui.mLogin.Hide()
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
	// Rows that only make sense with a daemon: control, account/version, and
	// the download dir + config file (player-only installs neither). The Player
	// submenu + Open unarr.app / library / docs stay in both modes.
	daemon := []*systray.MenuItem{
		ui.mPause, ui.mResume, ui.mRestart, ui.mAccount, ui.mVersion,
		ui.mDownloads, ui.mEdit,
	}
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
	// No daemon here, so there is nothing to sign in for — and the item would
	// otherwise linger if the CLI was removed while auth was failing.
	ui.showLogin(false)
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
	fail, _ := ui.controlFail.Load().(controlFailure)
	st := displayState(s, isPausedMarker(), fail.failed())
	// One authority for the sign-in row: deciding it here, on the ticker, keeps
	// the account goroutine from fighting the renderer over the same item every
	// few seconds.
	ui.showLogin(signInNeeded(ui.accountOK.Load(), st, fail))
	ui.applyState(st)
	ui.setTooltip(trayTooltip(st, s))
	switch st {
	case stateRunning, stateDownloading:
		ui.controlFail.Store(controlFailure{}) // it is up: any past failure is moot
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
		now := time.Now()
		if s.pid != ui.reportedCrashPID && now.After(ui.suppressCrashUntil) {
			ui.reportedCrashPID = s.pid
			ui.crashes.observe(now)
			// A restart loop re-reports one failure: the developers need it
			// once, not once per restart.
			if ui.crashes.shouldReport(now) {
				go handleCrash(s)
			}
		}
		flapping := ui.crashes.flapping(now)
		ui.mStatus.SetTitle(crashStatusTitle(flapping))
		ui.mStatus.SetTooltip(crashStatusTooltip(flapping))
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
	case stateFailed:
		// The red badge already said something is wrong without opening the
		// menu; this says what.
		ui.mStatus.SetTitle(fail.title)
		ui.mStatus.SetTooltip(fail.detail)
		ui.mPause.Disable()
		if fail.authRequired {
			// Every control would fail the same way until the machine is
			// reconnected, so the buttons that cannot work are taken away.
			ui.mResume.Disable()
			ui.mRestart.Disable()
		} else {
			ui.mResume.Enable()
			ui.mRestart.Enable()
		}
	case statePaused:
		ui.mStatus.SetTitle("Agent: paused")
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
	default:
		ui.mStatus.SetTitle("Agent: stopped")
		ui.mStatus.SetTooltip("")
		ui.mPause.Disable()
		ui.mResume.Enable()
		ui.mRestart.Disable()
	}
}

// signInNeeded reports whether the tray should offer to sign in: whenever
// there is no confirmed account, or a control was refused because the
// credential was rejected. "Account: unavailable" counts — the user is told
// something is wrong with their account, so the way to fix it has to be there
// too; offering it and having it fail beats showing a dead end.
func signInNeeded(accountOK bool, st trayState, fail controlFailure) bool {
	if !accountOK {
		return true
	}
	return st == stateFailed && fail.authRequired
}

// showLogin reveals the sign-in action only while it is the thing to do. The
// menu is append-only, so the item is created up front and hidden — the same
// slots pattern the update item uses.
func (ui *trayUI) showLogin(on bool) {
	if on {
		ui.mLogin.Show()
		ui.mLogin.Enable()
		return
	}
	ui.mLogin.Hide()
}

// control execs a daemon command, surfaces spawn errors, and nudges a refresh
// shortly after (the state file updates asynchronously) on top of the ticker.
// Stops/restarts remember the targeted PID so a state file the old daemon
// failed to clean up is recognized as our stop, not a crash.
// action names the command for the user ("start"/"stop"/"restart"). The command
// is supervised on its own goroutine, so a failing one never blocks the click
// loop — and never fails silently the way a bare spawn did.
func (ui *trayUI) control(action string, args ...string) {
	if args[0] == "stop" || args[0] == "daemon" {
		if s := readStatus(); s.running {
			ui.lastStopPID = s.pid
		}
	}
	ui.suppressCrashUntil = time.Now().Add(crashSuppressWindow)
	ui.controlFail.Store(controlFailure{}) // a fresh attempt clears the last failure
	cmd, out, err := startUnarr(args...)
	if err != nil {
		ui.reportControlFailure(action, err)
		return
	}
	go func() {
		if waitErr := awaitControl(cmd, out, watchFor(action)); waitErr != nil {
			ui.reportControlFailure(action, waitErr)
		}
	}()
	time.AfterFunc(1500*time.Millisecond, ui.refresh)
}

// reportControlFailure surfaces a failed control on every surface the tray has,
// because it has no terminal and the user is standing right there having just
// clicked something that did nothing.
//
// A dialog leads: the user asked for this, so a failure has earned the
// interruption (background events stay notifications). Where no dialog program
// exists — a Linux box with neither zenity nor kdialog — an urgent
// notification takes its place, so something always reaches the user. The
// status row and the red-badged icon then keep saying it after either is gone.
func (ui *trayUI) reportControlFailure(action string, err error) {
	fail := describeControlFailure(action, err)
	ui.controlFail.Store(fail)
	ui.refresh() // repaint first: the menu is already right when the dialog appears
	switch dialog.Error("unarr agent", fail.detail) {
	case dialog.SendReport:
		go reportFailureToSupport(action, fail.detail, err)
	case dialog.Unavailable:
		// No dialog program on this box. An urgent notification stays on screen
		// until dismissed, so the failure is not lost; the menu's "Send logs to
		// support" is still there if the user wants to report it.
		notify.SendUrgent("unarr agent", fail.detail)
	case dialog.Dismissed:
	}
}

// reportFailureToSupport hands the developers the failure the user just saw,
// with the logs attached — the point of offering it on the dialog is that a
// failure gets reported while the user is still in front of it, instead of
// dying on their screen.
func reportFailureToSupport(action, detail string, cause error) {
	msg := fmt.Sprintf("Desktop tray: %q failed.\n\nShown to the user: %s\n\nUnderlying error: %v",
		action, detail, cause)
	if err := sendReport("error", msg); err != nil {
		notify.Send("unarr agent", "Could not send the report: "+err.Error())
		return
	}
	notify.Send("unarr agent", "Report sent. Thank you — it helps us fix this.")
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
	if !hasCLI() {
		// The CLI was removed while the (blocking) fetch was in flight — the
		// ticker has already switched to player-only mode and hidden these
		// rows. Applying the result now would re-Show() a stray upgrade CTA
		// that then persists (no further applyMode until the mode flips again).
		return
	}
	var httpErr *agent.HTTPError
	authDead := errors.As(err, &httpErr) &&
		(httpErr.StatusCode == 401 || httpErr.StatusCode == 410)
	switch {
	case errors.Is(err, errNotSignedIn) || authDead:
		ui.mAccount.SetTitle("Account: not signed in")
		ui.mUpgrade.Hide()
		ui.accountShown = false
		ui.accountOK.Store(false)
	case err != nil:
		fmt.Fprintln(os.Stderr, "unarr-desktop: account:", err)
		ui.accountOK.Store(false)
		if !ui.accountShown {
			ui.mAccount.SetTitle("Account: unavailable")
		}
	default:
		ui.accountShown = true
		ui.accountOK.Store(true)
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
			ui.control("stop", "stop")
		case <-ui.mResume.ClickedCh:
			markPaused(false)
			ui.control("start", "start")
		case <-ui.mRestart.ClickedCh:
			markPaused(false)
			ui.control("restart", "daemon", "restart")
		case <-ui.mLogin.ClickedCh:
			// --browser: the flow the tray can actually drive, since it needs
			// no TTY. It opens the browser and waits on a local callback.
			markPaused(false)
			ui.control(signInAction, "login", "--browser")
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
