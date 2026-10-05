package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/naming"
)

func runOrganizePreview(t *testing.T, cfg config.Config, args ...string) (string, error) {
	t.Helper()
	withConfig(t, cfg)
	cmd := newOrganizePreviewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestOrganizePreviewConfiguredPreset(t *testing.T) {
	cfg := config.Default()
	cfg.Organize.MoviesDir = "/lib/Movies"
	cfg.Organize.TVShowsDir = "/lib/TV"
	cfg.Organize.Naming = "plex"
	out, err := runOrganizePreview(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		filepath.Join("/lib/TV", "One Piece (1999) {imdb-tt0388629}", "Season 01", "One Piece (1999) - S01E01 - I'm Luffy! The Man Who Will Become the Pirate King!.mkv"),
		filepath.Join("/lib/TV", "ONE PIECE (2023) {imdb-tt11737520}", "Season 01"),
		filepath.Join("/lib/Movies", "One Piece - The Movie (2000) {imdb-tt0814243}", "One Piece - The Movie (2000) {imdb-tt0814243}.mkv"),
		filepath.Join("/lib/Movies", "Some Film (2021)", "Some Film (2021).mkv"),
		naming.OriginalName,
		"never moved",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
}

func TestOrganizePreviewFlagsOverrideConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Organize.Naming = "plex"
	out, err := runOrganizePreview(t, cfg, "--naming", "default",
		"--series-format", "{n}< [tvdbid-{tvdbid}]>/Season {s00}/{n} {sxe}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, filepath.Join("One Piece [tvdbid-81797]", "Season 01", "One Piece 1x01.mkv")) {
		t.Errorf("series override not previewed:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join("One Piece - The Movie (2000)", "One Piece - The Movie (2000).mkv")) {
		t.Errorf("--naming default should reset the movie layout:\n%s", out)
	}
}

func TestOrganizePreviewRejectsBadTemplate(t *testing.T) {
	_, err := runOrganizePreview(t, config.Default(), "--movie-format", "{n}/../{n}")
	if err == nil || !strings.Contains(err.Error(), "movie_format") {
		t.Errorf("want a movie_format error, got %v", err)
	}
}

func TestOrganizeNamingFallsBackOnInvalidConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Organize.Naming = "kodi"
	if got := organizeNaming(cfg); got.Series.String() != naming.Default().Series.String() {
		t.Errorf("invalid config should fall back to the default layout, got %s", got.Series)
	}
	cfg.Organize.Naming = "jellyfin"
	if got := organizeNaming(cfg); got.Movie.String() != naming.Presets["jellyfin"].Movie {
		t.Errorf("valid preset not used: %s", got.Movie)
	}
}
