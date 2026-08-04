package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/control"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// newDownloadsCmd builds `unarr downloads` — the local control surface for the
// download queue.
//
// It exists because every other way to stop a download goes through the server:
// the web sends cancel/pause on the task row, and when that row is gone (the
// user cancelled and then removed the entry) nothing can reach the agent, which
// keeps re-submitting the download from its resume store on every start. These
// commands talk to the daemon over loopback instead, and when even the daemon
// is down they edit the resume store directly — so there is always a way out.
func newDownloadsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "downloads",
		Aliases: []string{"dl", "queue"},
		Short:   "List and control the download queue (pause, resume, stop, retry)",
		Long: `Show what the daemon is downloading and control it locally.

Run an action with no id and it shows a menu of the downloads that action can
apply to — tick the ones you want with space, confirm with enter. --all skips
the menu and hits every download.

Every action here also reports to the web, so the dashboard reflects what you
did from the terminal (or from the desktop tray) within a few seconds.

Task ids may be abbreviated to any unique prefix — the 8-character ids printed
by this command and by the logs are enough.

When the daemon is not running, 'list', 'stop' and 'purge' still work: they read
and edit the on-disk resume queue, which is what would otherwise bring a
download back on the next start.`,
		Example: `  unarr downloads                     # what is running right now
  unarr downloads pause               # pick from a menu
  unarr downloads pause --all         # pause everything
  unarr downloads stop                # pick what to stop
  unarr downloads stop --all          # stop everything (asks first)
  unarr downloads pause 31ec4169      # or name it directly
  unarr downloads resume 31ec4169
  unarr downloads stop 31ec4169 --delete
  unarr downloads stop --all --force  # stop everything, daemon up or not
  unarr downloads retry 31ec4169
  unarr downloads purge               # forget queued leftovers (zombies)`,
		// Without this, cobra hands an unknown verb to the parent as a positional
		// argument and this command quietly lists the queue instead — so
		// `unarr downloads start` looked like it worked and did nothing.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownloadsList(cmd.Context(), jsonOut)
		},
	}

	cmd.AddCommand(
		newDownloadsListCmd(),
		newDownloadsActionCmd(control.ActionPause, "pause <id>", "Pause a download (keeps partial files)"),
		newDownloadsActionCmd(control.ActionResume, "resume <id>", "Resume a paused download"),
		newDownloadsStopCmd(),
		newDownloadsActionCmd(control.ActionRetry, "retry <id>", "Restart a download from the beginning of its current attempt"),
		newDownloadsPurgeCmd(),
	)
	return cmd
}

func newDownloadsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active and queued downloads",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownloadsList(cmd.Context(), jsonOut)
		},
	}
}

// actionAliases are the other words people reach for. "start" in particular:
// it is the natural opposite of the "stop" right next to it in the help, and
// typing it used to silently list the queue instead of resuming anything.
var actionAliases = map[string][]string{
	control.ActionResume: {"start", "continue", "unpause"},
	control.ActionPause:  {"hold"},
	control.ActionRetry:  {"restart"},
}

// newDownloadsActionCmd builds the plain one-target actions. stop and purge get
// their own constructors because they carry extra flags.
func newDownloadsActionCmd(action, use, short string) *cobra.Command {
	opts := actionOptions{action: action}
	cmd := &cobra.Command{
		Use:     use,
		Aliases: actionAliases[action],
		Short:   short,
		Long: short + `.

With no id, it lists the downloads this action applies to and lets you tick the
ones you want. --all skips the menu and hits every one of them.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.taskID = firstArg(args)
			return runDownloadsAction(cmd.Context(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.all, "all", false, "apply to every download, no menu")
	cmd.Flags().BoolVarP(&opts.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func newDownloadsStopCmd() *cobra.Command {
	opts := actionOptions{action: control.ActionCancel}
	cmd := &cobra.Command{
		Use:     "stop [id]",
		Aliases: []string{"cancel"},
		Short:   "Stop a download for good (and stop it coming back after a restart)",
		Long: `Cancel a download and drop it from the resume queue.

With no id, it lists what the agent is working on and lets you tick what to
stop. --all stops everything without asking which.

Partial files are kept unless --delete is given.

--force also works with the daemon stopped: it edits the on-disk resume queue,
which is what resurrects a download on the next start. Use it when a download
survives cancelling from the web — that means the task row is gone and the
server has no way left to reach the agent.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.taskID = firstArg(args)
			return runDownloadsAction(cmd.Context(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.all, "all", false, "stop every download, no menu")
	cmd.Flags().BoolVar(&opts.deleteFiles, "delete", false, "also delete the partial files from disk")
	cmd.Flags().BoolVar(&opts.force, "force", false, "edit the resume queue directly if the daemon is not reachable")
	cmd.Flags().BoolVarP(&opts.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func newDownloadsPurgeCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Forget queued downloads that are not running (zombie cleanup)",
		Long: `Drop every entry from the resume queue that is not currently downloading.

The resume queue is what lets an interrupted download continue after a restart.
When a download was cancelled on the website and its entry removed, the server
can no longer tell the agent to stop, so the entry keeps restarting it forever.
Purging is how that loop ends.

Running downloads are never touched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownloadsPurge(cmd.Context(), force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "edit the resume queue directly if the daemon is not reachable")
	return cmd
}

// addDownloadAliases hangs the queue-control verbs off `unarr download` too.
// Users say "unarr download stop", and being told "no such command, it's
// `downloads`" while a runaway download keeps eating disk is the wrong answer.
// The subcommands are hidden so `unarr download --help` still reads as the
// one-shot downloader it is, with a pointer to the real command.
func addDownloadAliases(downloadCmd *cobra.Command) {
	for _, sub := range []*cobra.Command{
		newDownloadsListCmd(),
		newDownloadsActionCmd(control.ActionPause, "pause <id>", "Pause a download (see `unarr downloads`)"),
		newDownloadsActionCmd(control.ActionResume, "resume <id>", "Resume a paused download (see `unarr downloads`)"),
		newDownloadsStopCmd(),
		newDownloadsActionCmd(control.ActionRetry, "retry <id>", "Restart a download (see `unarr downloads`)"),
		newDownloadsPurgeCmd(),
	} {
		sub.Hidden = true
		downloadCmd.AddCommand(sub)
	}
}

// actionOptions is one control invocation. Grouped into a struct because the
// five booleans that used to be positional parameters read identically at the
// call site, and swapping two of them silently turns "stop this one" into
// "delete everything".
type actionOptions struct {
	action      string
	taskID      string
	all         bool
	deleteFiles bool
	force       bool
	assumeYes   bool
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// controlClient builds a client for the running daemon's control plane.
// Returns control.ErrNoDaemon when there is no daemon, or when the daemon
// predates the control plane — both mean "fall back to the offline path".
func controlClient() (*control.Client, error) {
	return control.Discover()
}

func runDownloadsList(ctx context.Context, asJSON bool) error {
	ctx, cancel := withDownloadsTimeout(ctx)
	defer cancel()

	client, err := controlClient()
	if err != nil {
		if !errors.Is(err, control.ErrNoDaemon) {
			return err
		}
		return listOfflineQueue(asJSON, err)
	}

	tasks, err := client.List(ctx)
	if err != nil {
		if errors.Is(err, control.ErrNoDaemon) {
			return listOfflineQueue(asJSON, err)
		}
		return err
	}
	if asJSON {
		return printJSON(tasks)
	}
	printTaskTable(tasks, "")
	return nil
}

// listOfflineQueue shows the on-disk resume queue when there is no daemon to
// ask. These are exactly the downloads that would start again on the next
// `unarr start`, so they are worth showing even (especially) when nothing runs.
func listOfflineQueue(asJSON bool, reason error) error {
	store := agent.NewActiveTaskStore()
	queued := store.Load()

	tasks := make([]control.TaskInfo, 0, len(queued))
	for _, t := range queued {
		tasks = append(tasks, control.TaskInfo{
			ID:        t.ID,
			Title:     t.Title,
			State:     "queued",
			Persisted: true,
		})
	}
	if asJSON {
		return printJSON(tasks)
	}
	printTaskTable(tasks, fmt.Sprintf("daemon not reachable (%v) — showing the on-disk resume queue", unwrapNoDaemon(reason)))
	return nil
}

func runDownloadsAction(ctx context.Context, opts actionOptions) error {
	ctx, cancel := withDownloadsTimeout(ctx)
	defer cancel()

	client, err := controlClient()
	if err == nil {
		return runDownloadsActionOnline(ctx, client, opts)
	}
	if !errors.Is(err, control.ErrNoDaemon) {
		return err
	}

	// No daemon. Only a stop can be honoured offline — and only against the
	// resume queue, which is the part that would restart the download.
	if opts.action != control.ActionCancel {
		return fmt.Errorf("the daemon is not running, so there is nothing to %s (%v).\nStart it with `unarr start`, or use `unarr downloads stop --force` to drop queued downloads", opts.action, unwrapNoDaemon(err))
	}
	if !opts.force {
		return fmt.Errorf("the daemon is not running (%v).\nRe-run with --force to drop the download from the on-disk resume queue so it does not start again", unwrapNoDaemon(err))
	}
	return stopOfflineQueue(ctx, opts)
}

// runDownloadsActionOnline drives the daemon's control plane: work out WHICH
// downloads (an id, everything, or whatever the user ticks in the menu),
// confirm anything irreversible, then apply.
func runDownloadsActionOnline(ctx context.Context, client *control.Client, opts actionOptions) error {
	targets, proceed, err := resolveTargets(ctx, client, opts)
	if err != nil || !proceed {
		return err
	}

	ok, err := confirmAction(opts, len(targets))
	if err != nil || !ok {
		if err == nil {
			fmt.Println("  Cancelled — nothing was changed.")
		}
		return err
	}

	// One request per explicit target keeps the per-download result (and its
	// failure) attached to the download it belongs to.
	var results []control.ActionResult
	if len(targets) == 0 { // --all: let the daemon expand it, so a download that
		// started a second ago is included too
		results, err = client.Do(ctx, opts.action, control.ActionRequest{All: true, DeleteFiles: opts.deleteFiles})
		if err != nil {
			return err
		}
	} else {
		for _, id := range targets {
			res, derr := client.Do(ctx, opts.action, control.ActionRequest{TaskID: id, DeleteFiles: opts.deleteFiles})
			if derr != nil {
				return derr
			}
			results = append(results, res...)
		}
	}

	printActionResults(opts.action, results)
	return nil
}

// resolveTargets turns the invocation into concrete ids. An empty slice with
// proceed=true means "--all": the daemon expands it. proceed=false means the
// user backed out of the menu.
func resolveTargets(ctx context.Context, client *control.Client, opts actionOptions) (ids []string, proceed bool, err error) {
	if opts.all {
		return nil, true, nil
	}
	if opts.taskID != "" {
		return []string{opts.taskID}, true, nil
	}

	// No id and no --all: show what this action can act on and let the user tick.
	if !interactiveEnabled() {
		return nil, false, fmt.Errorf("give a download id (see `unarr downloads`), or use --all — there is no terminal here to show the menu on")
	}
	tasks, err := client.List(ctx)
	if err != nil {
		return nil, false, err
	}
	picked, ok, err := pickTasks(opts.action, tasks)
	if err != nil || !ok {
		return nil, false, err
	}
	return picked, true, nil
}

// confirmAction gates the two irreversible shapes: deleting partial files, and
// hitting every download at once.
func confirmAction(opts actionOptions, targetCount int) (bool, error) {
	switch {
	case opts.deleteFiles:
		what := fmt.Sprintf("%d download(s)", targetCount)
		if opts.all {
			what = "EVERY download"
		}
		return confirmDestructive(
			fmt.Sprintf("Stop %s and DELETE the partial files from disk?", what), opts.assumeYes)
	case opts.all && opts.action == control.ActionCancel:
		return confirmDestructive("Stop every download? They will not resume by themselves.", opts.assumeYes)
	default:
		return true, nil
	}
}

// stopOfflineQueue removes entries from the resume queue with no daemon around.
// Files are never deleted here: without the daemon we do not know which paths
// belong to the task, and guessing is how you delete somebody's library.
func stopOfflineQueue(_ context.Context, opts actionOptions) error {
	if opts.deleteFiles {
		color.New(color.FgYellow).Println("  ⚠  --delete needs the daemon: dropping the queue entry only, partial files are left on disk.")
	}
	store := agent.NewActiveTaskStore()
	queued := store.Load()
	if len(queued) == 0 {
		fmt.Println("  Nothing in the resume queue.")
		return nil
	}

	matched, proceed, err := matchOfflineTargets(queued, opts)
	if err != nil || !proceed {
		return err
	}

	for _, t := range matched {
		store.Remove(t.ID)
		fmt.Printf("  %s  %s — dropped from the resume queue\n",
			color.New(color.FgGreen).Sprint("✓"), shortLabel(t.ID, t.Title))
	}
	fmt.Println()
	color.New(color.FgHiBlack).Println("  It will not start again. If a daemon is running, restart it so it forgets the task too.")
	return nil
}

// matchOfflineTargets picks the queue entries to drop: by id, all of them, or
// whatever the user ticks. proceed=false means they backed out.
func matchOfflineTargets(queued []agent.Task, opts actionOptions) (matched []agent.Task, proceed bool, err error) {
	if opts.taskID == "" && !opts.all {
		if !interactiveEnabled() {
			return nil, false, errors.New("give a download id (see `unarr downloads`), or use --all — there is no terminal here to show the menu on")
		}
		tasks := make([]control.TaskInfo, 0, len(queued))
		for _, t := range queued {
			tasks = append(tasks, control.TaskInfo{ID: t.ID, Title: t.Title, State: "queued", Persisted: true})
		}
		picked, ok, perr := pickTasks(opts.action, tasks)
		if perr != nil || !ok {
			return nil, false, perr
		}
		chosen := make(map[string]bool, len(picked))
		for _, id := range picked {
			chosen[id] = true
		}
		for _, t := range queued {
			if chosen[t.ID] {
				matched = append(matched, t)
			}
		}
		return matched, true, nil
	}

	for _, t := range queued {
		if opts.all || t.ID == opts.taskID || strings.HasPrefix(t.ID, opts.taskID) {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return nil, false, fmt.Errorf("no queued download matches %q", opts.taskID)
	}
	if !opts.all && len(matched) > 1 {
		return nil, false, fmt.Errorf("%q matches %d queued downloads — use the full id", opts.taskID, len(matched))
	}

	ok, err := confirmAction(opts, len(matched))
	if err != nil || !ok {
		if err == nil {
			fmt.Println("  Cancelled — nothing was changed.")
		}
		return nil, false, err
	}
	return matched, true, nil
}

func runDownloadsPurge(ctx context.Context, force bool) error {
	ctx, cancel := withDownloadsTimeout(ctx)
	defer cancel()

	client, err := controlClient()
	if err == nil {
		var results []control.ActionResult
		results, err = client.Do(ctx, control.ActionPurge, control.ActionRequest{})
		if err == nil {
			if len(results) == 0 {
				fmt.Println("  Nothing to purge — no stopped downloads in the resume queue.")
				return nil
			}
			printActionResults(control.ActionPurge, results)
			return nil
		}
	}
	if !errors.Is(err, control.ErrNoDaemon) {
		return err
	}
	if !force {
		return fmt.Errorf("the daemon is not running (%v).\nRe-run with --force to clear the on-disk resume queue", unwrapNoDaemon(err))
	}

	store := agent.NewActiveTaskStore()
	store.Load()
	n := store.Clear()
	fmt.Printf("  %s  cleared %d queued download(s) from the resume queue\n",
		color.New(color.FgGreen).Sprint("✓"), n)
	return nil
}

// unwrapNoDaemon strips the ErrNoDaemon prefix so the printed reason reads as a
// cause ("state file not found") rather than repeating the headline.
func unwrapNoDaemon(err error) string {
	msg := err.Error()
	prefix := control.ErrNoDaemon.Error() + ": "
	return strings.TrimPrefix(msg, prefix)
}

func shortLabel(id, title string) string {
	label := agent.ShortID(id)
	if title != "" {
		label += "  " + title
	}
	return label
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printTaskTable(tasks []control.TaskInfo, notice string) {
	fmt.Println()
	if notice != "" {
		color.New(color.FgYellow).Printf("  %s\n\n", notice)
	}
	if len(tasks) == 0 {
		color.New(color.FgHiBlack).Println("  No downloads.")
		fmt.Println()
		return
	}

	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)
	bold.Printf("  %-10s %-13s %6s %10s %12s  %s\n", "ID", "STATE", "PROG", "SPEED", "SIZE", "TITLE")
	for _, t := range tasks {
		speed := "—"
		if t.SpeedBps > 0 {
			speed = humanize.Bytes(uint64(t.SpeedBps)) + "/s"
		}
		size := "—"
		if t.TotalBytes > 0 {
			size = humanize.Bytes(uint64(t.TotalBytes))
		}
		title := t.Title
		if title == "" {
			title = t.FileName
		}
		fmt.Printf("  %-10s %-13s %5d%% %10s %12s  %s\n",
			agent.ShortID(t.ID), stateLabel(t.State), t.Progress, speed, size, truncateTitle(title, 44))
		if t.ErrorMessage != "" {
			dim.Printf("  %-10s %s\n", "", truncateTitle(t.ErrorMessage, 90))
		}
	}
	fmt.Println()
	dim.Println("  unarr downloads pause|resume|stop|retry <id>   (ids may be abbreviated)")
	fmt.Println()
}

// stateLabel colours the states a user acts on: paused and stopped are the ones
// that need a decision, so they must not read like ordinary progress.
func stateLabel(state string) string {
	switch state {
	case "paused":
		return color.New(color.FgYellow).Sprint(state)
	case "stopped", "queued":
		return color.New(color.FgHiBlack).Sprint(state)
	case "failed":
		return color.New(color.FgRed).Sprint(state)
	case "downloading", "seeding":
		return color.New(color.FgGreen).Sprint(state)
	default:
		return state
	}
}

func truncateTitle(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

func printActionResults(action string, results []control.ActionResult) {
	fmt.Println()
	if len(results) == 0 {
		color.New(color.FgHiBlack).Printf("  Nothing to %s.\n\n", action)
		return
	}
	for _, r := range results {
		mark := color.New(color.FgGreen).Sprint("✓")
		if !r.Applied {
			mark = color.New(color.FgHiBlack).Sprint("·")
		}
		msg := r.Message
		if msg == "" {
			msg = action + "ed"
		}
		fmt.Printf("  %s  %s — %s\n", mark, shortLabel(r.TaskID, r.Title), msg)
	}
	fmt.Println()
}

// downloadsCommandTimeout bounds a control call made from the CLI. Chosen so a
// wedged daemon fails with a readable error instead of hanging a terminal.
const downloadsCommandTimeout = 15 * time.Second

// withDownloadsTimeout is used by callers that have no context of their own
// (cobra gives one, but `unarr download stop` aliases call in directly).
func withDownloadsTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, downloadsCommandTimeout)
}
