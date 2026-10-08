package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestMountSetupRefusalAndCancellationPreserveConfig(t *testing.T) {
	for _, reason := range []string{"refusal", "cancel before setup", "cancel after setup", "invalid settings"} {
		t.Run(reason, func(t *testing.T) {
			cfg := isolatedMountConfig(t)
			cfg.Mount.Enabled = false
			before := saveMountFixture(t, cfg)
			cfg.Mount.Enabled, cfg.Mount.Directory = true, t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if reason == "cancel before setup" {
				cancel()
			}
			if reason == "invalid settings" {
				cfg.Mount.Directory = "relative"
			}
			calls := 0
			err := savePreparedMount(ctx, cfg, func(ctx context.Context) (string, error) {
				calls++
				if reason == "refusal" {
					return "", errors.New("installation cancelled")
				}
				if reason == "cancel after setup" {
					cancel()
				}
				return "synthetic-rclone", ctx.Err()
			})
			if err == nil {
				t.Fatal("failed preflight reported success")
			}
			if reason == "invalid settings" && calls != 0 {
				t.Fatal("invalid settings reached dependency setup")
			}
			after, err := os.ReadFile(resolvedConfigPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("failed activation changed config", err)
			}
			if appCfg.Mount.Enabled {
				t.Fatal("failed activation changed in-memory intention")
			}
		})
	}
}

func TestMountDefaultOffAndPaidDenialsNeverInstallOrSave(t *testing.T) {
	for _, plan := range []string{"disabled", "free", "trial", "expired"} {
		t.Run(plan, func(t *testing.T) {
			cfg := isolatedMountConfig(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"paid_plan_required"}`))
			}))
			defer srv.Close()
			cfg.Auth.APIURL, cfg.Mount.Enabled = srv.URL, plan != "disabled"
			before := saveMountFixture(t, cfg)
			c := newMountCmd()
			c.SetContext(context.Background())
			if err := runMountCommand(c, nil); err == nil {
				t.Fatal("unauthorized mount succeeded")
			}
			after, err := os.ReadFile(resolvedConfigPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("denial changed saved identity/settings", err)
			}
			if plan == "disabled" && calls.Load() != 0 {
				t.Fatal("disabled mount made provider request")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(resolvedConfigPath()), "tools")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("denial prepared dependencies")
			}
		})
	}
}

func TestPersistentUmountRejectsOtherConfigAndEnvironment(t *testing.T) {
	for _, key := range []string{"custom config", "UNARR_CONFIG_DIR", "UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR"} {
		t.Run(key, func(t *testing.T) {
			cfg := isolatedMountConfig(t)
			if key == "custom config" {
				cfgFile = filepath.Join(t.TempDir(), "other.toml")
			} else {
				t.Setenv(key, t.TempDir())
			}
			before := saveMountFixture(t, cfg)
			c := newUmountCmd()
			err := c.RunE(c, nil)
			if err == nil || !strings.Contains(err.Error(), "default") {
				t.Fatal("ambiguous service accepted", err)
			}
			after, err := os.ReadFile(resolvedConfigPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("guard modified another config", err)
			}
			unchanged, err := config.Load(resolvedConfigPath())
			if err != nil || !unchanged.Mount.Enabled {
				t.Fatal("guard disabled unrelated mount", err)
			}
		})
	}
}

func TestMountSuccessfulPreflightSavesCompleteIntention(t *testing.T) {
	cfg := isolatedMountConfig(t)
	cfg.Mount.Enabled = false
	saveMountFixture(t, cfg)
	cfg.Mount.Enabled, cfg.Mount.Directory = true, t.TempDir()
	calls := 0
	if err := savePreparedMount(context.Background(), cfg, func(context.Context) (string, error) {
		calls++
		prior, err := config.Load(resolvedConfigPath())
		if err != nil || prior.Mount.Enabled {
			t.Fatal("enabled before setup completed", err)
		}
		return "synthetic-rclone", nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(resolvedConfigPath())
	if err != nil || calls != 1 || !after.Mount.Enabled || after.Mount.Directory != cfg.Mount.Directory {
		t.Fatal("incomplete activation intention", err)
	}
}
