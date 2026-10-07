package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func waitMountIdentity(t *testing.T, started <-chan credentialIdentity, key, id string) {
	t.Helper()
	select {
	case got := <-started:
		if got.key != key || got.agentID != id {
			t.Fatal("wrong mount identity", got.agentID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("identity change did not restart mount")
	}
}

func TestMountSupervisorIdentityLifecycleAndJoinedShutdown(t *testing.T) {
	isolatedMountConfig(t)
	creds, file := storeAt(t, "boot-key", "boot-id")
	started := make(chan credentialIdentity, 8)
	var active, closed atomic.Int32
	runner := func(ctx context.Context, cfg config.Config) error {
		if active.Add(1) != 1 {
			t.Error("new identity started before old readers closed")
		}
		started <- credentialIdentity{key: cfg.Auth.APIKey, agentID: cfg.Agent.ID}
		<-ctx.Done()
		time.Sleep(80 * time.Millisecond) // delayed reader/child shutdown
		active.Add(-1)
		closed.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startRemoteMountSupervisor(ctx, config.Default(), creds, runner, nil)
	t.Cleanup(func() {
		if err := s.stop(time.Second); err != nil {
			t.Error(err)
		}
	})
	waitMountIdentity(t, started, "boot-key", "boot-id")
	creds.adoptKey("minted-key")
	waitMountIdentity(t, started, "minted-key", "boot-id")
	creds.wipe()
	assertMountIdle(t, started, &active)
	next := config.Default()
	next.Auth.APIKey, next.Agent.ID = "signed-in-key", "new-id"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload()
	waitMountIdentity(t, started, "signed-in-key", "new-id")
	next.Auth.APIKey, next.Agent.ID = "", ""
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload()
	assertMountIdle(t, started, &active)
	next.Auth.APIKey, next.Agent.ID = "bootstrap-key", "bootstrap-id"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload()
	waitMountIdentity(t, started, "bootstrap-key", "bootstrap-id")
	cancel()
	if err := s.stop(time.Second); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 || closed.Load() != 4 {
		t.Fatal("shutdown left a mount session", active.Load(), closed.Load())
	}
}

func assertMountIdle(t *testing.T, started <-chan credentialIdentity, active *atomic.Int32) {
	t.Helper()
	select {
	case <-started:
		t.Fatal("removed credentials restarted a mount")
	case <-time.After(120 * time.Millisecond):
	}
	if active.Load() != 0 {
		t.Fatal("removed identity left readers active")
	}
}

func TestMountSupervisorBootstrapAndBoundedJoin(t *testing.T) {
	creds, _ := storeAt(t, "", "bootstrap-id")
	started := make(chan credentialIdentity, 1)
	release := make(chan struct{})
	s := startRemoteMountSupervisor(context.Background(), config.Default(), creds, func(ctx context.Context, cfg config.Config) error {
		started <- credentialIdentity{key: cfg.Auth.APIKey, agentID: cfg.Agent.ID}
		<-ctx.Done()
		<-release
		return nil
	}, nil)
	creds.adoptKey("new-machine-key")
	waitMountIdentity(t, started, "new-machine-key", "bootstrap-id")
	if err := s.stop(20 * time.Millisecond); err == nil {
		t.Error("unjoined shutdown reported success")
	}
	close(release)
	if err := s.stop(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestMountSupervisorObservesSavedIdentityWhileHealthy(t *testing.T) {
	creds, file := storeAt(t, "old-key", "old-id")
	started := make(chan credentialIdentity, 2)
	s := startRemoteMountSupervisor(context.Background(), config.Default(), creds, func(ctx context.Context, cfg config.Config) error {
		started <- credentialIdentity{key: cfg.Auth.APIKey, agentID: cfg.Agent.ID}
		<-ctx.Done()
		return nil
	}, func() { creds.reload() })
	defer func() {
		if err := s.stop(time.Second); err != nil {
			t.Error(err)
		}
	}()
	waitMountIdentity(t, started, "old-key", "old-id")
	next := config.Default()
	next.Auth.APIKey, next.Agent.ID = "saved-key", "saved-id"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got.key != "saved-key" || got.agentID != "saved-id" {
			t.Fatal("saved identity ignored")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("healthy daemon never picked up saved identity")
	}
}

func TestRemoteLibraryCancellationClosesListener(t *testing.T) {
	cfg := isolatedMountConfig(t)
	cfg.Mount.Listen, cfg.Mount.CacheDir, cfg.Mount.NZBDir = "127.0.0.1:0", t.TempDir(), t.TempDir()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/agent/mount/access" {
			_, _ = w.Write([]byte(`{"allowed":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"accounts":[]}`))
	}))
	defer api.Close()
	cfg.Auth.APIURL = api.URL
	ctx, cancel := context.WithCancel(context.Background())
	s, err := startRemoteLibrary(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer s.Close()
	cancel()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for {
		resp, err := client.Get(s.URL + "/dav/")
		if err != nil {
			break
		}
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("cancelled identity retained a DAV listener")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.Mount.CacheDir); err != nil {
		t.Fatal(err)
	}
}
