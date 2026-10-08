package mountsetup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestDriverFailureMatrix(t *testing.T) {
	for _, mode := range []string{"permission-denied", "container", "installer-failed", "cancelled", "restart-pending"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			calls, prompts := 0, 0
			opts := Options{Output: io.Discard, Confirm: func(string) error { prompts++; return nil }}
			err := prepareDriver(ctx, opts, func() error {
				if mode == "permission-denied" {
					return os.ErrPermission
				}
				if mode == "container" {
					return errors.New("host must expose /dev/fuse")
				}
				return errDriverMissing
			}, func(context.Context, Options) error {
				calls++
				if mode == "installer-failed" {
					return errors.New("installer failed")
				}
				return nil
			})
			if err == nil {
				t.Fatal("failure accepted")
			}
			wantInstall := mode == "installer-failed" || mode == "restart-pending"
			if (calls == 1) != wantInstall || (prompts == 1) != wantInstall {
				t.Fatalf("calls=%d prompts=%d", calls, prompts)
			}
		})
	}
}

func TestSetupWaitForLockCanBeCancelled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a, _ := rcloneArtifact("linux", "amd64")
	dir := t.TempDir()
	// Find the same platform-specific cache directory without acquiring its lock.
	cache := cacheDirectory(dir)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(cache, "install.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := ensureRcloneArtifact(ctx, Options{Directory: dir, Output: io.Discard}, a, http.DefaultClient)
	if err == nil {
		t.Fatal("cancelled lock wait succeeded")
	}
}

func TestDownloadHTTPFailuresLeaveNoArtifacts(t *testing.T) {
	for _, mode := range []string{"404", "truncated", "mid-download-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "404" {
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Content-Length", "1000")
				_, _ = io.WriteString(w, "partial")
				if mode == "mid-download-cancel" {
					w.(http.Flusher).Flush()
					cancel()
				}
			}))
			defer s.Close()
			dir := t.TempDir()
			_, err := download(ctx, s.Client(), artifact{URL: s.URL, SHA256: strings.Repeat("0", 64)}, dir)
			if err == nil {
				t.Fatal("bad download accepted")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatal("download leaked artifacts", files, err)
			}
		})
	}
}
