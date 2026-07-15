package cmd

import (
	"reflect"
	"testing"
)

func TestDiscoveryHosts(t *testing.T) {
	tests := []struct {
		name     string
		apiURL   string
		mirrors  []string
		wantBase string
		wantRest []string
	}{
		{
			name:     "unarr default routes discovery to torrentclaw mirrors",
			apiURL:   "https://unarr.app",
			mirrors:  []string{"https://torrentclaw.to", "https://torrentclaw.com"},
			wantBase: "https://torrentclaw.to",
			wantRest: []string{"https://torrentclaw.com"},
		},
		{
			name:     "torrentclaw user keeps its own host as primary",
			apiURL:   "https://torrentclaw.com",
			mirrors:  []string{"https://torrentclaw.to"},
			wantBase: "https://torrentclaw.com",
			wantRest: []string{"https://torrentclaw.to"},
		},
		{
			name:     "unarr-only config with no mirrors falls back to built-in defaults",
			apiURL:   "https://unarr.app",
			mirrors:  nil,
			wantBase: "https://torrentclaw.to",
			wantRest: []string{"https://torrentclaw.com"},
		},
		{
			name:     "unarr subdomain is also excluded",
			apiURL:   "https://app.unarr.app",
			mirrors:  []string{"https://torrentclaw.com"},
			wantBase: "https://torrentclaw.com",
			wantRest: []string{},
		},
		{
			name:     "trailing slashes normalised and duplicates removed",
			apiURL:   "https://torrentclaw.com/",
			mirrors:  []string{"https://torrentclaw.com", " https://torrentclaw.to/ "},
			wantBase: "https://torrentclaw.com",
			wantRest: []string{"https://torrentclaw.to"},
		},
		{
			name:     "empty api_url uses the first torrentclaw mirror",
			apiURL:   "",
			mirrors:  []string{"https://torrentclaw.com"},
			wantBase: "https://torrentclaw.com",
			wantRest: []string{},
		},
		{
			name:     "everything empty falls back to defaults",
			apiURL:   "",
			mirrors:  nil,
			wantBase: "https://torrentclaw.to",
			wantRest: []string{"https://torrentclaw.com"},
		},
		{
			name:     "scheme-less unarr.app is still excluded",
			apiURL:   "unarr.app",
			mirrors:  []string{"https://torrentclaw.com"},
			wantBase: "https://torrentclaw.com",
			wantRest: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, rest := discoveryHosts(tt.apiURL, tt.mirrors)
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
			if len(rest) != len(tt.wantRest) || (len(rest) > 0 && !reflect.DeepEqual(rest, tt.wantRest)) {
				t.Errorf("rest = %#v, want %#v", rest, tt.wantRest)
			}
		})
	}
}

func TestIsUnarrBrandHost(t *testing.T) {
	cases := map[string]bool{
		"https://unarr.app":          true,
		"https://unarr.app/api/v1":   true,
		"https://app.unarr.app":      true,
		"unarr.app":                  true, // scheme-less hand-edit
		"UNARR.APP":                  true,
		"https://unarr.app.":         true, // rooted FQDN
		"https://unarr.app:443":      true,
		"https://torrentclaw.com":    false,
		"https://torrentclaw.to":     false,
		"torrentclaw.com":            false,
		"https://unarr.app.evil.com": false,
		"":                           false,
		"   ":                        false,
	}
	for host, want := range cases {
		if got := isUnarrBrandHost(host); got != want {
			t.Errorf("isUnarrBrandHost(%q) = %v, want %v", host, got, want)
		}
	}
}
