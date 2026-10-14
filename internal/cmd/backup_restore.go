package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/Unarr-app/unarr-cli/internal/backup"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

var errDaemonRunning = errors.New("the unarr daemon is running: stop it with 'unarr stop' before restoring")

type restoreFlags struct {
	file  string
	apply bool
	// dir overrides where the pre-restore snapshot goes.
	dir string
	pf  passphraseFlags
}

func newBackupRestoreCmd() *cobra.Command {
	var f restoreFlags
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Verify a backup and restore it (dry-run unless --apply)",
		Long: `Verify the archive (manifest + SHA-256 of every file) and show what would change.
Nothing is written without --apply. With --apply the daemon must be stopped,
and a pre-restore snapshot of the current state is saved first, beside
config.toml (it holds the current secrets, so it never goes to [backup] dir).`,
		Example: "  unarr backup restore ~/unarr-backup.zip\n  unarr backup restore ~/unarr-backup.zip --apply",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.file = args[0]
			return runBackupRestore(cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}
	cmd.Flags().BoolVar(&f.apply, "apply", false, "write the files (default: dry-run)")
	cmd.Flags().StringVar(&f.dir, "dir", "", "where the pre-restore snapshot goes (default: <config dir>/backups)")
	f.pf.bind(cmd)
	return cmd
}

func runBackupRestore(w, prompts io.Writer, f restoreFlags) error {
	arc, err := backup.Open(f.file, "")
	if errors.Is(err, backup.ErrPassphraseRequired) {
		pass, perr := f.pf.resolve(prompts, false)
		if perr != nil {
			return perr
		}
		arc, err = backup.Open(f.file, pass)
	}
	if err != nil {
		return err
	}
	m := arc.Manifest
	fmt.Fprintf(w, "Backup OK: unarr %s, %s/%s, %s, secrets %s\n", m.UnarrVersion, m.OS, m.Arch,
		m.CreatedAt.Local().Format("2006-01-02 15:04:05"), m.Secrets)
	if !f.apply {
		plan, err := arc.Plan(backupLayout())
		printRestorePlan(w, plan)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "Dry-run: nothing was written. Re-run with --apply (daemon stopped) to restore.")
		return nil
	}
	return withInstanceLock(config.LockPath(), func() error { return applyRestore(w, arc, f.dir) })
}

// applyRestore plans and applies under the instance lock, so the state the plan
// compared against is the state that gets snapshotted and replaced.
func applyRestore(w io.Writer, arc *backup.Archive, dir string) error {
	plan, err := arc.Plan(backupLayout())
	printRestorePlan(w, plan)
	if err != nil {
		return err
	}
	if !plan.Changes() {
		fmt.Fprintln(w, "Nothing to change.")
		return nil
	}
	if dir == "" {
		// The snapshot keeps the CURRENT secrets in the clear, so it stays beside
		// config.toml even when [backup] dir points at a NAS or a cloud folder.
		dir = config.DefaultBackupDir(resolvedConfigPath())
	}
	if err := plan.Apply(backup.ApplyOptions{SnapshotDir: dir, Version: Version}); err != nil {
		return err
	}
	if plan.PreRestore == "" {
		fmt.Fprintln(w, "Restored. There was no previous state to save.")
	} else {
		fmt.Fprintf(w, "Restored. Previous state saved to %s\n", plan.PreRestore)
	}
	return nil
}

func printRestorePlan(w io.Writer, plan backup.Plan) {
	for _, it := range plan.Items {
		fmt.Fprintf(w, "  %-9s %s (%d bytes) -> %s\n", it.Action, it.Name, it.Size, it.Dest)
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
}

// withInstanceLock runs fn while holding the daemon's single-instance lock. A
// running daemon refuses the restore up front, and a daemon started meanwhile
// fails to take the lock instead of loading half-restored state — a check that
// released the lock before writing (check-then-act) could not promise that.
func withInstanceLock(lockPath string, fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	l := flock.New(lockPath)
	locked, err := l.TryLock()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !locked {
		return errDaemonRunning
	}
	defer func() {
		if cerr := l.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("release instance lock: %w", cerr))
		}
	}()
	return fn()
}
