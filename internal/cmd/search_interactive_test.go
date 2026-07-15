package cmd

import (
	"strings"
	"testing"

	tc "github.com/torrentclaw/go-client"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestMagnetOf(t *testing.T) {
	const hash40 = "0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name   string
		in     tc.TorrentInfo
		wantOK bool
		want   string
	}{
		{
			name:   "full magnet URL is used verbatim",
			in:     tc.TorrentInfo{MagnetURL: strPtr("magnet:?xt=urn:btih:deadbeef&dn=x"), InfoHash: hash40},
			wantOK: true,
			want:   "magnet:?xt=urn:btih:deadbeef&dn=x",
		},
		{
			name:   "synthesizes a magnet from a 40-char info hash",
			in:     tc.TorrentInfo{InfoHash: hash40},
			wantOK: true,
			want:   "magnet:?xt=urn:btih:" + hash40,
		},
		{
			name:   "empty info hash and no magnet is not playable",
			in:     tc.TorrentInfo{InfoHash: ""},
			wantOK: false,
		},
		{
			name:   "short/gated info hash is not playable (no hashless magnet)",
			in:     tc.TorrentInfo{InfoHash: "abc"},
			wantOK: false,
		},
		{
			name:   "empty magnet string falls back to the info hash",
			in:     tc.TorrentInfo{MagnetURL: strPtr(""), InfoHash: hash40},
			wantOK: true,
			want:   "magnet:?xt=urn:btih:" + hash40,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := magnetOf(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("magnet = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBestTorrent(t *testing.T) {
	r := tc.SearchResult{Torrents: []tc.TorrentInfo{
		{InfoHash: "a", QualityScore: intPtr(70), Seeders: 500},
		{InfoHash: "b", QualityScore: intPtr(90), Seeders: 3},   // highest score wins
		{InfoHash: "c", QualityScore: intPtr(90), Seeders: 999}, // ties broken by seeders
		{InfoHash: "d", QualityScore: nil, Seeders: 10000},      // missing score = 0
	}}
	if got := bestTorrent(r); got.InfoHash != "c" {
		t.Fatalf("bestTorrent picked %q, want c", got.InfoHash)
	}
}

func TestTorrentLabel(t *testing.T) {
	label := torrentLabel(tc.TorrentInfo{
		Quality:      strPtr("1080p"),
		Codec:        strPtr("x265"),
		Seeders:      42,
		Source:       "bitmagnet",
		QualityScore: intPtr(77),
		Languages:    []string{"en"},
	})
	for _, want := range []string{"1080p", "42 seeds", "bitmagnet", "x265", "score 77"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q missing %q", label, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},                // under limit → unchanged
		{"exactly-ten", 11, "exactly-ten"},    // exact length → unchanged
		{"truncate me please", 8, "truncat…"}, // cut → ellipsis, len==max runes
		{"abc", 1, "a"},                       // max<=1 → raw slice, no ellipsis
		{"añoño", 3, "añ…"},                   // multibyte runes counted, not bytes
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.max); got != tt.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestFirstStreamable(t *testing.T) {
	const hash40 = "0123456789abcdef0123456789abcdef01234567"
	results := []tc.SearchResult{
		{Torrents: nil}, // no torrents → skip
		{Torrents: []tc.TorrentInfo{{InfoHash: "gated"}}},                          // gated (no 40-char hash) → skip
		{Torrents: []tc.TorrentInfo{{InfoHash: hash40, QualityScore: intPtr(50)}}}, // playable → pick
	}
	got, ok := firstStreamable(results)
	if !ok || got.InfoHash != hash40 {
		t.Fatalf("firstStreamable = (%+v, %v), want the %s release", got.InfoHash, ok, hash40)
	}

	if _, ok := firstStreamable([]tc.SearchResult{{Torrents: []tc.TorrentInfo{{InfoHash: "x"}}}}); ok {
		t.Error("firstStreamable should return ok=false when every release is gated")
	}
	if _, ok := firstStreamable(nil); ok {
		t.Error("firstStreamable should return ok=false on no results")
	}
}

func TestResultLabel(t *testing.T) {
	label := resultLabel(tc.SearchResult{
		Title:      "The Bear",
		Year:       intPtr(2022),
		RatingIMDb: strPtr("8.6"),
		Torrents:   []tc.TorrentInfo{{}, {}, {}},
	})
	for _, want := range []string{"The Bear", "2022", "8.6", "3 releases"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q missing %q", label, want)
		}
	}
}
