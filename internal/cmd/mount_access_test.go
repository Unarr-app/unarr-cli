package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestMountUsesConfiguredMirrorsWithoutBypassingPaidDenial(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var mirrorCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer primary.Close()
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mirrorCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer device-key" {
					t.Error("mirror request lost authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/internal/agent/mount/access" {
					_, _ = w.Write([]byte(`{"allowed":true}`))
				} else {
					_, _ = w.Write([]byte(`{"accounts":[]}`))
				}
			}))
			defer mirror.Close()
			cfg := config.Default()
			cfg.Auth.APIURL, cfg.Auth.APIKey = primary.URL, "device-key"
			cfg.Auth.Mirrors = []string{mirror.URL}
			err := probeMountAccount(context.Background(), &cfg)
			if code == http.StatusForbidden {
				if err == nil || mirrorCalls.Load() != 0 {
					t.Fatal("paid denial bypassed through a mirror", err)
				}
				return
			}
			if err != nil {
				t.Fatal("mount preflight ignored working mirror", err)
			}
			source := remoteSources(cfg)[0]
			defer source.Close()
			records, err := source.List(context.Background(), nil)
			if err != nil || len(records) != 0 || mirrorCalls.Load() != 2 {
				t.Fatal("mount source ignored working mirror", err)
			}
		})
	}
}

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
