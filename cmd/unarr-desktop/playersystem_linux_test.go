package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDesktopExecLine(t *testing.T) {
	const entry = `[Desktop Entry]
Name=Celluloid
Exec=celluloid --new-window %U
MimeType=video/x-matroska;

[Desktop Action NewWindow]
Name=New Window
Exec=celluloid --new-window
`
	got, ok := desktopExecLine(entry)
	if !ok || got != "celluloid --new-window %U" {
		t.Fatalf("desktopExecLine() = (%q, %v), want the [Desktop Entry] Exec", got, ok)
	}

	// A secondary action's Exec is a different command (a "new window" verb,
	// an "enqueue" verb…) and must never be mistaken for how to open a file.
	if _, ok := desktopExecLine("[Desktop Action Enqueue]\nExec=celluloid --enqueue\n"); ok {
		t.Fatal("desktopExecLine() accepted an Exec outside [Desktop Entry]")
	}
}

func TestDesktopEntryArgv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "celluloid.desktop")
	content := "[Desktop Entry]\nExec=/usr/bin/celluloid --new-window %U\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := desktopEntryArgv(path, "https://cdn.example.com/v.mkv")
	if err != nil {
		t.Fatalf("desktopEntryArgv() error = %v", err)
	}
	// %U is a placeholder for the launcher's file list — passing it through
	// would hand the player a literal "%U" to open.
	want := []string{"/usr/bin/celluloid", "--new-window", "https://cdn.example.com/v.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desktopEntryArgv() = %#v, want %#v", got, want)
	}
}

func TestDesktopEntryArgvFlatpakWrapper(t *testing.T) {
	// The case the name-based table can never resolve: no binary on PATH, the
	// entry runs the app through flatpak.
	dir := t.TempDir()
	path := filepath.Join(dir, "flatpak.desktop")
	content := "[Desktop Entry]\nExec=/usr/bin/flatpak run --branch=stable org.videolan.VLC %U\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := desktopEntryArgv(path, "https://cdn.example.com/v.mkv")
	if err != nil {
		t.Fatalf("desktopEntryArgv() error = %v", err)
	}
	want := []string{
		"/usr/bin/flatpak", "run", "--branch=stable", "org.videolan.VLC",
		"https://cdn.example.com/v.mkv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desktopEntryArgv() = %#v, want %#v", got, want)
	}
}

func TestFindDesktopEntry(t *testing.T) {
	origStat := statFile
	t.Cleanup(func() { statFile = origStat })

	dataHome := t.TempDir()
	apps := filepath.Join(dataHome, "applications")
	if err := os.MkdirAll(filepath.Join(apps, "kde4"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"celluloid.desktop", "kde4/dragonplayer.desktop"} {
		if err := os.WriteFile(filepath.Join(apps, name), []byte("[Desktop Entry]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_DATA_DIRS", t.TempDir())
	statFile = os.Stat

	if got, ok := findDesktopEntry("celluloid.desktop"); !ok || got != filepath.Join(apps, "celluloid.desktop") {
		t.Fatalf("findDesktopEntry(celluloid) = (%q, %v)", got, ok)
	}
	// Vendor-prefixed ids are stored as a subdirectory on disk.
	if got, ok := findDesktopEntry("kde4-dragonplayer.desktop"); !ok || got != filepath.Join(apps, "kde4", "dragonplayer.desktop") {
		t.Fatalf("findDesktopEntry(kde4-dragonplayer) = (%q, %v)", got, ok)
	}
	if _, ok := findDesktopEntry("nope.desktop"); ok {
		t.Fatal("findDesktopEntry() found an entry that does not exist")
	}
}

// The end of the chain: nothing installed by name, but the OS names a default
// video app — that must play, instead of dumping the stream in a browser.
func TestAutodetectFallsBackToTheSystemPlayer(t *testing.T) {
	spawned := stubPlayers(t, "linux", nil)
	systemPlayerArgv = func(url string) ([]string, bool) {
		return []string{"/usr/bin/gio", "launch", "/usr/share/applications/celluloid.desktop", url}, true
	}

	req := playRequest{URL: "https://cdn.example.com/v.mkv", Start: 30}
	if code := dispatchPlayer(req); code != 0 {
		t.Fatalf("dispatchPlayer() = %d, want 0", code)
	}
	want := [][]string{{
		"/usr/bin/gio", "launch", "/usr/share/applications/celluloid.desktop",
		"https://cdn.example.com/v.mkv",
	}}
	if !reflect.DeepEqual(*spawned, want) {
		t.Fatalf("spawned = %#v, want %#v", *spawned, want)
	}
}

// A named dialect still wins: it can pass resume/title/languages, which the
// system launch cannot.
func TestKnownPlayerBeatsTheSystemDefault(t *testing.T) {
	spawned := stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
	systemPlayerArgv = func(string) ([]string, bool) { return []string{"/usr/bin/gio"}, true }

	if code := dispatchPlayer(playRequest{URL: "https://cdn.example.com/v.mkv", Start: 30}); code != 0 {
		t.Fatalf("dispatchPlayer() = %d, want 0", code)
	}
	if len(*spawned) != 1 || (*spawned)[0][0] != "/usr/bin/mpv" {
		t.Fatalf("spawned = %#v, want mpv", *spawned)
	}
}

func TestSystemPlayerSelectableByName(t *testing.T) {
	stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
	systemPlayerArgv = func(url string) ([]string, bool) { return []string{"/usr/bin/gio", "launch", "x", url}, true }
	t.Setenv("UNARR_DESKTOP_PLAYER", "system")

	p, ok := selectPlayer(testReq)
	if !ok || p.kind != playerSystem {
		t.Fatalf("selectPlayer() = (%+v, %v), want the system player", p, ok)
	}
}
