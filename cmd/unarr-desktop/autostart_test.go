package main

// Tests for the untagged autostart helpers (autostart.go) only — pure
// content/path generation, so they run on any OS regardless of which
// per-OS backend is compiled in.

import (
	"strings"
	"testing"
)

func TestDesktopFileContent(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
	}{
		{"plain path", "/usr/local/bin/unarr-desktop"},
		{"path with spaces", "/opt/my apps/unarr-desktop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := desktopFileContent(tt.execPath)
			for _, want := range []string{
				"[Desktop Entry]",
				"Type=Application",
				"Terminal=false",
				"X-GNOME-Autostart-enabled=true",
				// Exec is word-split per the Desktop Entry spec, so the
				// path must be double-quoted (spaces would break it).
				`Exec="` + tt.execPath + `"`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("desktopFileContent(%q) missing %q in:\n%s", tt.execPath, want, got)
				}
			}
		})
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
	}{
		{"plain path", "/Applications/unarr-desktop"},
		{"path with spaces", "/opt/my apps/unarr-desktop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := launchAgentPlist(tt.execPath)
			for _, want := range []string{
				"<string>app.unarr.desktop</string>",
				// launchd execs ProgramArguments directly — the path goes
				// in unquoted, spaces and all.
				"<string>" + tt.execPath + "</string>",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("launchAgentPlist(%q) missing %q in:\n%s", tt.execPath, want, got)
				}
			}
			// RunAtLoad must be a true boolean, not merely mentioned.
			idx := strings.Index(got, "<key>RunAtLoad</key>")
			if idx == -1 {
				t.Fatalf("launchAgentPlist(%q) missing <key>RunAtLoad</key>:\n%s", tt.execPath, got)
			}
			if !strings.Contains(got[idx:], "<true/>") {
				t.Errorf("launchAgentPlist(%q): <key>RunAtLoad</key> not followed by <true/>:\n%s", tt.execPath, got)
			}
		})
	}
}

func TestRegistryRunValue(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
		want     string
	}{
		{"plain path", `C:\Program Files\unarr\unarr-desktop.exe`, `"C:\Program Files\unarr\unarr-desktop.exe"`},
		{"no spaces", `C:\unarr\unarr-desktop.exe`, `"C:\unarr\unarr-desktop.exe"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registryRunValue(tt.execPath)
			if got != tt.want {
				t.Errorf("registryRunValue(%q) = %q, want %q", tt.execPath, got, tt.want)
			}
			// Exactly one pair of quotes — wrapping twice would produce a
			// command line Windows cannot execute.
			if strings.Count(got, `"`) != 2 {
				t.Errorf("registryRunValue(%q) = %q: want exactly one pair of double quotes", tt.execPath, got)
			}
		})
	}
}

func TestAutostartDesktopPath(t *testing.T) {
	t.Run("XDG_CONFIG_HOME set", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
		got, err := autostartDesktopPath()
		if err != nil {
			t.Fatalf("autostartDesktopPath() error: %v", err)
		}
		if want := "/custom/cfg/autostart/unarr-desktop.desktop"; got != want {
			t.Errorf("autostartDesktopPath() = %q, want %q", got, want)
		}
	})
	t.Run("empty XDG_CONFIG_HOME falls back to HOME", func(t *testing.T) {
		// Per the XDG spec an empty XDG_CONFIG_HOME means unset — the
		// fallback to $HOME/.config must apply, not an "/autostart/..."
		// path rooted at the empty string.
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/u")
		got, err := autostartDesktopPath()
		if err != nil {
			t.Fatalf("autostartDesktopPath() error: %v", err)
		}
		if want := "/home/u/.config/autostart/unarr-desktop.desktop"; got != want {
			t.Errorf("autostartDesktopPath() = %q, want %q", got, want)
		}
	})
}

func TestLaunchAgentPath(t *testing.T) {
	t.Setenv("HOME", "/Users/u")
	got, err := launchAgentPath()
	if err != nil {
		t.Fatalf("launchAgentPath() error: %v", err)
	}
	if want := "/Users/u/Library/LaunchAgents/app.unarr.desktop.plist"; got != want {
		t.Errorf("launchAgentPath() = %q, want %q", got, want)
	}
}

func TestDesktopExecValueEscaping(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/usr/local/bin/unarr-desktop", `"/usr/local/bin/unarr-desktop"`},
		{"/home/u/my apps/unarr-desktop", `"/home/u/my apps/unarr-desktop"`},
		{"/apps/100%tools/unarr-desktop", `"/apps/100%%tools/unarr-desktop"`},
		{`/apps/pa$h/unarr-desktop`, `"/apps/pa\$h/unarr-desktop"`},
		{`/apps/a"b/unarr-desktop`, `"/apps/a\"b/unarr-desktop"`},
		{`/apps/back\slash/unarr-desktop`, `"/apps/back\\slash/unarr-desktop"`},
	}
	for _, tt := range tests {
		if got := desktopExecValue(tt.in); got != tt.want {
			t.Errorf("desktopExecValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLaunchAgentPlistXMLEscaping(t *testing.T) {
	content := launchAgentPlist("/apps/a&b <c>/unarr-desktop")
	if !strings.Contains(content, "<string>/apps/a&amp;b &lt;c&gt;/unarr-desktop</string>") {
		t.Errorf("plist did not XML-escape the path:\n%s", content)
	}
}
