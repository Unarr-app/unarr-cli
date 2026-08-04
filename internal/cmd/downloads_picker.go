package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/control"
	"github.com/charmbracelet/huh"
	"github.com/dustin/go-humanize"
	"golang.org/x/term"
)

// Interactive target selection for `unarr downloads <action>` with no id.
//
// Asking a user to copy an id out of one command and paste it into the next is
// busywork at the best of times, and this is not the best of times: they reach
// for these commands when a download is misbehaving. So a bare `unarr downloads
// stop` lists what is running and lets them tick what to act on — including
// everything at once.

// interactiveEnabled reports whether we can prompt at all. A pipe, a cron job
// or a CI run must fail with a readable error instead of blocking forever on a
// terminal that will never answer.
var interactiveEnabled = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// pickTasks prompts for the downloads an action applies to, returning their
// full ids. ok=false means the user aborted (Ctrl-C / Esc), which is not an
// error — nothing happens and the command exits quietly.
func pickTasks(action string, tasks []control.TaskInfo) (ids []string, ok bool, err error) {
	candidates := candidatesFor(action, tasks)
	if len(candidates) == 0 {
		return nil, false, noCandidatesError(action, len(tasks))
	}

	// One candidate: a tick-list of one is a trap. The natural key to press is
	// enter, which in a multi-select confirms an EMPTY selection — the command
	// then exits silently having done nothing, which reads as a broken feature
	// (reported on the first real run of this menu). Ask a plain yes/no instead.
	if len(candidates) == 1 {
		yes, err := confirmDestructive(
			fmt.Sprintf("%s  %s?", pickerVerb(action), pickerLabel(candidates[0])), false)
		if err != nil || !yes {
			return nil, false, err
		}
		return []string{candidates[0].ID}, true, nil
	}

	opts := make([]huh.Option[string], 0, len(candidates))
	for _, t := range candidates {
		opts = append(opts, huh.NewOption(pickerLabel(t), t.ID))
	}

	var selected []string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title(pickerTitle(action, len(candidates))).
				Description("SPACE to tick · enter to confirm · esc to cancel").
				Options(opts...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(selected) == 0 {
		// Enter with nothing ticked. Say so — silence here looks like a bug.
		fmt.Println("  Nothing ticked (use SPACE to tick) — no downloads were changed.")
		return nil, false, nil
	}
	return selected, true, nil
}

// candidatesFor narrows the list to the downloads the action can actually do
// something with. Offering "resume" on a running download (or "pause" on a
// stopped one) invites a click that reports "not running" and teaches the user
// the tool is unreliable.
func candidatesFor(action string, tasks []control.TaskInfo) []control.TaskInfo {
	var out []control.TaskInfo
	for _, t := range tasks {
		switch action {
		case control.ActionPause:
			if t.Running {
				out = append(out, t)
			}
		case control.ActionResume:
			if !t.Running {
				out = append(out, t)
			}
		default: // cancel, retry — valid for anything the agent knows about
			out = append(out, t)
		}
	}
	return out
}

// noCandidatesError explains WHY there is nothing to pick: "no downloads at
// all" and "none in a state this action applies to" call for different fixes.
func noCandidatesError(action string, total int) error {
	if total == 0 {
		return errors.New("there are no downloads")
	}
	switch action {
	case control.ActionPause:
		return errors.New("nothing is downloading right now — `unarr downloads` shows the queue")
	case control.ActionResume:
		return errors.New("every download is already running — `unarr downloads` shows the queue")
	default:
		return errors.New("there are no downloads to act on")
	}
}

// pickerVerb is the action as a user-facing word.
func pickerVerb(action string) string {
	verb := map[string]string{
		control.ActionPause:  "Pause",
		control.ActionResume: "Resume",
		control.ActionCancel: "Stop",
		control.ActionRetry:  "Retry",
	}[action]
	if verb == "" && action != "" {
		verb = strings.ToUpper(action[:1]) + action[1:]
	}
	return verb
}

func pickerTitle(action string, n int) string {
	return fmt.Sprintf("%s which download? (%d available)", pickerVerb(action), n)
}

// pickerLabel is one row of the menu: enough to tell two downloads of the same
// show apart without wrapping a normal terminal.
func pickerLabel(t control.TaskInfo) string {
	name := t.Title
	if name == "" {
		name = t.FileName
	}
	if name == "" {
		name = t.ID
	}

	detail := t.State
	if t.Running && t.TotalBytes > 0 {
		detail = fmt.Sprintf("%s %d%% of %s", t.State, t.Progress, humanize.Bytes(uint64(t.TotalBytes)))
		if t.SpeedBps > 0 {
			detail += fmt.Sprintf(" @ %s/s", humanize.Bytes(uint64(t.SpeedBps)))
		}
	}
	return fmt.Sprintf("%s  —  %s", truncateTitle(name, 52), detail)
}

// confirmDestructive asks before an action that cannot be walked back: deleting
// partial files, or hitting every download at once. --yes skips it, for scripts
// and for users who already know.
func confirmDestructive(prompt string, assumeYes bool) (bool, error) {
	if assumeYes {
		return true, nil
	}
	if !interactiveEnabled() {
		return false, errors.New("refusing to run without a terminal to confirm on — re-run with --yes")
	}

	var ok bool
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().Title(prompt).Affirmative("Yes").Negative("No").Value(&ok),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}
