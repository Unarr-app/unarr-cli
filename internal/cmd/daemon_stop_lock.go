package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/fatih/color"
	"github.com/gofrs/flock"
)

// Startup can precede the stop watcher. Normal shutdown can then spend 5s on
// telemetry, 30s draining downloads, 5s deregistering and 15s joining the mount.
// Stream shutdown/init can take longer: report incomplete cleanup, never force
// the parent dead and abandon its owned child to make the deadline.
// This context bounds lock acquisition and contextual callbacks, not the
// existing supervisor /end or best-effort firewall commands outside that wait.
const daemonStopTimeout = 90 * time.Second

func stopDaemonByLock(cleanup func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStopTimeout)
	defer cancel()
	return withStoppedDaemon(ctx, config.LockPath(), func(ctx context.Context) error {
		// State can be missing, stale or changed during startup/shutdown. Only
		// reap a dead recorded process; never act on a guessed live PID.
		if st := agent.ReadState(); st != nil && !agent.IsProcessAlive(st.PID) {
			agent.RemoveState()
		}
		if cleanup != nil {
			return cleanup(ctx)
		}
		color.New(color.FgGreen).Println("  ✓ Daemon stopped")
		return nil
	})
}

// The daemon releases this kernel lock after its deferred mount/log cleanup.
// Keep it through cleanup so a replacement cannot start during uninstall.
// This acknowledges cooperative shutdown, not arbitrary cleanup after a crash.
func withStoppedDaemon(ctx context.Context, lockPath string, cleanup func(context.Context) error) (result error) {
	instanceLock := flock.New(lockPath)
	locked, err := instanceLock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return fmt.Errorf("daemon cleanup incomplete: %w", err)
	}
	if !locked {
		return errors.New("daemon cleanup incomplete: instance lock is busy")
	}
	defer func() {
		if err := instanceLock.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("release daemon stop lock: %w", err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon cleanup incomplete: %w", err)
	}
	return cleanup(ctx)
}
