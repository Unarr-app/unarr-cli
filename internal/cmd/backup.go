package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Unarr-app/unarr-cli/internal/backup"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

const backupPassphraseEnv = "UNARR_BACKUP_PASSPHRASE" //nolint:gosec // env var NAME, not a credential

func newBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up and restore local state (config, download queue)",
		Long: `Snapshot config.toml and the download resume queue into a verified zip.

By default the archive contains NO secrets: the API key, agent hash and WebDAV
credentials are blanked, and resolved download URLs / NZB passwords are
stripped from the queue. Use --encrypt (AES-256-GCM, passphrase) to keep them.
Restoring a secret-free archive never wipes the credentials already on this
machine.

Scheduled backups ([backup] in config.toml) always run secret-free.`,
		GroupID: "system",
	}
	cmd.AddCommand(newBackupCreateCmd(), newBackupListCmd(), newBackupRestoreCmd(), newBackupPruneCmd())
	return cmd
}

func backupLayout() backup.Layout {
	return backup.Layout{ConfigPath: resolvedConfigPath(), DataDir: config.DataDir()}
}

func backupDir(override string) string {
	if override != "" {
		return override
	}
	return loadConfig().Backup.Directory(resolvedConfigPath())
}

type passphraseFlags struct {
	file string
}

func (p *passphraseFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&p.file, "passphrase-file", "", "read the passphrase from this file (or set "+backupPassphraseEnv+")")
}

// resolve returns the passphrase from file, env, or an interactive prompt on
// prompts (stderr, so it never mixes into redirected output). An empty
// passphrase is always an error: it must never quietly mean "no encryption".
func (p *passphraseFlags) resolve(prompts io.Writer, confirm bool) (string, error) {
	if p.file != "" {
		b, err := os.ReadFile(p.file)
		if err != nil {
			return "", fmt.Errorf("read passphrase file: %w", err)
		}
		pass := strings.TrimRight(string(b), "\r\n")
		if pass == "" {
			return "", fmt.Errorf("passphrase file %s is empty", p.file)
		}
		return pass, nil
	}
	if v := os.Getenv(backupPassphraseEnv); v != "" {
		return v, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("passphrase needed: use --passphrase-file or %s (stdin is not a terminal)", backupPassphraseEnv)
	}
	read := func(label string) (string, error) {
		fmt.Fprint(prompts, label)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(prompts)
		return string(b), err
	}
	pass, err := read("Passphrase: ")
	if err != nil || pass == "" {
		return "", errors.New("empty passphrase")
	}
	if confirm {
		if again, err := read("Repeat passphrase: "); err != nil || again != pass {
			return "", errors.New("passphrases do not match")
		}
	}
	return pass, nil
}

func newBackupCreateCmd() *cobra.Command {
	var out string
	var encrypt bool
	var pf passphraseFlags
	cmd := &cobra.Command{
		Use:     "create",
		Short:   "Create a backup archive",
		Example: "  unarr backup create\n  unarr backup create --out ~/unarr-backup.zip\n  unarr backup create --encrypt",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			pass := ""
			if encrypt {
				var err error
				if pass, err = pf.resolve(cmd.ErrOrStderr(), true); err != nil {
					return err
				}
			}
			res, err := backup.Create(backup.CreateOptions{
				Layout: backupLayout(), Version: Version, Out: out,
				Dir: backupDir(""), Passphrase: pass,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "Backup written: %s\n", res.Path)
			for _, f := range res.Manifest.Files {
				fmt.Fprintf(w, "  %-18s %6d bytes  sha256 %s…\n", f.Name, f.Size, f.SHA256[:12])
			}
			if pass == "" {
				fmt.Fprintf(w, "Secrets were NOT included (%s, download URLs). Use --encrypt to include them.\n",
					strings.Join(backup.RedactedConfigKeys(), ", "))
			} else {
				fmt.Fprintln(w, "Archive is encrypted (AES-256-GCM). Keep the passphrase: it cannot be recovered.")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "output file or directory (default: the backup dir)")
	cmd.Flags().BoolVar(&encrypt, "encrypt", false, "encrypt with a passphrase and include secrets")
	pf.bind(cmd)
	return cmd
}

func newBackupListCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List backups in the backup directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			d := backupDir(dir)
			entries, err := backup.List(d)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Fprintf(w, "No backups in %s\n", d)
				return nil
			}
			fmt.Fprintf(w, "Backups in %s\n", d)
			for _, e := range entries {
				fmt.Fprintf(w, "  %s  %7d bytes  %s%s\n", e.CreatedAt.Local().Format("2006-01-02 15:04:05"), e.Size, e.Name, entryTags(e))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "backup directory (default: [backup] dir)")
	return cmd
}

func entryTags(e backup.Entry) string {
	tags := ""
	if e.Encrypted {
		tags += " [encrypted]"
	}
	switch e.Kind {
	case backup.KindScheduled:
		tags += " [scheduled]"
	case backup.KindPreRestore:
		tags += " [pre-restore]"
	default:
	}
	return tags
}

func newBackupPruneCmd() *cobra.Command {
	var dir string
	var keep int
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old backups, keeping the newest [backup] retention_count",
		Long: `Delete all but the newest manual and scheduled backups in the backup directory.
Pre-restore snapshots are never pruned. (The daemon's own retention only ever
deletes scheduled backups.)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if keep <= 0 {
				keep = loadConfig().Backup.Retention()
			}
			removed, err := backup.Prune(backupDir(dir), keep)
			for _, p := range removed {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", p)
			}
			if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "kept the newest %d; removed %d\n", keep, len(removed))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "backup directory (default: [backup] dir)")
	cmd.Flags().IntVar(&keep, "keep", 0, "how many to keep (default: [backup] retention_count)")
	return cmd
}
