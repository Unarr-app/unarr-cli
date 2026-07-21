package main

// Linux backend for the system-default player: freedesktop MIME association.
//
//	xdg-mime query default video/x-matroska  →  <id>.desktop
//	→ find that entry under the XDG data dirs
//	→ `gio launch <entry> <url>`, else its own Exec line
//
// gio is preferred because it is the only thing that gets ALL of this right:
// Flatpak/Snap wrappers, D-Bus activation, and the Exec field codes. Parsing
// the entry ourselves is the fallback for a system without glib tools.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultVideoPlayerArgv resolves the user's default video application and
// returns the argv that opens url with it.
func defaultVideoPlayerArgv(url string) ([]string, error) {
	entry, err := defaultVideoDesktopEntry()
	if err != nil {
		return nil, err
	}
	if gio, lookErr := lookPath("gio"); lookErr == nil {
		return []string{gio, "launch", entry, url}, nil
	}
	return desktopEntryArgv(entry, url)
}

// defaultVideoDesktopEntry returns the absolute path of the .desktop entry
// registered for video, or an error naming what was missing.
func defaultVideoDesktopEntry() (string, error) {
	xdgMime, err := lookPath("xdg-mime")
	if err != nil {
		return "", fmt.Errorf("xdg-mime not installed")
	}
	for _, mime := range videoProbeTypes {
		out, qErr := exec.Command(xdgMime, "query", "default", mime).Output()
		id := strings.TrimSpace(string(out))
		if qErr != nil || id == "" {
			continue
		}
		if path, ok := findDesktopEntry(id); ok {
			return path, nil
		}
		return "", fmt.Errorf("default handler %q is registered but its .desktop entry was not found", id)
	}
	return "", fmt.Errorf("no default application registered for video")
}

// findDesktopEntry locates a .desktop id under the XDG data dirs. A "vendor
// subdir" id (`kde-mplayer.desktop` → `kde/mplayer.desktop`) is also tried,
// which is how such ids are stored on disk.
func findDesktopEntry(id string) (string, bool) {
	candidates := []string{id}
	if vendor, rest, found := strings.Cut(id, "-"); found {
		candidates = append(candidates, filepath.Join(vendor, rest))
	}
	for _, dir := range desktopEntryDirs() {
		for _, cand := range candidates {
			path := filepath.Join(dir, cand)
			if _, err := statFile(path); err == nil {
				return path, true
			}
		}
	}
	return "", false
}

// desktopEntryDirs lists the applications/ directories in XDG precedence
// order: the user's own entries first, then the system ones (defaults per the
// spec when the env vars are unset).
func desktopEntryDirs() []string {
	var dirs []string
	if home := os.Getenv("XDG_DATA_HOME"); home != "" {
		dirs = append(dirs, filepath.Join(home, "applications"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "applications"))
	}
	system := os.Getenv("XDG_DATA_DIRS")
	if system == "" {
		system = "/usr/local/share:/usr/share"
	}
	for _, dir := range strings.Split(system, ":") {
		if dir != "" {
			dirs = append(dirs, filepath.Join(dir, "applications"))
		}
	}
	return dirs
}

// desktopEntryArgv builds the argv from an entry's own Exec line: field codes
// (%u, %f, …) are dropped and the URL appended, per the desktop entry spec.
// Only used when gio is unavailable.
func desktopEntryArgv(entryPath, url string) ([]string, error) {
	data, err := os.ReadFile(entryPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", entryPath, err)
	}
	execLine, ok := desktopExecLine(string(data))
	if !ok {
		return nil, fmt.Errorf("%s has no Exec line", entryPath)
	}
	argv := make([]string, 0, 4)
	for _, tok := range splitCommand(execLine) {
		// Field codes are placeholders for the file/URL list and for metadata
		// the launcher supplies; passing them through would hand the player a
		// literal "%u" to open.
		if len(tok) == 2 && tok[0] == '%' {
			continue
		}
		argv = append(argv, tok)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s has an empty Exec line", entryPath)
	}
	return append(argv, url), nil
}

// desktopExecLine extracts Exec= from the [Desktop Entry] group. Later groups
// ([Desktop Action …]) have their own Exec lines for secondary actions — those
// are NOT what a plain "open this file" launch should use.
func desktopExecLine(content string) (string, bool) {
	inMainGroup := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inMainGroup = line == "[Desktop Entry]"
			continue
		}
		if inMainGroup && strings.HasPrefix(line, "Exec=") {
			return strings.TrimPrefix(line, "Exec="), true
		}
	}
	return "", false
}
