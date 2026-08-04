package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
)

// withoutTTY makes the interactive guard report "no terminal", the state a
// pipe, a cron job or a CI run is in. Every command must then fail with a
// readable error rather than blocking on input nobody will type.
func withoutTTY(t *testing.T) {
	t.Helper()
	orig := interactiveEnabled
	interactiveEnabled = func() bool { return false }
	t.Cleanup(func() { interactiveEnabled = orig })
}

func withTTY(t *testing.T) {
	t.Helper()
	orig := interactiveEnabled
	interactiveEnabled = func() bool { return true }
	t.Cleanup(func() { interactiveEnabled = orig })
}

func task(id, state string, running bool) control.TaskInfo {
	return control.TaskInfo{ID: id, Title: "Title " + id, State: state, Running: running}
}

// Offering "resume" on a running download (or "pause" on a stopped one) invites
// a pick that answers "not running" — the menu must only list what the action
// can actually do something with.
func TestCandidatesFor(t *testing.T) {
	tasks := []control.TaskInfo{
		task("running-1", "downloading", true),
		task("paused-1", "paused", false),
		task("stopped-1", "stopped", false),
	}

	cases := []struct {
		action string
		want   []string
	}{
		{control.ActionPause, []string{"running-1"}},
		{control.ActionResume, []string{"paused-1", "stopped-1"}},
		{control.ActionCancel, []string{"running-1", "paused-1", "stopped-1"}},
		{control.ActionRetry, []string{"running-1", "paused-1", "stopped-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			got := candidatesFor(tc.action, tasks)
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %d candidates, want %d (%+v)", tc.action, len(got), len(tc.want), got)
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Errorf("%s: candidate %d = %q, want %q", tc.action, i, got[i].ID, id)
				}
			}
		})
	}
}

// "Nothing to pick" has two very different causes, and the user's next move
// differs: there is nothing downloading vs there is nothing at all.
func TestNoCandidatesError(t *testing.T) {
	if err := noCandidatesError(control.ActionPause, 0); !strings.Contains(err.Error(), "no downloads") {
		t.Errorf("empty queue: %v", err)
	}
	if err := noCandidatesError(control.ActionPause, 3); !strings.Contains(err.Error(), "nothing is downloading") {
		t.Errorf("nothing running: %v", err)
	}
	if err := noCandidatesError(control.ActionResume, 3); !strings.Contains(err.Error(), "already running") {
		t.Errorf("all running: %v", err)
	}
}

func TestPickTasks_NoCandidatesIsAnError(t *testing.T) {
	withTTY(t)
	_, ok, err := pickTasks(control.ActionPause, []control.TaskInfo{task("a", "paused", false)})
	if ok {
		t.Fatal("pickTasks reported a selection with no candidates")
	}
	if err == nil {
		t.Fatal("expected an explanatory error")
	}
}

func TestPickerLabel(t *testing.T) {
	running := control.TaskInfo{
		ID: "31ec4169", Title: "Big Movie", State: "downloading", Running: true,
		Progress: 42, TotalBytes: 1 << 30, SpeedBps: 1 << 20,
	}
	label := pickerLabel(running)
	for _, want := range []string{"Big Movie", "downloading", "42%"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q is missing %q", label, want)
		}
	}

	// No title, no filename: the row must still identify the download.
	bare := pickerLabel(control.TaskInfo{ID: "31ec4169-aaaa", State: "stopped"})
	if !strings.Contains(bare, "31ec4169") {
		t.Errorf("bare label %q does not identify the task", bare)
	}
}

func TestPickerTitle(t *testing.T) {
	if got := pickerTitle(control.ActionCancel, 3); !strings.Contains(got, "Stop") || !strings.Contains(got, "3") {
		t.Errorf("title = %q", got)
	}
	if got := pickerTitle(control.ActionPause, 1); !strings.Contains(got, "Pause") {
		t.Errorf("title = %q", got)
	}
}

// --yes exists for scripts; it must never open a prompt.
func TestConfirmDestructive_AssumeYesSkipsPrompt(t *testing.T) {
	withoutTTY(t) // would error if it tried to prompt
	ok, err := confirmDestructive("delete everything?", true)
	if err != nil || !ok {
		t.Fatalf("--yes should confirm without prompting (ok=%v err=%v)", ok, err)
	}
}

// Without a terminal AND without --yes, a destructive action must refuse rather
// than either hanging or silently proceeding.
func TestConfirmDestructive_NoTTYRefuses(t *testing.T) {
	withoutTTY(t)
	ok, err := confirmDestructive("delete everything?", false)
	if ok {
		t.Fatal("confirmed a destructive action with no terminal and no --yes")
	}
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a pointer to --yes", err)
	}
}

func TestConfirmAction_OnlyGatesTheIrreversible(t *testing.T) {
	withoutTTY(t)

	// A plain pause/stop of one download needs no confirmation.
	if ok, err := confirmAction(actionOptions{action: control.ActionPause, taskID: "x"}, 1); !ok || err != nil {
		t.Errorf("pause was gated (ok=%v err=%v)", ok, err)
	}
	if ok, err := confirmAction(actionOptions{action: control.ActionCancel, taskID: "x"}, 1); !ok || err != nil {
		t.Errorf("single stop was gated (ok=%v err=%v)", ok, err)
	}

	// Deleting files and stopping everything are both gated.
	if _, err := confirmAction(actionOptions{action: control.ActionCancel, deleteFiles: true}, 1); err == nil {
		t.Error("--delete was not gated")
	}
	if _, err := confirmAction(actionOptions{action: control.ActionCancel, all: true}, 4); err == nil {
		t.Error("stop --all was not gated")
	}
	// …but pausing everything is reversible, so it is not.
	if ok, err := confirmAction(actionOptions{action: control.ActionPause, all: true}, 4); !ok || err != nil {
		t.Errorf("pause --all was gated (ok=%v err=%v)", ok, err)
	}
}

// With no id, no --all and no terminal, the command must say what to do instead
// of trying to draw a menu into a pipe.
func TestRunDownloadsAction_NoTargetNoTTYExplains(t *testing.T) {
	withEmptyDataDir(t)
	withoutTTY(t)
	seedResumeQueue(t, "31ec4169-aaaa")

	err := runDownloadsAction(context.Background(), actionOptions{
		action: control.ActionCancel, force: true, // offline path
	})
	if err == nil {
		t.Fatal("expected an error with no id, no --all and no terminal")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Fatalf("error does not mention --all: %v", err)
	}
}

// queuedTasks builds resume-store entries the way the daemon persists them.
func queuedTasks(ids ...string) []agent.Task {
	out := make([]agent.Task, 0, len(ids))
	for _, id := range ids {
		out = append(out, agent.Task{ID: id, Title: "Title " + id, Mode: "download"})
	}
	return out
}

// The offline --all path is what a user reaches for when downloads keep coming
// back after every restart.
func TestMatchOfflineTargets_AllNeedsNoTTY(t *testing.T) {
	withoutTTY(t)

	matched, proceed, err := matchOfflineTargets(queuedTasks("a-1", "b-2"), actionOptions{
		action: control.ActionCancel, all: true, assumeYes: true,
	})
	if err != nil || !proceed {
		t.Fatalf("--all offline: proceed=%v err=%v", proceed, err)
	}
	if len(matched) != 2 {
		t.Fatalf("matched %d entries, want 2", len(matched))
	}
}

// stop --all without --yes must ask, and refuse when it cannot.
func TestMatchOfflineTargets_AllWithoutYesRefusesHeadless(t *testing.T) {
	withoutTTY(t)
	_, proceed, err := matchOfflineTargets(queuedTasks("a-1"), actionOptions{
		action: control.ActionCancel, all: true,
	})
	if proceed {
		t.Fatal("stopped everything without a confirmation")
	}
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want the --yes hint", err)
	}
}
