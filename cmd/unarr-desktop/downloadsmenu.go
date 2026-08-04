package main

// The Downloads section of the tray menu: one row per active download, each
// with Pause / Resume / Stop under it.
//
// Why it exists: until now the only way to stop a download was the website, and
// the website can only reach a download through its task row. Delete that row
// (cancel, then remove from the list) and the download becomes unreachable —
// the agent keeps it in its resume store and restarts it on every boot. The
// tray talks to the daemon's local control plane instead, so the machine
// running the download can always stop it. Actions are reported back to the
// web, so the dashboard follows within a couple of seconds.
//
// systray menus are append-only: items cannot be created per tick. So the rows
// are pre-created hidden ("slots", the same pattern as the update item) and
// each refresh binds the visible ones to whatever the daemon reports.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
	"github.com/Unarr-app/unarr-cli/internal/notify"
)

// downloadSlots is how many downloads get their own row. Beyond this the menu
// stops being a menu; the overflow row points at `unarr downloads`, which has
// no such limit. Five covers the default max_concurrent (3) with headroom.
const downloadSlots = 5

// controlCallTimeout bounds a tray-initiated control call. The tray must never
// hang on a wedged daemon — the click loop is what keeps the whole menu alive.
const controlCallTimeout = 10 * time.Second

// downloadSlot is one pre-created menu row plus its action submenu.
type downloadSlot struct {
	item    *systray.MenuItem
	pause   *systray.MenuItem
	resume  *systray.MenuItem
	stop    *systray.MenuItem
	stopDel *systray.MenuItem

	// taskID is what the submenu acts on, rewritten on every refresh. Atomic:
	// the render goroutine writes it while a click goroutine reads it.
	taskID atomic.Value // string
}

// downloadsMenu owns the section: a header, the slots, and an overflow row.
type downloadsMenu struct {
	header   *systray.MenuItem
	slots    []*downloadSlot
	overflow *systray.MenuItem

	// shownTitles diffs each row so an unchanged tick does not spam the host
	// (a SetTitle per row per 5s tick over DBus is exactly the noise applyState
	// and setTooltip already avoid).
	shownTitles []string
}

func (s *downloadSlot) currentTaskID() string {
	v, _ := s.taskID.Load().(string)
	return v
}

// buildDownloadsMenu creates the section. Called from newTrayUI, in menu order.
func buildDownloadsMenu() *downloadsMenu {
	m := &downloadsMenu{
		shownTitles: make([]string, downloadSlots),
	}
	m.header = systray.AddMenuItem("Downloads", "Downloads running on this machine")
	m.header.Disable()
	m.header.Hide()

	for i := 0; i < downloadSlots; i++ {
		item := systray.AddMenuItem("", "")
		slot := &downloadSlot{
			item:    item,
			pause:   item.AddSubMenuItem("Pause", "Pause this download (keeps the partial file)"),
			resume:  item.AddSubMenuItem("Resume", "Continue this download"),
			stop:    item.AddSubMenuItem("Stop", "Cancel this download and keep the partial file"),
			stopDel: item.AddSubMenuItem("Stop and delete files", "Cancel this download and delete the partial file"),
		}
		slot.taskID.Store("")
		item.Hide()
		m.slots = append(m.slots, slot)
	}

	m.overflow = systray.AddMenuItem("", "Run `unarr downloads` to see them all")
	m.overflow.Disable()
	m.overflow.Hide()
	return m
}

// hideAll collapses the section — no daemon, no control plane, or nothing
// downloading. Called from the render path (holds renderMu), so it must not
// block.
func (m *downloadsMenu) hideAll() {
	m.header.Hide()
	m.overflow.Hide()
	for i, s := range m.slots {
		s.item.Hide()
		s.taskID.Store("")
		m.shownTitles[i] = ""
	}
}

// render binds the reported tasks to the slots. Returns the number of rows
// shown so the caller can tell "no downloads" from "no daemon".
func (m *downloadsMenu) render(tasks []control.TaskInfo) int {
	if len(tasks) == 0 {
		m.hideAll()
		return 0
	}

	m.header.Show()
	shown := 0
	for i, slot := range m.slots {
		if i >= len(tasks) {
			slot.item.Hide()
			slot.taskID.Store("")
			m.shownTitles[i] = ""
			continue
		}
		t := tasks[i]
		title := downloadRowTitle(t)
		if m.shownTitles[i] != title {
			slot.item.SetTitle(title)
			slot.item.SetTooltip(downloadRowTooltip(t))
			m.shownTitles[i] = title
		}
		slot.taskID.Store(t.ID)
		// A running download cannot be resumed and a stopped one cannot be
		// paused; showing both makes the menu lie about what will happen.
		if t.Running {
			slot.pause.Show()
			slot.resume.Hide()
		} else {
			slot.pause.Hide()
			slot.resume.Show()
		}
		slot.item.Show()
		shown++
	}

	if extra := len(tasks) - downloadSlots; extra > 0 {
		m.overflow.SetTitle(fmt.Sprintf("…and %d more — run `unarr downloads`", extra))
		m.overflow.Show()
	} else {
		m.overflow.Hide()
	}
	return shown
}

// downloadRowTitle is the row label: state and progress first (that is what the
// user scans for), then as much of the title as fits.
func downloadRowTitle(t control.TaskInfo) string {
	name := t.Title
	if name == "" {
		name = t.FileName
	}
	if name == "" {
		name = agent.ShortID(t.ID)
	}
	name = ellipsize(name, 42)

	if t.Running && t.TotalBytes > 0 {
		return fmt.Sprintf("%d%%  %s", t.Progress, name)
	}
	// No size yet, or not running at all: the state is the useful part.
	return fmt.Sprintf("%s  %s", t.State, name)
}

func downloadRowTooltip(t control.TaskInfo) string {
	if t.ErrorMessage != "" {
		return t.ErrorMessage
	}
	if t.Method != "" {
		return fmt.Sprintf("%s via %s — %s", t.State, t.Method, agent.ShortID(t.ID))
	}
	return fmt.Sprintf("%s — %s", t.State, agent.ShortID(t.ID))
}

func ellipsize(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// fetchDownloads asks the daemon what it is working on. A missing control plane
// is not an error worth surfacing on every tick — the section just stays
// hidden, exactly as it did before this feature existed.
func fetchDownloads() []control.TaskInfo {
	client, err := control.Discover()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	tasks, err := client.List(ctx)
	if err != nil {
		return nil
	}
	return sortForMenu(tasks)
}

// sortForMenu puts running downloads first: with only five slots, an idle
// leftover must never push a live download out of the menu.
func sortForMenu(tasks []control.TaskInfo) []control.TaskInfo {
	out := make([]control.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		if t.Running {
			out = append(out, t)
		}
	}
	for _, t := range tasks {
		if !t.Running {
			out = append(out, t)
		}
	}
	return out
}

// runControlAction performs one action and reports the outcome to the user.
// Runs off the click loop: the call crosses the network stack and the daemon
// may be busy dropping a torrent handle, and a blocked click loop freezes the
// whole menu.
func runControlAction(action, taskID string, deleteFiles bool, notifyFn func(title, body string)) {
	client, err := control.Discover()
	if err != nil {
		notifyFn("unarr", "The agent is not running, so the download cannot be "+actionVerb(action)+".")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()

	results, err := client.Do(ctx, action, control.ActionRequest{TaskID: taskID, DeleteFiles: deleteFiles})
	if err != nil {
		notifyFn("unarr", fmt.Sprintf("Could not %s the download: %v", actionVerb(action), err))
		return
	}
	if len(results) == 0 {
		notifyFn("unarr", "That download is no longer in the queue.")
		return
	}
	r := results[0]
	label := r.Title
	if label == "" {
		label = agent.ShortID(r.TaskID)
	}
	notifyFn("unarr", fmt.Sprintf("%s — %s", label, r.Message))
}

// startDownloadWatchers arms the per-row submenu handlers. Called from onReady
// alongside the other loops; notifications carry the outcome because the menu
// is closed by the time an action finishes.
func (ui *trayUI) startDownloadWatchers() {
	if ui.downloads == nil {
		return
	}
	ui.downloads.watchClicks(notify.Send)
}

// watchClicks runs one goroutine per slot. A goroutine per row rather than one
// big select because systray gives every item its own channel and the set is
// fixed at build time — and because a control call must not sit in the main
// click loop (see runControlAction).
func (m *downloadsMenu) watchClicks(notifyFn func(title, body string)) {
	for _, slot := range m.slots {
		go slot.watch(notifyFn)
	}
}

func (s *downloadSlot) watch(notifyFn func(title, body string)) {
	act := func(action string, deleteFiles bool) {
		id := s.currentTaskID()
		if id == "" {
			// The row was rebound (or emptied) between the click and here.
			// Acting on a stale id would stop somebody else's download.
			return
		}
		go runControlAction(action, id, deleteFiles, notifyFn)
	}
	for {
		select {
		case <-s.pause.ClickedCh:
			act(control.ActionPause, false)
		case <-s.resume.ClickedCh:
			act(control.ActionResume, false)
		case <-s.stop.ClickedCh:
			act(control.ActionCancel, false)
		case <-s.stopDel.ClickedCh:
			act(control.ActionCancel, true)
		}
	}
}

func actionVerb(action string) string {
	switch action {
	case control.ActionPause:
		return "paused"
	case control.ActionResume:
		return "resumed"
	case control.ActionCancel:
		return "stopped"
	case control.ActionRetry:
		return "retried"
	default:
		return action + "ed"
	}
}
