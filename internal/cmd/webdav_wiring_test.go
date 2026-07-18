package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/fatih/color"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// TestSetupWebDAV covers the opt-in gate plus the invariant that the mount is
// armed ONLY when a usable credential exists — status must never advertise a
// mount the daemon refused to start. davfs.New is lazy, so no disk is touched.
func TestSetupWebDAV(t *testing.T) {
	t.Run("disabled → not armed", func(t *testing.T) {
		ss := engine.NewStreamServer(0, 1)
		setupWebDAV(ss, config.Config{Download: config.DownloadConfig{WebDAVEnabled: false}})
		if ss.WebDAVEnabled() {
			t.Error("WebDAVEnabled() = true, want false when webdav_enabled is off")
		}
	})

	t.Run("enabled but no password and no API key → not armed", func(t *testing.T) {
		ss := engine.NewStreamServer(0, 1)
		setupWebDAV(ss, config.Config{
			Download: config.DownloadConfig{WebDAVEnabled: true},
			Auth:     config.AuthConfig{APIKey: ""},
		})
		if ss.WebDAVEnabled() {
			t.Error("WebDAVEnabled() = true, want false with no derivable credential")
		}
	})

	t.Run("enabled + API key → armed (password derived from the stable key)", func(t *testing.T) {
		ss := engine.NewStreamServer(0, 1)
		setupWebDAV(ss, config.Config{
			Download: config.DownloadConfig{WebDAVEnabled: true},
			Auth:     config.AuthConfig{APIKey: "tc_testkey_abcdef0123456789"},
		})
		if !ss.WebDAVEnabled() {
			t.Error("WebDAVEnabled() = false, want true when the API key can derive a password")
		}
	})

	t.Run("enabled + explicit password, no API key → armed", func(t *testing.T) {
		ss := engine.NewStreamServer(0, 1)
		setupWebDAV(ss, config.Config{
			Download: config.DownloadConfig{WebDAVEnabled: true, WebDAVPassword: "explicit-pw"},
		})
		if !ss.WebDAVEnabled() {
			t.Error("WebDAVEnabled() = false, want true with an explicit webdav_password")
		}
	})
}

// TestPrintWebDAVStatus: the status block advertises the local /dav/ URL, the
// Basic-auth user, and the (already-resolved) password so a user can mount it.
func TestPrintWebDAVStatus(t *testing.T) {
	cfg := config.Config{Download: config.DownloadConfig{StreamPort: 11818}}

	out := captureStdout(t, func() {
		printWebDAVStatus(cfg, "unarr", "derived-pass-123")
	})

	for _, want := range []string{
		"WebDAV (read-only library)",
		"http://127.0.0.1:11818/dav/",
		"User:       unarr",
		"Password:   derived-pass-123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printWebDAVStatus output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// captureStdout redirects both os.Stdout (fmt.Printf) and color.Output (the
// fatih/color calls) to a pipe while fn runs, and returns everything written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origStdout, origColorOut, origNoColor := os.Stdout, color.Output, color.NoColor
	os.Stdout = w
	color.Output = w
	color.NoColor = true // strip ANSI so assertions match plain text
	defer func() {
		os.Stdout, color.Output, color.NoColor = origStdout, origColorOut, origNoColor
	}()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
