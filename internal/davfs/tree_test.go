package davfs

import (
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// NOTE: sanitizeName and parseModTime are covered by tree_sanitize_test.go.
// This file covers the folder-naming fallbacks and the ModTime rollup, which
// that file does not.

// TestMovieFolderFallbacks exercises the degenerate-metadata fallbacks: an empty
// (or separator-only) title becomes "Unknown", and a missing year drops the
// "(year)" suffix.
func TestMovieFolderFallbacks(t *testing.T) {
	cases := []struct{ title, year, want string }{
		{"", "", "Unknown"},
		{"", "2020", "Unknown (2020)"},
		{"Cars", "", "Cars"},
		{"Cars", "2006", "Cars (2006)"},
		{"/", "2006", "Unknown (2006)"}, // sanitizes to "" → Unknown, year kept
	}
	for _, c := range cases {
		if got := movieFolder(library.LibraryItem{Title: c.title, Year: c.year}); got != c.want {
			t.Errorf("movieFolder(title=%q year=%q) = %q, want %q", c.title, c.year, got, c.want)
		}
	}
}

// TestShowFolderFallbacks: an empty or marker-only title yields "Unknown Show";
// otherwise the series part (episode marker stripped) is used.
func TestShowFolderFallbacks(t *testing.T) {
	cases := []struct{ title, want string }{
		{"", "Unknown Show"},
		{"S01E01", "Unknown Show"}, // marker-only → series part is empty
		{"1x05", "Unknown Show"},   // alt marker-only
		{"Breaking Bad S01E01", "Breaking Bad"},
	}
	for _, c := range cases {
		if got := showFolder(library.LibraryItem{Title: c.title}); got != c.want {
			t.Errorf("showFolder(title=%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

// TestShowTitleSeasonPack: a season pack packed as a single file parses to a
// Title carrying a trailing bare season token ("Show S01" / "Show Season 2"),
// which must be stripped so every season groups under one show folder instead
// of spawning a "Show S01" folder. The control cases guard against the stripper
// eating a legitimate title — an episode marker is peeled first (leaving the
// real series name, digits and all), and a title that merely ends in digits is
// left untouched.
func TestShowTitleSeasonPack(t *testing.T) {
	cases := []struct{ title, want string }{
		{"Show S01", "Show"},                      // season pack (S + digits)
		{"Show Name S1", "Show Name"},             // single-digit season
		{"Breaking Bad Season 2", "Breaking Bad"}, // "Season N" word form
		{"The 100 S02E05", "The 100"},             // control: episode marker only → trailing digits kept
		{"The 100", "The 100"},                    // control: title ending in digits, not a season token
		{"Money Heist", "Money Heist"},            // control: plain title untouched
	}
	for _, c := range cases {
		if got := showTitle(library.LibraryItem{Title: c.title}); got != c.want {
			t.Errorf("showTitle(title=%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

// TestSeasonFolder: season 0 or negative → "Specials"; otherwise zero-padded.
func TestSeasonFolder(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "Specials"},
		{-1, "Specials"},
		{1, "Season 01"},
		{9, "Season 09"},
		{12, "Season 12"},
		{123, "Season 123"},
	}
	for _, c := range cases {
		if got := seasonFolder(c.in); got != c.want {
			t.Errorf("seasonFolder(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCollisionSuffixDeterministic: two files sharing a virtual folder+leaf,
// distinguished only by real path, must get the SAME suffix assignment
// regardless of input order — otherwise a file's WebDAV URL/ETag flips between
// rescans. The lexicographically-first real path keeps the unsuffixed name.
func TestCollisionSuffixDeterministic(t *testing.T) {
	a := library.LibraryItem{FilePath: "/aaa/Movie.2020.mkv", FileName: "Movie.2020.mkv", Title: "Movie", Year: "2020"}
	b := library.LibraryItem{FilePath: "/zzz/Movie.2020.mkv", FileName: "Movie.2020.mkv", Title: "Movie", Year: "2020"}

	nameFor := func(items []library.LibraryItem) map[string]string {
		root := buildTree(items)
		folder := root.children[movieRoot].children["Movie (2020)"]
		if folder == nil {
			t.Fatal("Movie (2020) folder missing")
		}
		out := map[string]string{}
		for _, n := range folder.children {
			out[n.realPath] = n.name
		}
		return out
	}
	forward := nameFor([]library.LibraryItem{a, b})
	reverse := nameFor([]library.LibraryItem{b, a})

	if forward["/aaa/Movie.2020.mkv"] != reverse["/aaa/Movie.2020.mkv"] ||
		forward["/zzz/Movie.2020.mkv"] != reverse["/zzz/Movie.2020.mkv"] {
		t.Errorf("collision suffix not deterministic across input order: forward=%v reverse=%v", forward, reverse)
	}
	if forward["/aaa/Movie.2020.mkv"] != "Movie.2020.mkv" {
		t.Errorf("first-by-path should keep the base name, got %q", forward["/aaa/Movie.2020.mkv"])
	}
	if forward["/zzz/Movie.2020.mkv"] != "Movie.2020 (2).mkv" {
		t.Errorf("second-by-path should be suffixed, got %q", forward["/zzz/Movie.2020.mkv"])
	}
}

// TestModTimeRollup verifies finalize's ETag rollup: a directory's reported
// ModTime equals its newest descendant leaf's, and each leaf's info ModTime
// matches its source item. webdav keys ETag/Last-Modified on ModTime + Size.
func TestModTimeRollup(t *testing.T) {
	const (
		older = "2020-01-01T00:00:00Z"
		newer = "2023-07-15T10:30:00Z"
	)
	items := []library.LibraryItem{
		{FilePath: "/x/a.mkv", FileName: "a.mkv", Title: "Rollup", Year: "2020", ModTime: older},
		{FilePath: "/x/b.mkv", FileName: "b.mkv", Title: "Rollup", Year: "2020", ModTime: newer},
	}
	root := buildTree(items)
	wantOlder, _ := time.Parse(time.RFC3339, older)
	wantNewer, _ := time.Parse(time.RFC3339, newer)

	movies := root.children[movieRoot]
	if movies == nil {
		t.Fatal("Movies dir missing")
	}
	folder := movies.children["Rollup (2020)"]
	if folder == nil {
		t.Fatalf("movie folder missing; children=%v", movies.order)
	}

	// Directory ModTime rolls up to the newest descendant, all the way to root.
	if got := folder.info().ModTime(); !got.Equal(wantNewer) {
		t.Errorf("folder ModTime = %v, want newest leaf %v", got, wantNewer)
	}
	if got := movies.info().ModTime(); !got.Equal(wantNewer) {
		t.Errorf("Movies ModTime = %v, want %v", got, wantNewer)
	}
	if got := root.info().ModTime(); !got.Equal(wantNewer) {
		t.Errorf("root ModTime = %v, want %v", got, wantNewer)
	}
	// Each leaf's info ModTime matches its source item.
	if got := folder.children["a.mkv"].info().ModTime(); !got.Equal(wantOlder) {
		t.Errorf("leaf a.mkv ModTime = %v, want %v", got, wantOlder)
	}
	if got := folder.children["b.mkv"].info().ModTime(); !got.Equal(wantNewer) {
		t.Errorf("leaf b.mkv ModTime = %v, want %v", got, wantNewer)
	}
}
