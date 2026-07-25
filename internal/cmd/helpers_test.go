package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/Documents", home + "/Documents"},
		{"~/", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~notexpanded", "~notexpanded"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := expandHome(tt.input)
			if got != tt.want {
				t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSafeBrowserURL(t *testing.T) {
	good := []string{
		"http://localhost:3000",
		"https://torrentclaw.com/some/path?q=1",
	}
	bad := []string{
		"--help",
		"-version",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html,foo",
		"ftp://example.com",
		"",
	}
	for _, u := range good {
		if !isSafeBrowserURL(u) {
			t.Errorf("isSafeBrowserURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if isSafeBrowserURL(u) {
			t.Errorf("isSafeBrowserURL(%q) = true, want false", u)
		}
	}
}

func TestOpenBrowserRejectsUnsafeURL(t *testing.T) {
	// Only the reject path is exercised — a safe URL would actually spawn a
	// browser on the test machine.
	for _, u := range []string{"--help", "file:///etc/passwd", ""} {
		if err := openBrowser(u); err == nil {
			t.Errorf("openBrowser(%q) = nil, want error", u)
		}
	}
}

func TestDefaultAPIURLMatchesConfigDefault(t *testing.T) {
	// The wizard, login and up all fall back through this; drifting from
	// config.Default() would silently point a fresh install at another server.
	if got, want := defaultAPIURL(), config.Default().Auth.APIURL; got != want {
		t.Errorf("defaultAPIURL() = %q, want %q", got, want)
	}
	if defaultAPIURL() == "" {
		t.Error("defaultAPIURL() is empty")
	}
}

func TestDefaultDownloadDir(t *testing.T) {
	dir := defaultDownloadDir()
	if dir == "" {
		t.Error("defaultDownloadDir() returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(dir, home) {
		t.Errorf("defaultDownloadDir() = %q, expected to start with home dir %q", dir, home)
	}
}

// setupHint must never send a container user to the interactive wizard: there is
// no tty and no browser inside, so `unarr init` is a dead end there.
func TestSetupHintPointsAtAuthKeyInDocker(t *testing.T) {
	t.Setenv("UNARR_DOCKER", "1")
	hint := setupHint("https://unarr.app")
	if !strings.Contains(hint, "UNARR_AUTHKEY") {
		t.Errorf("docker hint should name UNARR_AUTHKEY, got %q", hint)
	}
	if strings.Contains(hint, "unarr init") {
		t.Errorf("docker hint must not send the user to the wizard, got %q", hint)
	}
	if !strings.Contains(hint, "https://unarr.app/profile?tab=agents") {
		t.Errorf("hint should say where the key comes from, got %q", hint)
	}
}

// With no api_url configured the hint still has to name a host the user can open.
func TestSetupHintFallsBackToDefaultAPIURL(t *testing.T) {
	t.Setenv("UNARR_DOCKER", "1")
	if hint := setupHint(""); !strings.Contains(hint, defaultAPIURL()) {
		t.Errorf("hint should fall back to the default API URL, got %q", hint)
	}
}
