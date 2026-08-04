package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
)

// withEmptyDataDir points the data dir at a temp dir, so there is no daemon
// state file and no resume queue: the "nothing installed / nothing running"
// baseline.
func withEmptyDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	return dir
}

// seedResumeQueue writes tasks into the on-disk resume queue — the file that
// resurrects a download on every daemon start.
func seedResumeQueue(t *testing.T, ids ...string) *agent.ActiveTaskStore {
	t.Helper()
	store := agent.NewActiveTaskStore()
	for _, id := range ids {
		store.Add(agent.Task{ID: id, Title: "Title " + id, Mode: "download"})
	}
	return store
}

func TestControlClient_NoStateFileIsNoDaemon(t *testing.T) {
	withEmptyDataDir(t)
	_, err := controlClient()
	if !errors.Is(err, control.ErrNoDaemon) {
		t.Fatalf("err = %v, want ErrNoDaemon", err)
	}
}

// A daemon from before the control plane must be reported as "no control
// plane", not as a connection failure to port 0.
func TestControlClient_OldDaemonWithoutControlPlane(t *testing.T) {
	dir := withEmptyDataDir(t)
	agent.WriteState(&agent.DaemonState{AgentID: "a", Status: "running", PID: os.Getpid()})
	if _, err := os.Stat(filepath.Join(dir, "unarr", "daemon.state.json")); err != nil {
		t.Fatalf("fixture: state file not written: %v", err)
	}

	_, err := controlClient()
	if !errors.Is(err, control.ErrNoDaemon) {
		t.Fatalf("err = %v, want ErrNoDaemon", err)
	}
	if !strings.Contains(err.Error(), "unarr update") {
		t.Fatalf("error does not tell the user how to fix it: %v", err)
	}
}

func TestControlClient_ReadsPortAndToken(t *testing.T) {
	withEmptyDataDir(t)
	agent.WriteState(&agent.DaemonState{
		AgentID: "a", Status: "running", PID: os.Getpid(),
		ControlPort: 45123, ControlToken: "s3cret",
	})

	client, err := controlClient()
	if err != nil {
		t.Fatalf("controlClient: %v", err)
	}
	if client == nil {
		t.Fatal("controlClient returned no client and no error")
	}
}

// THE recovery path from the incident: the daemon is stopped and a download
// still comes back on every start. `unarr downloads stop --force` must drop it
// from the on-disk queue.
func TestStopOfflineQueue_DropsMatchingEntry(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "31ec4169-aaaa", "99999999-bbbb")

	if err := stopOfflineQueue(context.Background(), actionOptions{action: control.ActionCancel, taskID: "31ec4169"}); err != nil {
		t.Fatalf("stopOfflineQueue: %v", err)
	}

	left := agent.NewActiveTaskStore().Load()
	if len(left) != 1 || left[0].ID != "99999999-bbbb" {
		t.Fatalf("queue after stop = %+v, want only the untouched task", left)
	}
}

func TestStopOfflineQueue_AllClearsEverything(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "aaaa1111", "bbbb2222", "cccc3333")

	if err := stopOfflineQueue(context.Background(), actionOptions{action: control.ActionCancel, all: true, assumeYes: true}); err != nil {
		t.Fatalf("stopOfflineQueue --all: %v", err)
	}
	if left := agent.NewActiveTaskStore().Load(); len(left) != 0 {
		t.Fatalf("queue after --all = %+v", left)
	}
}

func TestStopOfflineQueue_AmbiguousPrefixRefuses(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "31ec4169-aaaa", "31ec4169-bbbb")

	err := stopOfflineQueue(context.Background(), actionOptions{action: control.ActionCancel, taskID: "31ec4169"})
	if err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
	if left := agent.NewActiveTaskStore().Load(); len(left) != 2 {
		t.Fatalf("an ambiguous stop removed entries: %+v", left)
	}
}

func TestStopOfflineQueue_UnknownIDErrors(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "aaaa1111")

	if err := stopOfflineQueue(context.Background(), actionOptions{action: control.ActionCancel, taskID: "zzzz"}); err == nil {
		t.Fatal("stopping a non-existent queued task succeeded")
	}
}

func TestStopOfflineQueue_EmptyQueueIsNotAnError(t *testing.T) {
	withEmptyDataDir(t)
	if err := stopOfflineQueue(context.Background(), actionOptions{action: control.ActionCancel, taskID: "anything"}); err != nil {
		t.Fatalf("an empty queue should not be an error: %v", err)
	}
}

// Without --force, a stop with no daemon must explain the escape hatch rather
// than failing opaquely — this is the exact moment a user is stuck.
func TestRunDownloadsAction_NoDaemonWithoutForceExplains(t *testing.T) {
	withEmptyDataDir(t)

	err := runDownloadsAction(context.Background(), actionOptions{action: control.ActionCancel, taskID: "31ec4169"})
	if err == nil {
		t.Fatal("expected an error with no daemon and no --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error does not point at --force: %v", err)
	}
}

// Pause/resume genuinely need a daemon; say so instead of pretending.
func TestRunDownloadsAction_NoDaemonPauseIsRefused(t *testing.T) {
	withEmptyDataDir(t)

	err := runDownloadsAction(context.Background(), actionOptions{action: control.ActionPause, taskID: "31ec4169", force: true})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v, want a clear \"daemon is not running\"", err)
	}
}

func TestRunDownloadsAction_ForceStopsOffline(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "31ec4169-aaaa")

	if err := runDownloadsAction(context.Background(), actionOptions{action: control.ActionCancel, taskID: "31ec4169", force: true}); err != nil {
		t.Fatalf("forced offline stop: %v", err)
	}
	if left := agent.NewActiveTaskStore().Load(); len(left) != 0 {
		t.Fatalf("forced stop left the entry behind: %+v", left)
	}
}

func TestRunDownloadsPurge_ForceClearsQueueOffline(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "aaaa1111", "bbbb2222")

	if err := runDownloadsPurge(context.Background(), true); err != nil {
		t.Fatalf("purge --force: %v", err)
	}
	if left := agent.NewActiveTaskStore().Load(); len(left) != 0 {
		t.Fatalf("purge --force left %d entries", len(left))
	}
}

func TestRunDownloadsPurge_NoForceExplains(t *testing.T) {
	withEmptyDataDir(t)
	err := runDownloadsPurge(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want the --force hint", err)
	}
}

// Listing with no daemon must still show the resume queue: those are precisely
// the downloads that will start again, and the user needs to see them.
func TestRunDownloadsList_ShowsOfflineQueue(t *testing.T) {
	withEmptyDataDir(t)
	seedResumeQueue(t, "31ec4169-aaaa")

	if err := runDownloadsList(context.Background(), false); err != nil {
		t.Fatalf("list with no daemon: %v", err)
	}
}

func TestFirstArg(t *testing.T) {
	if got := firstArg(nil); got != "" {
		t.Errorf("no args should yield an empty id, got %q", got)
	}
	if got := firstArg([]string{"31ec4169"}); got != "31ec4169" {
		t.Errorf("firstArg = %q", got)
	}
}

// `unarr download stop …` is the phrasing users reach for; it must work.
func TestDownloadAliases_AreRegistered(t *testing.T) {
	cmd := newDownloadCmd()
	addDownloadAliases(cmd)

	for _, name := range []string{"stop", "pause", "resume", "retry", "purge", "list"} {
		sub, _, err := cmd.Find([]string{name})
		if err != nil || sub == nil || sub.Name() != name {
			t.Errorf("`unarr download %s` did not resolve (got %v, err %v)", name, sub, err)
		}
		if sub != nil && !sub.Hidden {
			t.Errorf("alias %q should be hidden so `download --help` stays about the one-shot downloader", name)
		}
	}

	// The one-shot download must still accept a bare hash.
	if sub, args, err := cmd.Find([]string{"abc123"}); err != nil || sub != cmd || len(args) != 1 {
		t.Errorf("a bare hash no longer reaches the download command (sub=%v args=%v err=%v)", sub, args, err)
	}
}

func TestNewDownloadsCmd_Structure(t *testing.T) {
	cmd := newDownloadsCmd()
	if cmd.Name() != "downloads" {
		t.Fatalf("command name = %q", cmd.Name())
	}
	for _, name := range []string{"list", "pause", "resume", "stop", "retry", "purge"} {
		if sub, _, err := cmd.Find([]string{name}); err != nil || sub.Name() != name {
			t.Errorf("subcommand %q missing", name)
		}
	}
	// `cancel` is the word half the world uses for stop.
	if sub, _, err := cmd.Find([]string{"cancel"}); err != nil || sub.Name() != "stop" {
		t.Errorf("`downloads cancel` should alias stop, got %v", sub)
	}
}

func TestUnwrapNoDaemon(t *testing.T) {
	err := errors.New(control.ErrNoDaemon.Error() + ": state file not found")
	if got := unwrapNoDaemon(err); got != "state file not found" {
		t.Fatalf("unwrapNoDaemon = %q", got)
	}
}

func TestShortLabel(t *testing.T) {
	if got := shortLabel("31ec4169-1111", "Movie"); !strings.Contains(got, "31ec4169") || !strings.Contains(got, "Movie") {
		t.Fatalf("shortLabel = %q", got)
	}
	if got := shortLabel("31ec4169-1111", ""); strings.Contains(got, "  ") {
		t.Fatalf("shortLabel with no title has stray padding: %q", got)
	}
}
