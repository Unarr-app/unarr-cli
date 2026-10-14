package cmd

import (
	"context"
	"log"

	"github.com/Unarr-app/unarr-cli/internal/backup"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// startBackupScheduler runs the [backup] schedule for the life of the daemon.
// Nothing starts when it is disabled, or when the table is invalid — that is
// logged once instead of guessing what "3am" meant. Turning it on, like every
// other config change, needs a daemon restart.
func startBackupScheduler(ctx context.Context, cfg config.Config) {
	b := cfg.Backup
	if !b.Enabled {
		return
	}
	if err := b.Validate(); err != nil {
		log.Printf("[backup] scheduled backups disabled: %v (see `unarr config check`)", err)
		return
	}
	s := &backup.Scheduler{Config: b, Layout: backupLayout(), Version: Version}
	go s.Run(ctx)
}
