package main

// Autostart ("Start at login") — shared pure helpers.
//
// Contract implemented by the per-OS files (autostart_linux.go,
// autostart_darwin.go, autostart_windows.go):
//
//	autostartEnabled() (bool, error) — enabled == the artifact exists
//	                                   (.desktop file / LaunchAgent plist /
//	                                   registry Run value)
//	setAutostart(enable bool) error  — create or remove that artifact
//
// Errors are returned, never swallowed (project rule). Everything in this
// file is OS-agnostic — content/path generation plus the file-artifact I/O
// shared by the Linux and macOS backends — so it is unit-testable from any
// OS; only the thin per-OS wrappers carry build tags.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// autostartName names every artifact (the .desktop basename and the
	// registry Run value) so enable/disable and reinstalls always target
	// the same entry.
	autostartName = "unarr-desktop"
	// launchAgentLabel is the launchd job Label; reverse-DNS per launchd
	// convention, and doubles as the plist basename so label and file can
	// never diverge.
	launchAgentLabel = "app.unarr.desktop"
)

// desktopExecValue quotes a path for a Desktop Entry Exec key. Per the spec,
// quoting alone is not enough: inside double quotes the reserved characters
// \ " ` $ must be backslash-escaped, and % is a field-code marker everywhere
// (even quoted), escaped by doubling — otherwise a path like
// /apps/100%tools/unarr-desktop misparses and the entry silently never
// launches while the checkbox claims enabled.
func desktopExecValue(execPath string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"`", "\\`",
		`$`, `\$`,
	).Replace(execPath)
	return `"` + strings.ReplaceAll(escaped, "%", "%%") + `"`
}

// xmlEscape escapes the three characters that would break a plist <string>
// value (& < >) — install paths are user-controlled.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// desktopFileContent renders the freedesktop autostart entry. Exec is
// double-quoted AND escaped: per the Desktop Entry spec, Exec is word-split
// and expands field codes, so a raw install path with spaces or reserved
// characters would break.
func desktopFileContent(execPath string) string {
	return `[Desktop Entry]
Type=Application
Name=unarr-desktop
Comment=unarr agent tray companion
Exec=` + desktopExecValue(execPath) + `
Terminal=false
X-GNOME-Autostart-enabled=true
`
}

// launchAgentPlist renders the launchd agent. ProgramArguments (not Program)
// as a single-element array: launchd execs argv directly, so no shell quoting
// is needed — but the path still lives inside XML, so XML metacharacters must
// be entity-escaped or the plist is unparseable.
func launchAgentPlist(execPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(execPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

// registryRunValue renders the HKCU ...\CurrentVersion\Run command line. Run
// values are parsed like a command line, so a path with spaces must be
// double-quoted or Windows runs the wrong (truncated) program.
func registryRunValue(execPath string) string {
	return `"` + execPath + `"`
}

// autostartDesktopPath resolves the XDG autostart entry path. The XDG rule
// ($XDG_CONFIG_HOME, else ~/.config) is applied by hand instead of via
// os.UserConfigDir so the env override holds — and is testable — on any OS.
func autostartDesktopPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", autostartName+".desktop"), nil
}

// launchAgentPath resolves the per-user LaunchAgents plist path.
func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// artifactExists reports whether the autostart artifact at path exists — on
// the file-backed OSes (Linux/macOS) existence IS the enabled state.
func artifactExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// writeArtifact writes the autostart artifact, creating the parent directory
// first: a fresh ~/.config has no autostart/, a fresh macOS account no
// LaunchAgents/.
func writeArtifact(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// removeArtifact deletes the autostart artifact. Already-absent is success:
// disable must be idempotent (the user may have removed the file by hand).
func removeArtifact(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
