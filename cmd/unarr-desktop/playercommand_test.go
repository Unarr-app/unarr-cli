package main

import (
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "   ", nil},
		{"plain", "mpv --fs {url}", []string{"mpv", "--fs", "{url}"}},
		{"collapses runs of whitespace", "mpv   \t --fs", []string{"mpv", "--fs"}},
		{
			name: "double quotes group a token",
			in:   `"/opt/my player/mpv" --title="My Show" {url}`,
			want: []string{"/opt/my player/mpv", "--title=My Show", "{url}"},
		},
		{
			name: "single quotes group a token and keep backslashes",
			in:   `mpv --title='a\b' {url}`,
			want: []string{"mpv", `--title=a\b`, "{url}"},
		},
		{"escaped space stays in the token", `mpv /media/My\ Films`, []string{"mpv", "/media/My Films"}},
		{"escaped quote is literal", `mpv --title=\"x\"`, []string{"mpv", `--title="x"`}},
		{"empty quoted token survives", `mpv "" {url}`, []string{"mpv", "", "{url}"}},
		{
			// The whole point of tokenizing ourselves: shell metacharacters are
			// ordinary characters here. Nothing is executed, expanded or
			// redirected — they just end up inside an argv entry.
			name: "shell metacharacters are literal, not operators",
			in:   `mpv --title=a;rm\ -rf\ / {url} && echo hi | tee /tmp/x`,
			want: []string{"mpv", "--title=a;rm -rf /", "{url}", "&&", "echo", "hi", "|", "tee", "/tmp/x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitCommand(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitCommand(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpandPlayerCommand(t *testing.T) {
	full := playRequest{
		URL:    "https://cdn.example.com/v.mkv",
		WebURL: "https://unarr.app/stream/play?id=abc",
		Start:  90,
		Title:  "My Show",
		ALang:  []string{"es", "en"},
		SLang:  []string{"es"},
	}
	minimal := playRequest{URL: "https://cdn.example.com/v.mkv"}

	tests := []struct {
		name string
		tmpl string
		req  playRequest
		want []string
	}{
		{
			name: "every placeholder substituted",
			tmpl: "smplayer -media-title {title} -start {start} -alang {alang} -slang {slang} {url}",
			req:  full,
			want: []string{
				"smplayer", "-media-title", "My Show", "-start", "90",
				"-alang", "es,en", "-slang", "es", "https://cdn.example.com/v.mkv",
			},
		},
		{
			// A flag and its value live in one token on purpose: dropping the
			// token drops both, so no player is handed a dangling `--start`.
			name: "tokens with no value are dropped whole",
			tmpl: "mpv --start={start} --force-media-title={title} --alang={alang} -- {url}",
			req:  minimal,
			want: []string{"mpv", "--", "https://cdn.example.com/v.mkv"},
		},
		{
			name: "url is appended when the template never mentions it",
			tmpl: "flatpak run org.videolan.VLC",
			req:  minimal,
			want: []string{"flatpak", "run", "org.videolan.VLC", "https://cdn.example.com/v.mkv"},
		},
		{
			name: "{web} targets the web player page instead of the raw stream",
			tmpl: "firefox {web}",
			req:  full,
			want: []string{"firefox", "https://unarr.app/stream/play?id=abc"},
		},
		{
			// {web} degrades to the stream URL rather than dropping the token:
			// an old web build sends no web=, and the browser can still play it.
			name: "{web} falls back to the stream url",
			tmpl: "firefox {web}",
			req:  minimal,
			want: []string{"firefox", "https://cdn.example.com/v.mkv"},
		},
		{
			name: "a placeholder embedded in a larger token stays in that token",
			tmpl: "player --opts=start:{start},title:{title} {url}",
			req:  full,
			want: []string{
				"player", "--opts=start:90,title:My Show", "https://cdn.example.com/v.mkv",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandPlayerCommand(splitCommand(tt.tmpl), tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expandPlayerCommand() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// A stream URL that looks like a flag must stay one argv entry — it can never
// become a switch of its own, whatever the template does with it.
func TestExpandPlayerCommandKeepsHostileURLInOneToken(t *testing.T) {
	req := playRequest{URL: "--script=http://evil.example/x.lua"}
	got := expandPlayerCommand(splitCommand("mpv {url}"), req)
	want := []string{"mpv", "--script=http://evil.example/x.lua"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandPlayerCommand() = %#v, want %#v", got, want)
	}
}

func TestSelectPlayerCustomCommand(t *testing.T) {
	t.Run("custom command wins over every other layer", func(t *testing.T) {
		spawned := stubPlayers(t, "linux", map[string]string{"mpv": "/usr/bin/mpv"})
		t.Setenv("UNARR_DESKTOP_PLAYER", "mpv")
		t.Setenv("UNARR_DESKTOP_PLAYER_COMMAND", "flatpak run io.github.celluloid_player.Celluloid --mpv-start={start} -- {url}")

		req := playRequest{URL: "https://cdn.example.com/v.mkv", Start: 42}
		if code := dispatchPlayer(req); code != 0 {
			t.Fatalf("dispatchPlayer() = %d, want 0", code)
		}
		want := [][]string{{
			"flatpak", "run", "io.github.celluloid_player.Celluloid",
			"--mpv-start=42", "--", "https://cdn.example.com/v.mkv",
		}}
		if !reflect.DeepEqual(*spawned, want) {
			t.Fatalf("spawned = %#v, want %#v", *spawned, want)
		}
	})
}

// TestExpandPlayerCommandSubFile pins the documented {subfile} limitation: a
// placeholder substitutes inside ONE token (the property that keeps a template
// from splitting into extra arguments), so a repeatable flag cannot be emitted
// N times — {subfile} therefore carries the FIRST subtitle only, and the token
// disappears entirely when there is none.
func TestExpandPlayerCommandSubFile(t *testing.T) {
	const (
		subA = "https://unarr.app/api/internal/subtitles/proxy?token=a"
		subB = "https://unarr.app/api/internal/subtitles/proxy?token=b"
	)
	tmpl := splitCommand("myplayer --sub-file={subfile} -- {url}")

	t.Run("expands to the first sub file only", func(t *testing.T) {
		req := playRequest{URL: "https://x.example/v.mkv", SubFiles: []string{subA, subB}}
		got := expandPlayerCommand(tmpl, req)
		want := []string{"myplayer", "--sub-file=" + subA, "--", "https://x.example/v.mkv"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("expandPlayerCommand() = %q, want %q", got, want)
		}
	})

	t.Run("drops the whole token when there is no sub file", func(t *testing.T) {
		req := playRequest{URL: "https://x.example/v.mkv"}
		got := expandPlayerCommand(tmpl, req)
		want := []string{"myplayer", "--", "https://x.example/v.mkv"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("expandPlayerCommand() = %q, want %q", got, want)
		}
	})

	t.Run("a sub file url stays inside one token", func(t *testing.T) {
		// The parser only lets http(s) through, but the one-token guarantee must
		// hold on its own: a value with spaces/quotes can never become argv.
		req := playRequest{URL: "https://x.example/v.mkv", SubFiles: []string{"https://x.example/a b.vtt"}}
		got := expandPlayerCommand(splitCommand("p {subfile} {url}"), req)
		want := []string{"p", "https://x.example/a b.vtt", "https://x.example/v.mkv"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("expandPlayerCommand() = %q, want %q", got, want)
		}
	})
}
