package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

func rcloneEnvironment(s *remoteLibrary, obscured string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, v := range os.Environ() {
		// Inherited rclone settings can enable writable mounts, change the
		// endpoint or run credential helpers. This child has a complete config.
		if !strings.HasPrefix(strings.ToUpper(v), "RCLONE_") {
			env = append(env, v)
		}
	}
	return append(env,
		"RCLONE_CONFIG_UNARR_TYPE=webdav", "RCLONE_CONFIG_UNARR_URL="+s.URL+"/dav/",
		"RCLONE_CONFIG_UNARR_VENDOR=other", "RCLONE_CONFIG_UNARR_USER="+s.user,
		"RCLONE_CONFIG_UNARR_PASS="+obscured,
	)
}

func runRclone(ctx context.Context, s *remoteLibrary, directory string) error {
	binary := s.rclone
	if binary == "" {
		binary = "rclone"
	}
	obscure := exec.CommandContext(ctx, binary, "obscure", "-")
	winproc.HideWindow(obscure)
	obscure.Stdin = strings.NewReader(s.password + "\n")
	obscure.Env = rcloneEnvironment(s, "")
	password, err := obscure.Output()
	if err != nil {
		return fmt.Errorf("rclone password setup failed: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, "mount", "unarr:", directory,
		"--config", os.DevNull, "--read-only", "--vfs-cache-mode", "off",
		"--buffer-size", "4M", "--vfs-read-chunk-size", "32M",
		"--vfs-read-chunk-size-limit", "128M", "--dir-cache-time", "15s",
		"--poll-interval", "0", "--webdav-pacer-min-sleep", "0",
		"--low-level-retries", "2", "--retries", "2",
	)
	cmd.Env = rcloneEnvironment(s, strings.TrimSpace(string(password)))
	winproc.HideWindow(cmd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Give rclone time to unmount before the server goes away. CommandContext
	// escalates to Kill after WaitDelay if it cannot exit gracefully.
	cmd.Cancel = func() error { return interruptMountProcess(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second
	log.Printf("[mount] mounting remote library at %s; new files appear as background indexing completes", directory)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rclone mount failed: %w", err)
	}
	return nil
}
