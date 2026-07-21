package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testReq is the request selection is exercised with. Only the URL matters:
// the system-default layer resolves a command line for a specific URL.
var testReq = playRequest{URL: "https://cdn.example.com/v.mkv"}

// stubPlayers swaps the discovery/spawn seams for the duration of a test:
// only binaries in `installed` resolve, hostGOOS is forced, notifications are
// swallowed, and every spawned argv is captured instead of executed. The
// system-default lookup answers "no handler" unless a test says otherwise —
// it would otherwise shell out to the developer's real desktop.
// Config is isolated to an empty temp dir so a developer's real
// ~/.config/unarr/config.toml can never leak a [desktop] override into tests.
func stubPlayers(t *testing.T, goos string, installed map[string]string) *[][]string {
	t.Helper()
	origLook, origStat, origGOOS := lookPath, statFile, hostGOOS
	origStart, origNotify, origBrowser := startProc, notifySend, openInBrowser
	origSystem := systemPlayerArgv
	t.Cleanup(func() {
		lookPath, statFile, hostGOOS = origLook, origStat, origGOOS
		startProc, notifySend, openInBrowser = origStart, origNotify, origBrowser
		systemPlayerArgv = origSystem
	})
	systemPlayerArgv = func(string) ([]string, bool) { return nil, false }
	t.Setenv("UNARR_CONFIG_DIR", t.TempDir())
	t.Setenv("UNARR_DESKTOP_PLAYER", "")
	t.Setenv("UNARR_DESKTOP_PLAYER_COMMAND", "")

	hostGOOS = goos
	lookPath = func(name string) (string, error) {
		if p, ok := installed[name]; ok {
			return p, nil
		}
		return "", errors.New("not found: " + name)
	}
	statFile = func(name string) (os.FileInfo, error) {
		if _, ok := installed[name]; ok {
			return nil, nil // only existence is checked
		}
		return nil, os.ErrNotExist
	}
	var spawned [][]string
	startProc = func(argv []string) error {
		spawned = append(spawned, argv)
		return nil
	}
	notifySend = func(title, body string) {}
	openInBrowser = func(u string) error {
		spawned = append(spawned, []string{"browser", u})
		return nil
	}
	return &spawned
}

func TestSelectPlayerAutodetect(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		installed map[string]string
		wantKind  playerKind
		wantBin   string
		wantOK    bool
	}{
		{"mpv beats vlc", "linux", map[string]string{"mpv": "/usr/bin/mpv", "vlc": "/usr/bin/vlc"}, playerMPV, "/usr/bin/mpv", true},
		{"vlc when no mpv", "linux", map[string]string{"vlc": "/usr/bin/vlc"}, playerVLC, "/usr/bin/vlc", true},
		{
			// IINA "installed" but we're not on darwin — must never be picked.
			name:      "iina ignored off darwin",
			goos:      "linux",
			installed: map[string]string{iinaAppPath: iinaAppPath, "open": "/usr/bin/open"},
			wantOK:    false,
		},
		{
			name:      "mpc ignored off windows",
			goos:      "linux",
			installed: map[string]string{"mpc-hc64.exe": `C:\mpc\mpc-hc64.exe`},
			wantOK:    false,
		},
		{
			name:      "iina on darwin via open",
			goos:      "darwin",
			installed: map[string]string{iinaAppPath: iinaAppPath, "open": "/usr/bin/open"},
			wantKind:  playerIINA,
			wantBin:   "/usr/bin/open",
			wantOK:    true,
		},
		{
			name:      "mpc on windows, 64-bit name preferred",
			goos:      "windows",
			installed: map[string]string{"mpc-hc64.exe": `C:\mpc\mpc-hc64.exe`, "mpc-hc.exe": `C:\mpc\mpc-hc.exe`},
			wantKind:  playerMPC,
			wantBin:   `C:\mpc\mpc-hc64.exe`,
			wantOK:    true,
		},
		{"nothing installed", "linux", nil, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubPlayers(t, tt.goos, tt.installed)
			p, ok := selectPlayer(testReq)
			if ok != tt.wantOK {
				t.Fatalf("selectPlayer() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (p.kind != tt.wantKind || p.bin != tt.wantBin) {
				t.Errorf("selectPlayer() = {%s %s}, want {%s %s}", p.kind, p.bin, tt.wantKind, tt.wantBin)
			}
		})
	}
}

func TestSelectPlayerOverrides(t *testing.T) {
	t.Run("env override wins over autodetect", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv", "vlc": "/usr/bin/vlc"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "vlc")
		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerVLC {
			t.Fatalf("selectPlayer() = (%+v, %v), want vlc", p, ok)
		}
	})
	t.Run("unresolvable override falls back to autodetect", func(t *testing.T) {
		// iina on linux can never resolve — playing via mpv beats failing.
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "iina")
		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerMPV {
			t.Fatalf("selectPlayer() = (%+v, %v), want mpv fallback", p, ok)
		}
	})
	t.Run("unresolvable override tells the user why", func(t *testing.T) {
		// The bug: `player = "mpv"` with mpv not installed played through VLC
		// on every click, warning only on a stderr nobody reads (the handler
		// has no terminal) — the setting looked ignored.
		stubPlayers(t, "linux", map[string]string{"vlc": "/usr/bin/vlc"})
		var notes [][2]string
		notifySend = func(title, body string) { notes = append(notes, [2]string{title, body}) }
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")

		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerVLC {
			t.Fatalf("selectPlayer() = (%+v, %v), want vlc fallback", p, ok)
		}
		if len(notes) != 1 {
			t.Fatalf("notifications = %v, want exactly one", notes)
		}
		if !strings.Contains(notes[0][0], "mpv") || !strings.Contains(notes[0][1], "vlc") {
			t.Fatalf("notification %q / %q must name both the missing and the used player", notes[0][0], notes[0][1])
		}
	})
	t.Run("no notification when the override resolves", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv", "vlc": "/usr/bin/vlc"})
		notified := false
		notifySend = func(string, string) { notified = true }
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")
		if _, ok := selectPlayer(testReq); !ok || notified {
			t.Fatalf("ok=%v notified=%v, want a silent successful selection", ok, notified)
		}
	})
	t.Run("mpv resolves celluloid when mpv is not installed", func(t *testing.T) {
		// Celluloid EMBEDS mpv and installs no `mpv` binary, so a machine with
		// only Celluloid used to fall through to VLC on `player = "mpv"`.
		stubPlayers(t, "linux", map[string]string{"celluloid": "/usr/bin/celluloid", "vlc": "/usr/bin/vlc"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")
		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerCelluloid || p.bin != "/usr/bin/celluloid" {
			t.Fatalf("selectPlayer() = (%+v, %v), want celluloid", p, ok)
		}
	})
	t.Run("real mpv beats celluloid", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv", "celluloid": "/usr/bin/celluloid"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")
		if p, ok := selectPlayer(testReq); !ok || p.kind != playerMPV {
			t.Fatalf("selectPlayer() = (%+v, %v), want mpv", p, ok)
		}
	})
	t.Run("web player needs nothing installed", func(t *testing.T) {
		stubPlayers(t, "linux", nil)
		t.Setenv("UNARR_DESKTOP_PLAYER", "web")
		if p, ok := selectPlayer(testReq); !ok || p.kind != playerWeb {
			t.Fatalf("selectPlayer() = (%+v, %v), want web", p, ok)
		}
	})
	t.Run("autodetect never picks the web player", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"vlc": "/usr/bin/vlc"})
		if p, ok := selectPlayer(testReq); !ok || p.kind != playerVLC {
			t.Fatalf("selectPlayer() = (%+v, %v), want vlc", p, ok)
		}
	})
	t.Run("config toml [desktop] player honored", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv", "vlc": "/usr/bin/vlc"})
		dir := t.TempDir()
		cfg := "[desktop]\nplayer = \"vlc\"\n"
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("UNARR_CONFIG_DIR", dir)
		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerVLC {
			t.Fatalf("selectPlayer() = (%+v, %v), want vlc from config", p, ok)
		}
	})
	t.Run("env beats config", func(t *testing.T) {
		stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv", "vlc": "/usr/bin/vlc"})
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[desktop]\nplayer = \"vlc\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("UNARR_CONFIG_DIR", dir)
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")
		p, ok := selectPlayer(testReq)
		if !ok || p.kind != playerMPV {
			t.Fatalf("selectPlayer() = (%+v, %v), want mpv from env", p, ok)
		}
	})
}

func TestBuildPlayerArgv(t *testing.T) {
	full := playRequest{
		URL:   "https://cdn.example.com/v.mkv",
		Start: 90,
		Title: "My Show",
		ALang: []string{"es", "en"},
		SLang: []string{"es"},
	}
	minimal := playRequest{URL: "https://cdn.example.com/v.mkv"}

	tests := []struct {
		name    string
		p       player
		req     playRequest
		want    []string
		wantErr bool
	}{
		{
			name: "mpv full",
			p:    player{kind: playerMPV, bin: "/usr/bin/mpv"},
			req:  full,
			want: []string{
				"/usr/bin/mpv",
				"--start=90",
				"--force-media-title=My Show",
				"--alang=es,en",
				"--slang=es",
				"--",
				"https://cdn.example.com/v.mkv",
			},
		},
		{
			name: "mpv minimal keeps terminator",
			p:    player{kind: playerMPV, bin: "/usr/bin/mpv"},
			req:  minimal,
			want: []string{"/usr/bin/mpv", "--", "https://cdn.example.com/v.mkv"},
		},
		{
			name: "vlc full",
			p:    player{kind: playerVLC, bin: "/usr/bin/vlc"},
			req:  full,
			want: []string{
				"/usr/bin/vlc",
				"--start-time=90",
				"--meta-title=My Show",
				"--audio-language=es,en",
				"--sub-language=es",
				"--",
				"https://cdn.example.com/v.mkv",
			},
		},
		{
			name: "iina via open, no extras in v1",
			p:    player{kind: playerIINA, bin: "/usr/bin/open"},
			req:  full,
			want: []string{"/usr/bin/open", "-a", "IINA", "https://cdn.example.com/v.mkv"},
		},
		{
			name: "mpc url plus start in ms",
			p:    player{kind: playerMPC, bin: `C:\mpc\mpc-hc64.exe`},
			req:  full,
			want: []string{`C:\mpc\mpc-hc64.exe`, "https://cdn.example.com/v.mkv", "/start", "90000"},
		},
		{
			// The parser can't produce these URLs (http/https only), but the
			// builder must hold on its own: mpc-hc has no `--` terminator.
			name:    "mpc refuses dash url",
			p:       player{kind: playerMPC, bin: `C:\mpc\mpc-hc64.exe`},
			req:     playRequest{URL: "--evil"},
			wantErr: true,
		},
		{
			name:    "mpc refuses slash url",
			p:       player{kind: playerMPC, bin: `C:\mpc\mpc-hc64.exe`},
			req:     playRequest{URL: "/dvd"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPlayerArgv(tt.p, tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildPlayerArgv() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildPlayerArgv() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildPlayerArgv() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildPlayerArgvTerminatorPosition pins the security invariant directly:
// for mpv/vlc the URL is the LAST element and `--` sits immediately before it,
// whatever optional flags precede them.
func TestBuildPlayerArgvTerminatorPosition(t *testing.T) {
	req := playRequest{URL: "https://x.example/v", Start: 5, Title: "t"}
	for _, p := range []player{{kind: playerMPV, bin: "mpv"}, {kind: playerVLC, bin: "vlc"}} {
		argv, err := buildPlayerArgv(p, req)
		if err != nil {
			t.Fatalf("%s: %v", p.kind, err)
		}
		n := len(argv)
		if n < 2 || argv[n-1] != req.URL || argv[n-2] != "--" {
			t.Errorf("%s argv = %q: want ...,\"--\",%q", p.kind, argv, req.URL)
		}
	}
}

func TestDispatchPlayer(t *testing.T) {
	req := playRequest{URL: "https://cdn.example.com/v.mkv", Start: 90}

	t.Run("launches detected player", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0", code)
		}
		want := [][]string{{"/usr/bin/mpv", "--start=90", "--", "https://cdn.example.com/v.mkv"}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Errorf("spawned = %q, want %q", *spawned, want)
		}
	})
	t.Run("no player falls back to browser", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", nil)
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0 (browser fallback)", code)
		}
		want := [][]string{{"browser", "https://cdn.example.com/v.mkv"}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Errorf("spawned = %q, want %q", *spawned, want)
		}
	})
	t.Run("spawn failure falls back to browser", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
		startProc = func(argv []string) error { return errors.New("boom") }
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0 (browser fallback)", code)
		}
		want := [][]string{{"browser", "https://cdn.example.com/v.mkv"}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Errorf("spawned = %q, want %q", *spawned, want)
		}
	})
	t.Run("browser fallback failure returns 1", func(t *testing.T) {
		stubPlayers(t, "linux", nil)
		openInBrowser = func(u string) error { return errors.New("no browser") }
		if code := dispatchPlayer(req); code != 1 {
			t.Fatalf("dispatchPlayer() = %d, want 1", code)
		}
	})
}

// The web choice and the no-player fallback must open the unarr web PLAYER
// page when the link carries one (web=), not dump the raw .mkv in a tab.
func TestDispatchPlayerWebTarget(t *testing.T) {
	req := playRequest{
		URL:    "https://agent.example.com/stream/abc.mkv",
		WebURL: "https://unarr.app/es/play/abc",
	}
	t.Run("player=web opens the web player page", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", map[string]string{"vlc": "/usr/bin/vlc"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "web")
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0", code)
		}
		want := [][]string{{"browser", "https://unarr.app/es/play/abc"}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Fatalf("spawned = %v, want %v", *spawned, want)
		}
	})
	t.Run("no player installed falls back to the web player page", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", nil)
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0", code)
		}
		want := [][]string{{"browser", "https://unarr.app/es/play/abc"}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Fatalf("spawned = %v, want %v", *spawned, want)
		}
	})
	t.Run("without web= it falls back to the stream url", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", nil)
		if code := dispatchPlayer(playRequest{URL: req.URL}); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0", code)
		}
		want := [][]string{{"browser", req.URL}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Fatalf("spawned = %v, want %v", *spawned, want)
		}
	})
}

func TestCelluloidArgv(t *testing.T) {
	got := celluloidArgv(player{kind: playerCelluloid, bin: "/usr/bin/celluloid"}, playRequest{
		URL:   "https://cdn.example.com/v.mkv",
		Start: 90,
		Title: "My Show",
		ALang: []string{"es", "en"},
		SLang: []string{"es"},
	})
	want := []string{
		"/usr/bin/celluloid",
		"--mpv-start=90",
		"--mpv-force-media-title=My Show",
		"--mpv-alang=es,en",
		"--mpv-slang=es",
		"--",
		"https://cdn.example.com/v.mkv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("celluloidArgv() = %v, want %v", got, want)
	}
}
