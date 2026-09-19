package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestPaidMountDeniedBeforeOpeningCache(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/mount/access" {
			t.Error(r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"mount_paid_required","error":"Paid plan required"}`))
	}))
	defer web.Close()
	cfg := config.Default()
	cfg.Auth.APIURL, cfg.Auth.APIKey = web.URL, "device-key"
	cfg.Mount.Enabled = true
	cfg.Mount.CacheDir = filepath.Join(t.TempDir(), "no-cache")
	cfg.Mount.NZBDir = t.TempDir()
	if _, err := startRemoteLibrary(context.Background(), cfg); err == nil {
		t.Fatal("free user started mount")
	}
	if _, err := os.Stat(cfg.Mount.CacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache created before access grant")
	}
}

func TestMountAccessRevocationStopsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	revoked := errors.New("paid access revoked")
	stopped := make(chan error, 1)
	checks := 0
	go watchMountAccess(ctx, func(context.Context) error {
		checks++
		if checks == 1 {
			return nil
		}
		return revoked
	}, time.Millisecond,
		func(err error) { stopped <- err })
	select {
	case err := <-stopped:
		if !errors.Is(err, revoked) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("revoked mount continued running")
	}
}
