package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/mountsetup"
)

const (
	remoteMountRetryDelay         = 30 * time.Second
	remoteMountStopTimeout        = 15 * time.Second
	remoteMountCredentialInterval = 5 * time.Second
)

// Login and removal can replace the saved identity while the agent is healthy,
// not just while it is blocked. This callback updates credentialStore only;
// the daemon owner applies its key+ID after the old register/sync cycle exits.
func watchRemoteMountCredentials(ctx context.Context, reload func()) {
	ticker := time.NewTicker(remoteMountCredentialInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reload()
		}
	}
}

type remoteMountSupervisor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type remoteMountRunner func(context.Context, config.Config) error

func startRemoteMountSupervisor(parent context.Context, cfg config.Config, creds *credentialStore, run remoteMountRunner, reload func()) *remoteMountSupervisor {
	ctx, cancel := context.WithCancel(parent)
	s := &remoteMountSupervisor{cancel: cancel, done: make(chan struct{})}
	var watchers sync.WaitGroup
	if reload != nil {
		watchers.Add(1)
		go func() { defer watchers.Done(); watchRemoteMountCredentials(ctx, reload) }()
	}
	go func() {
		defer close(s.done)
		defer watchers.Wait()
		defer cancel()
		superviseRemoteMount(ctx, cfg, creds, run)
	}()
	return s
}

func (s *remoteMountSupervisor) stop(timeout time.Duration) error {
	s.cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("remote mount did not finish shutting down within %s", timeout)
	}
}

// superviseRemoteMount keeps the optional filesystem attached for the lifetime
// of the normal unarr agent service. The interactive `unarr mount` command has
// already installed/approved dependencies; this path only reuses them and can
// never approve privileged system changes on its own.
func runPersistentRemoteMount(ctx context.Context, cfg config.Config) error {
	directory := cfg.Mount.Directory
	if directory == "" {
		var err error
		directory, err = mountDestination(nil)
		if err != nil {
			return err
		}
	}

	binary, err := mountsetup.Ensure(ctx, mountsetup.Options{
		Directory: filepath.Join(filepath.Dir(resolvedConfigPath()), "tools"), Output: os.Stderr,
	})
	if err != nil {
		return err
	}
	log.Printf("[mount] requesting persistent remote folder at %s", directory)
	return runRemoteMount(ctx, cfg, directory, binary)
}

func superviseRemoteMount(ctx context.Context, cfg config.Config, creds *credentialStore, run remoteMountRunner) {
	for ctx.Err() == nil {
		identity := creds.identity()
		if identity.key == "" || identity.agentID == "" {
			select {
			case <-ctx.Done():
				return
			case <-identity.changed:
				continue
			}
		}
		session := cfg
		session.Auth.APIKey, session.Agent.ID = identity.key, identity.agentID
		sessionCtx, cancel := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { result <- run(sessionCtx, session) }()
		err := awaitRemoteMountSession(ctx, identity, cancel, result)
		cancel()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-identity.changed:
			continue
		default:
		}
		log.Printf("[mount] stopped: %v; retrying in %s", err, remoteMountRetryDelay)
		timer := time.NewTimer(remoteMountRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-identity.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Cancel and join DAV/catalog/readers/rclone cleanup before the next identity
// can bind the listener or reuse device-local resources.
func awaitRemoteMountSession(ctx context.Context, identity credentialIdentity, cancel context.CancelFunc, result <-chan error) error {
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
	case <-identity.changed:
	}
	cancel()
	// The public stop has a bounded wait and reports failure if this cleanup
	// stalls. Never close supervisor.done or start another identity prematurely.
	return <-result
}
