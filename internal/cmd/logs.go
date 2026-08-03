package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/spf13/cobra"
)

// logFileName is the daemon log every path agrees on — the systemd/launchd
// templates, the Windows shim's redirect, the detached launcher and this
// reader.
const logFileName = "unarr.log"

// daemonLogPath returns the live daemon log file.
func daemonLogPath() string { return filepath.Join(config.DataDir(), logFileName) }

// logRingOptions resolves the [daemon] log_* keys into the shape the logging
// package takes. Single source of truth: the detached start, the pre-launch
// rotation, the daemon's janitor and `unarr logs` must all agree on which
// files make up the ring, or `clean` and the reader would miss half of it.
//
// Owner is set here, for every caller at once. Only RotateNow consults it — the
// daemon's own Writer never asks, because a process cannot be an outsider to
// its own file — so wiring it in one place makes "rotate from the outside while
// a live daemon owns the file" impossible to reach by forgetting a field.
//
// MaxSizeMB is 0 on a stock install: rotation is opt-in, so every consumer of
// these Options (Writer, RotateNow, Sweep) is a no-op until the user sets
// log_max_size_mb. That is the single switch — there is no second one.
func logRingOptions(path string) logging.Options {
	cfg := loadConfig()
	return logging.Options{
		Path:      path,
		MaxSizeMB: cfg.Daemon.LogMaxSizeMB,
		MaxFiles:  cfg.Daemon.LogMaxFiles,
		Owner:     daemonLogOwner,
	}
}

// rotateDaemonLog trims this machine's daemon log if it is already over budget.
// For callers that hold no install directory of their own (`unarr logs rotate`,
// `unarr daemon start`).
func rotateDaemonLog() { rotateDaemonLogIn(config.DataDir()) }

// rotateDaemonLogIn trims the daemon log under dir. Called in the gap BEFORE a
// service manager opens the file (launchd, the Windows task). Best-effort —
// never block a daemon start over a log file.
//
// "The gap" is an assumption, and one caller does not hold it: the post-upgrade
// re-registration runs while the OLD daemon is still alive and owns the file.
// That is why the returned error is discarded but the ownership check inside
// RotateNow is not optional — the trim becomes a no-op there, and the daemon
// that comes up next does it by rename instead.
//
// The installers take dir rather than resolving config.DataDir(): they already
// hold the directory whose file the service manager is about to open, and the
// global data dir can be a DIFFERENT one — so resolving it here would truncate
// a file nobody asked about. That is not hypothetical: an installer test that
// redirects HOME but not the data dir would copy-truncate the developer's own
// live daemon log, shifting the ring and discarding its oldest slot.
func rotateDaemonLogIn(dir string) {
	_ = logging.RotateNow(logRingOptions(filepath.Join(dir, logFileName)))
}

// logsOptions carries the flags of `unarr logs`. A struct rather than five
// positional parameters, per the repo's argument-limit rule.
type logsOptions struct {
	follow bool
	lines  int
	since  string
	level  string
	grep   string
	boot   bool
}

func newLogsCmd() *cobra.Command {
	var o logsOptions

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the daemon log",
		Long: `Read the unarr daemon log, with filtering.

Reads ` + logFileName + ` from the data directory, transparently continuing into
the rotated copies (` + logFileName + `.1, .2, …) when the live file holds fewer
lines than asked for. On Linux, where the systemd unit sends output to the
journal instead of a file, this falls back to journalctl.

--boot reads ` + bootLogFileName + ` instead: the small file the service
launcher itself holds, carrying what never reaches the daemon's own log — the
start banner, a fatal error from a start that never got going, a crash dump.
That is where to look when the daemon does not come up at all.

Severity is inferred from the line itself — the daemon logs free-form text —
so --level is a reading aid, not a guarantee.

The global --json renders one JSON object per line, overriding [daemon]
log_format.`,
		Example: `  unarr logs
  unarr logs -f
  unarr logs -n 200 --level warn
  unarr logs --boot
  unarr logs --since 2h --grep 'usenet|nzb'
  unarr logs --since "2025-01-20 09:00" --level error
  unarr logs --json | jq -r .message`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(o)
		},
	}

	bindLogsFlags(cmd, &o)
	cmd.AddCommand(newLogsRotateCmd())
	return cmd
}

// bindLogsFlags wires the shared flag set, so `unarr logs` and the
// `unarr daemon logs` alias cannot drift apart.
func bindLogsFlags(cmd *cobra.Command, o *logsOptions) {
	cmd.Flags().BoolVarP(&o.follow, "follow", "f", false, "stream new lines as they are written")
	cmd.Flags().IntVarP(&o.lines, "lines", "n", logging.DefaultLines, "number of lines to show")
	cmd.Flags().StringVar(&o.since, "since", "", `only lines newer than this ("30m", "2h", "7d", "2006-01-02 15:04")`)
	cmd.Flags().StringVar(&o.level, "level", "", "minimum severity: debug, info, warn, error (default: [daemon] log_level)")
	cmd.Flags().StringVar(&o.grep, "grep", "", "keep only lines matching this case-insensitive regular expression")
	cmd.Flags().BoolVar(&o.boot, "boot", false, "read "+bootLogFileName+" (startup output and crashes) instead of the daemon log")
}

func newLogsRotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the daemon log now if it is over its size budget",
		Long: `Rotate ` + logFileName + ` immediately when it has reached log_max_size_mb.

ROTATION IS OPT-IN AND OFF BY DEFAULT (log_max_size_mb = 0), so on a stock
install this command does nothing at all. It also does nothing when the log is
still under budget.

Turning rotation on has known limitations, and they are not theoretical. Any
second process holding the file can block a rename or a truncate: an open
'unarr logs -f', an antivirus scanner, OneDrive or Dropbox syncing the data
directory, Windows Search. Windows is the strict case — a holder there can deny
write access outright. Read docs/plans/daemon-log-ownership.md ("Deuda abierta")
before enabling it; without rotation, bound the log with 'unarr clean' or an
external logrotate using copytruncate.

A daemon started by a service launcher OWNS its log and rotates it itself, from
the inside — this command cannot rotate it from the outside while that daemon
runs, and says so rather than shifting the ring for nothing.`,
		Example: `  unarr logs rotate`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogsRotate()
		},
	}
}

// runLogsRotate rotates the daemon log from the outside, and explains itself
// when it must not.
//
// A live daemon owning the file is not a failure — it is the design working:
// that daemon rotates the file itself, and a copy-truncate from here would lose
// the lines written between the copy and the truncate. So it prints what is
// happening and exits 0, rather than failing a command the user was right to
// run.
func runLogsRotate() error {
	err := logging.RotateNow(logRingOptions(daemonLogPath()))
	if errors.Is(err, logging.ErrOwnedByLiveProcess) {
		fmt.Println()
		fmt.Printf("  Nothing to do: %v\n", err)
		fmt.Println("  The daemon rotates this log itself once it reaches log_max_size_mb.")
		fmt.Println("  (Rotation is opt-in: log_max_size_mb = 0 means it never does.)")
		fmt.Println()
		return nil
	}
	return err
}

// runLogs prints (or follows) the daemon log, from whichever source this
// platform actually has: the systemd journal, or unarr.log and its rotated
// copies. Both go through the same filters.
func runLogs(o logsOptions) error {
	if o.boot {
		if err := bootSourceUnavailable(); err != nil {
			return err
		}
	}
	q, err := buildLogQuery(o)
	if err != nil {
		return err
	}
	// --boot never reaches here on a systemd box: bootSourceUnavailable above
	// has already refused it, precisely so the journal cannot answer a question
	// asked about a file.
	if usesJournald() {
		return runJournalLogs(q, o.follow)
	}
	if !logRingExists(q) {
		if o.boot {
			return missingBootLogError(q.Path)
		}
		return missingDaemonLogError(q.Path)
	}
	if !o.follow {
		return logging.Print(q, os.Stdout)
	}

	// Ctrl-C ends a follow; that is the user finishing, not an error, so the
	// context cancellation returns nil out of Follow.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return logging.Follow(ctx, q, os.Stdout)
}

// buildLogQuery turns flags + config into a logging.Query. Every bad value is
// reported here, before any file is touched, so a typo in --since or --level
// fails loudly instead of quietly showing the wrong slice of the log.
func buildLogQuery(o logsOptions) (logging.Query, error) {
	cfg := loadConfig()

	if o.lines < 0 {
		return logging.Query{}, fmt.Errorf("--lines must be zero or more, got %d", o.lines)
	}
	level, err := resolveLogLevel(cfg, o.level)
	if err != nil {
		return logging.Query{}, err
	}
	format, err := resolveLogFormat(cfg)
	if err != nil {
		return logging.Query{}, err
	}
	since, err := logging.ParseSince(o.since, time.Now())
	if err != nil {
		return logging.Query{}, err
	}

	q := logging.Query{
		Path:     daemonLogPath(),
		Lines:    o.lines,
		MinLevel: level,
		Grep:     o.grep,
		Since:    since,
		Format:   format,
		MaxFiles: cfg.Daemon.LogMaxFiles,
	}
	if o.boot {
		q = bootLogQuery(q)
	}
	// Compile the pattern here too. The filters do it lazily, and on a systemd
	// box the journalctl child is already running by then — a caller that gave
	// up before draining its pipe would hang instead of printing the error.
	if err := q.Validate(); err != nil {
		return logging.Query{}, err
	}
	return q, nil
}

// resolveLogFormat picks how the kept lines are rendered. Precedence mirrors
// severity's: the global --json beats [daemon] log_format, which beats the
// built-in default. The config value is parsed either way, so a typo in it is
// still reported when --json makes it moot.
func resolveLogFormat(cfg config.Config) (logging.Format, error) {
	format, err := logging.ParseFormat(cfg.Daemon.LogFormat)
	if err != nil {
		return logging.DefaultFormat, fmt.Errorf("config [daemon] log_format: %w", err)
	}
	if jsonOut {
		return logging.FormatJSON, nil
	}
	return format, nil
}

// resolveLogLevel picks the effective minimum severity. Precedence: the
// command's own --level, then the global --log-level, then [daemon] log_level,
// then the built-in default.
func resolveLogLevel(cfg config.Config, cmdLevel string) (logging.Level, error) {
	for _, candidate := range []string{cmdLevel, logLevelFlag, cfg.Daemon.LogLevel} {
		if candidate == "" {
			continue
		}
		return logging.ParseLevel(candidate)
	}
	return logging.DefaultLevel, nil
}

// usesJournald reports whether daemon output goes to the systemd journal
// rather than to a file. Checked BEFORE the file reader so a systemd box keeps
// the behaviour `unarr daemon logs` has always had — a stale unarr.log left
// behind by an earlier `unarr up` must not shadow the live journal.
func usesJournald() bool {
	return runtime.GOOS == "linux" && service.Respawns()
}

// logRingExists reports whether any file of the ring is on disk. Checking the
// rotated slots too matters right after a rotation, when the live file can be
// momentarily empty but the history is very much there.
func logRingExists(q logging.Query) bool {
	for _, p := range append([]string{q.Path}, logging.RotatedPaths(q.Path, q.MaxFiles)...) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
