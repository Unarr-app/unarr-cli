package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/Unarr-app/unarr-cli/internal/logging"
	"github.com/Unarr-app/unarr-cli/internal/support"
)

// newSupportBundleCmd wires `unarr support-bundle`. Everything this file does
// is assemble inputs and render the result — the collection and the redaction
// live in internal/support, where they can be reasoned about (and tested)
// without a Cobra command in the way.
func newSupportBundleCmd() *cobra.Command {
	var out string
	var logLines int
	var printOnly bool

	cmd := &cobra.Command{
		Use:   "support-bundle",
		Short: "Collect a redacted diagnostic bundle for a bug report",
		Long: `Write one attachable file with everything a maintainer normally asks for.

The bundle holds: the doctor report, a REDACTED copy of your settings, the last
lines of the daemon log (from the journal on systemd hosts), the daemon state,
the active-task list, the cached encode benchmark, and a summary of the host's
disks, ports and ffmpeg.

What it does NOT hold: your API key, the agent hash, WebDAV credentials, the
funnel hostname, download URLs, or the contents of your directories. The
configuration is rebuilt from an allowlist rather than copied, so free-form
values — paths, custom endpoints, agent names — are reported as <set>/<custom>
instead of being reproduced. Log lines are scrubbed for tokens on the way in.

The bundle is written to a local file. It is never uploaded, and nothing here
sends it anywhere: attaching it is your decision, and --print shows exactly
what would go in it before a file exists.`,
		Example: `  unarr support-bundle
  unarr support-bundle --out /tmp/unarr-issue-1234.tar.gz
  unarr support-bundle --log-lines 2000
  unarr support-bundle --print`,
		Args:    cobra.NoArgs,
		GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSupportBundle(supportBundleOpts{out: out, logLines: logLines, printOnly: printOnly})
		},
	}

	cmd.Flags().StringVarP(&out, "out", "o", "", "write the bundle here (default ./unarr-support-<timestamp>.tar.gz)")
	cmd.Flags().IntVar(&logLines, "log-lines", support.DefaultLogLines, "lines of daemon log to include")
	cmd.Flags().BoolVar(&printOnly, "print", false, "list what would be collected, without writing a file")
	return cmd
}

// supportBundleOpts carries the flags, per the repo's argument-limit rule.
type supportBundleOpts struct {
	out       string
	logLines  int
	printOnly bool
}

func runSupportBundle(o supportBundleOpts) error {
	if o.logLines < 0 {
		return fmt.Errorf("--log-lines must be zero or more, got %d", o.logLines)
	}
	bundle := support.Collect(supportInputs(o.logLines))

	if o.printOnly {
		return printBundlePreview(bundle)
	}
	path := o.out
	if path == "" {
		path = support.DefaultName(time.Now())
	}
	if err := bundle.WriteTarGz(path); err != nil {
		return err
	}
	return announceBundle(bundle, path)
}

// supportInputs assembles what internal/support cannot resolve for itself.
//
// The doctor closure runs the SAME specs `unarr doctor` runs, through the same
// runner: the bundle must not be able to disagree with the report the user is
// looking at. It is also the only thing in the whole command that touches the
// network, and it does so because a connectivity check is the point.
func supportInputs(logLines int) support.Inputs {
	cfg := loadConfig()
	ffmpegPath, _ := mediainfo.LocateFFmpeg(cfg.Library.FFmpegPath)
	return support.Inputs{
		Config:   cfg,
		Version:  Version,
		LogLines: logLines,
		Doctor: func() (doctor.Report, error) {
			c := cfg
			return doctor.Run(doctorSpecs(&c), nil), nil
		},
		Journal:        journalLogSource(),
		Logs:           bundleLogPaths(cfg.Daemon.LogMaxFiles),
		FFmpegPath:     ffmpegPath,
		BenchCachePath: engine.EncodeBenchCachePath(),
	}
}

// bundleLogPaths hands the collector the file names this package owns, so the
// bundle and `unarr logs` can never end up reading different files.
func bundleLogPaths(maxFiles int) support.LogPaths {
	dir := config.DataDir()
	return support.LogPaths{
		Daemon:   filepath.Join(dir, logFileName),
		Err:      filepath.Join(dir, errLogFileName),
		Boot:     filepath.Join(dir, bootLogFileName),
		MaxFiles: maxFiles,
	}
}

// journalLogSource returns the journal reader, or nil on every host where the
// daemon writes a log file. Same decision `unarr logs` makes, through the same
// predicate — on a systemd box there is no unarr.log, and a stale one left by
// an earlier foreground run would answer with the wrong process's output.
func journalLogSource() func(io.Writer, int) error {
	if !usesJournald() {
		return nil
	}
	return func(w io.Writer, n int) error {
		return journalTo(w, logging.Query{Path: daemonLogPath(), Lines: n, Format: logging.FormatText}, false)
	}
}

// printBundlePreview shows what would be collected without writing anything.
// The global --json renders the manifest instead, so a script can check the
// section list without unpacking an archive.
func printBundlePreview(b *support.Bundle) error {
	if jsonOut {
		manifest, err := b.Manifest()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(manifest)
		return err
	}
	fmt.Println()
	fmt.Println("  Would collect (nothing has been written):")
	fmt.Println()
	fmt.Print(b.Listing())
	fmt.Println()
	return nil
}

// announceBundle prints where the file is and what is in it.
//
// The reminder is not decoration. The bundle is meant to be attached to a
// public issue, and a user who does not know what it holds either attaches it
// blindly or does not attach it at all. Both outcomes are bad, and both are
// avoided by one sentence and a --print they can run first.
func announceBundle(b *support.Bundle, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	fmt.Println()
	fmt.Print(b.Listing())
	fmt.Println()
	fmt.Printf("  Bundle written to %s\n", abs)
	fmt.Println("  It holds your doctor report, redacted settings, recent daemon log lines and host info —")
	fmt.Println("  no API key, agent hash, passwords or funnel URL. Nothing was uploaded; attach it yourself.")
	fmt.Println()
	return nil
}
