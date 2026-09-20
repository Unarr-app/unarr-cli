package cmd

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/mountsetup"
)

const remoteMountRetryDelay = 30 * time.Second

// superviseRemoteMount keeps the optional filesystem attached for the lifetime
// of the normal unarr agent service. The interactive `unarr mount` command has
// already installed/approved dependencies; this path only reuses them and can
// never approve privileged system changes on its own.
func superviseRemoteMount(ctx context.Context, cfg config.Config) {
	directory := cfg.Mount.Directory
	if directory == "" {
		var err error
		directory, err = mountDestination(nil)
		if err != nil {
			log.Printf("[mount] destination: %v", err)
			return
		}
	}

	for ctx.Err() == nil {
		binary, err := mountsetup.Ensure(ctx, mountsetup.Options{
			Directory: filepath.Join(filepath.Dir(resolvedConfigPath()), "tools"),
			Output:    os.Stderr,
		})
		if err == nil {
			log.Printf("[mount] persistent remote folder enabled at %s", directory)
			err = runRemoteMount(ctx, cfg, directory, binary)
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("[mount] stopped: %v; retrying in %s", err, remoteMountRetryDelay)
		timer := time.NewTimer(remoteMountRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
