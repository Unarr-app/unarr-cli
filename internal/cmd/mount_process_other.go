//go:build !darwin

package cmd

import (
	"context"
	"os/exec"
	"time"
)

func configureRcloneMount(_ context.Context, cmd *exec.Cmd, _ string) (func() error, error) {
	// Give rclone time to unmount before the server goes away. CommandContext
	// escalates to Kill after WaitDelay if it cannot exit gracefully.
	cmd.Cancel = func() error { return interruptMountProcess(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second
	return func() error { return nil }, nil
}
