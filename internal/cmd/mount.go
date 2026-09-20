package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/remotefs"
)

func newMountCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "mount [directory]", GroupID: "daemon",
		Short: "Configure and activate the persistent remote folder",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runMountCommand,
	}
	c.AddCommand(&cobra.Command{
		Use: "serve", Short: "Serve the remote library over loopback WebDAV", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := loadConfig()
			if !cfg.Mount.Enabled {
				return errors.New("remote mount is disabled; set mount.enabled = true in config.toml")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), mountSignals()...)
			defer stop()
			s, err := startRemoteLibrary(ctx, cfg)
			if err != nil {
				return err
			}
			defer s.Close()
			log.Printf("[mount] remote library at %s/dav/", s.URL)
			select {
			case <-ctx.Done():
				return nil
			case err := <-s.errors:
				return err
			}
		},
	})
	return c
}

type remoteLibrary struct {
	URL, user, password string
	rclone              string
	server              *http.Server
	catalog             *remotefs.Catalog
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	errors              chan error
}

func remoteSources(cfg config.Config, nzbDir string) []remotefs.Source {
	c := mountAPIClient(cfg)
	sources := []remotefs.Source{&remotefs.WebSource{API: c, AccountIdentity: cfg.Auth.APIURL + ":" + cfg.Auth.APIKey}}
	n := &mountNNTP{api: c}
	sources = append(sources, &remotefs.NZBSource{Directory: nzbDir, Fetcher: n, CloseFetcher: n.Close})
	return sources
}

func mountAPIClient(cfg config.Config) *agent.Client {
	return agent.NewClientWithMirrors(cfg.Auth.APIURL, cfg.Auth.Mirrors, cfg.Auth.APIKey, "unarr-mount")
}

func startRemoteLibrary(parent context.Context, cfg config.Config) (*remoteLibrary, error) {
	if !cfg.Mount.Enabled {
		return nil, errors.New("remote mount disabled")
	}
	if err := cfg.Mount.Validate(); err != nil {
		return nil, err
	}
	user, pass, active := engine.ResolveWebDAVCreds("", "", cfg.Auth.APIKey)
	if !active {
		return nil, errors.New("sign in to Unarr before mounting; provider accounts are managed on the website")
	}
	api := mountAPIClient(cfg)
	if err := api.MountAccess(parent); err != nil {
		return nil, fmt.Errorf("mount access: %w", err)
	}
	nzbDir := cfg.Mount.NZBDirectory(resolvedConfigPath())
	if err := os.MkdirAll(nzbDir, 0o700); err != nil {
		return nil, fmt.Errorf("mount NZB inbox: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Mount.Address())
	if err != nil {
		return nil, fmt.Errorf("mount listener: %w", err)
	}
	sources := remoteSources(cfg, nzbDir)
	dir := cfg.Mount.CacheDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(resolvedConfigPath()), "remote-library")
	}
	cat, err := remotefs.OpenCatalog(dir, sources)
	if err != nil {
		_ = ln.Close()
		for _, s := range sources {
			_ = s.Close()
		}
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	srv := &http.Server{
		Handler: remotefs.Handler(cat.FS, user, pass), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	s := &remoteLibrary{URL: "http://" + ln.Addr().String(), user: user, password: pass, server: srv, catalog: cat, ctx: ctx, cancel: cancel, done: make(chan struct{}), errors: make(chan error, 1)}
	go watchMountAccess(ctx, api.MountAccess, 30*time.Second, s.revokeAccess)
	go func() {
		defer close(s.done)
		cat.Run(ctx, cfg.Mount.RefreshEvery(), logMountRefresh)
	}()
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errors <- err
			cancel()
		}
	}()
	return s, nil
}

func logMountRefresh(name string, err error) {
	if err != nil {
		log.Printf("[mount] %s refresh: %v (retaining last valid catalog)", name, err)
	}
}

func (s *remoteLibrary) Close() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		_ = s.server.Close()
	}
	<-s.done
	_ = s.catalog.Close()
}

func runRemoteMount(ctx context.Context, cfg config.Config, directory, binary string) error {
	if err := cfg.Mount.Validate(); err != nil {
		return err
	}
	dir, err := validateMountPoint(directory)
	if err != nil {
		return err
	}
	s, err := startRemoteLibrary(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	s.rclone = binary
	err = runRclone(s.ctx, s, dir)
	select {
	case serveErr := <-s.errors:
		return serveErr
	default:
		return err
	}
}

func mountContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, mountSignals()...)
}
