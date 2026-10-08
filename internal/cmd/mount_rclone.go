package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/mountsetup"
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
	args := append([]string{"mount", "unarr:", directory}, mountsetup.MountFlags()...)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = rcloneEnvironment(s, strings.TrimSpace(string(password)))
	winproc.HideWindow(cmd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	finish, err := configureRcloneMount(ctx, cmd, directory)
	if err != nil {
		return fmt.Errorf("rclone mount preparation failed: %w", err)
	}
	log.Printf("[mount] mounting remote library at %s; new files appear as background indexing completes", directory)
	err = cmd.Run()
	if cleanupErr := finish(); cleanupErr != nil {
		return fmt.Errorf("rclone mount cleanup failed: %w", cleanupErr)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rclone mount failed: %w", err)
	}
	return nil
}
