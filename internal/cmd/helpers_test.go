package cmd

import (
	"os"
	"path/filepath"
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
		// expandHome joins with filepath.Join, so the separator is native.
		{"~/Documents", filepath.Join(home, "Documents")},
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

// The non-Docker branches: a headless host (systemd unit, provisioning script,
// CI) gets the auth-key command, and only a real terminal gets the wizard.
// Exercised through setupHintFor because isTerminal() is true under `go test`
// (stdin is /dev/null, itself a character device).
func TestSetupHintNonDockerBranches(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		want        string
		notWant     string
	}{
		{"headless", false, "unarr up --auth-key", "unarr init"},
		{"terminal", true, "unarr init", "--auth-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := setupHintFor("https://mirror.example.com", false, tt.interactive)
			if !strings.Contains(hint, tt.want) {
				t.Errorf("hint %q should contain %q", hint, tt.want)
			}
			if strings.Contains(hint, tt.notWant) {
				t.Errorf("hint %q should not contain %q", hint, tt.notWant)
			}
		})
	}
}

// A user on a mirror or a self-hosted server must be sent to THEIR server —
// pointing them at unarr.app, where their key does not exist, is the dead end.
func TestSetupHintUsesConfiguredAPIURL(t *testing.T) {
	const mirror = "https://unarr.example.net"
	for _, inDocker := range []bool{true, false} {
		hint := setupHintFor(mirror, inDocker, false)
		if !strings.Contains(hint, mirror+"/profile?tab=agents") {
			t.Errorf("inDocker=%v: hint %q should point at the configured server", inDocker, hint)
		}
		if strings.Contains(hint, config.Default().Auth.APIURL) {
			t.Errorf("inDocker=%v: hint %q leaked the default server", inDocker, hint)
		}
	}
}

// With no api_url configured the hint still has to name a host the user can open.
func TestSetupHintFallsBackToDefaultAPIURL(t *testing.T) {
	if hint := setupHintFor("", true, false); !strings.Contains(hint, config.Default().Auth.APIURL) {
		t.Errorf("hint should fall back to the default API URL, got %q", hint)
	}
}

// The sudo guard has to fire for every shape of "a normal user typed sudo" and
// stay quiet in environments that are legitimately root-only — a false positive
// bricks Docker/NAS installs, a false negative writes config.toml and the
// systemd USER unit under /root.
func TestIsSudoEnv(t *testing.T) {
	tests := []struct {
		name                           string
		euid                           int
		sudoUser, sudoUID, home, login string
		want                           bool
	}{
		{"normal user", 1000, "", "", "/home/dave", "dave", false},
		{"sudo unarr init", 0, "dave", "1000", "/root", "dave", true},
		{"sudo -i (SUDO_USER dropped)", 0, "", "1000", "/root", "dave", true},
		{"sudo su - (all SUDO_* dropped)", 0, "", "", "/root", "dave", true},
		{"container as root (no login name)", 0, "", "", "/root", "", false},
		{"root-owned systemd unit", 0, "", "", "/root", "", false},
		{"real root login", 0, "", "", "/root", "root", false},
		{"root with its own HOME (NAS shell)", 0, "", "", "/volume1/homes/admin", "admin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSudoEnv(tt.euid, tt.sudoUser, tt.sudoUID, tt.home, tt.login); got != tt.want {
				t.Errorf("isSudoEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
