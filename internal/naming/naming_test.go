package naming

import (
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

func ip(n int) *int { return &n }

func join(r Result) string {
	if !r.FileOK {
		return path.Join(append(r.Dirs, "<original>")...)
	}
	return path.Join(append(r.Dirs, r.File)...)
}

func mustResolve(t *testing.T, preset string) Scheme {
	t.Helper()
	s, err := Resolve(preset, "", "")
	if err != nil {
		t.Fatalf("Resolve(%q): %v", preset, err)
	}
	return s
}

var (
	onePieceAnime = Vars{Title: "One Piece", Year: "1999", ImdbID: "tt0388629", TmdbID: "37854", TvdbID: "81797",
		EpisodeTitle: "I'm Luffy! The Man Who Will Become the Pirate King!", Season: ip(1), Episode: ip(1)}
	onePieceLive  = Vars{Title: "ONE PIECE", Year: "2023", ImdbID: "tt11737520", Season: ip(1), Episode: ip(1), EpisodeTitle: "ROMANCE DAWN"}
	onePieceMovie = Vars{Title: "One Piece: The Movie", Year: "2000", ImdbID: "tt0814243", TmdbID: "19576"}
)

func TestPlexPresetSeparatesHomonyms(t *testing.T) {
	s := mustResolve(t, "plex")
	cases := []struct {
		tpl  *Template
		v    Vars
		want string
	}{
		{s.Series, onePieceAnime, "One Piece (1999) {imdb-tt0388629}/Season 01/One Piece (1999) - S01E01 - I'm Luffy! The Man Who Will Become the Pirate King!"},
		{s.Series, onePieceLive, "ONE PIECE (2023) {imdb-tt11737520}/Season 01/ONE PIECE (2023) - S01E01 - ROMANCE DAWN"},
		{s.Movie, onePieceMovie, "One Piece - The Movie (2000) {imdb-tt0814243}/One Piece - The Movie (2000) {imdb-tt0814243}"},
		// No IMDb id / no year: the optional segments vanish, nothing empty is left.
		{s.Movie, Vars{Title: "One Piece", Year: "2021"}, "One Piece (2021)/One Piece (2021)"},
		{s.Movie, Vars{Title: "Untitled"}, "Untitled/Untitled"},
		// No episode title: the " - {t}" segment drops.
		{s.Series, Vars{Title: "Show", Year: "2020", Season: ip(2), Episode: ip(3)}, "Show (2020)/Season 02/Show (2020) - S02E03"},
		// 4-digit absolute episode.
		{s.Series, Vars{Title: "One Piece", Year: "1999", ImdbID: "tt0388629", Season: ip(22), Episode: ip(1071)}, "One Piece (1999) {imdb-tt0388629}/Season 22/One Piece (1999) - S22E1071"},
	}
	for _, c := range cases {
		if got := join(c.tpl.Render(c.v)); got != c.want {
			t.Errorf("Render(%s)\n got  %q\n want %q", c.tpl, got, c.want)
		}
	}
}

func TestJellyfinPreset(t *testing.T) {
	s := mustResolve(t, "jellyfin")
	got := join(s.Movie.Render(onePieceMovie))
	want := "One Piece - The Movie (2000) [imdbid-tt0814243]/One Piece - The Movie (2000) [imdbid-tt0814243]"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if a := mustResolve(t, "infuse"); a.Series.String() != mustResolve(t, "plex").Series.String() {
		t.Error("infuse should alias plex")
	}
}

// The default preset is the layout organize always produced; the engine's
// golden tests pin the full paths, this pins the template semantics.
func TestDefaultPresetMatchesLegacyLayout(t *testing.T) {
	s := Default()
	cases := []struct {
		tpl  *Template
		v    Vars
		want string
	}{
		{s.Movie, Vars{Title: "Oppenheimer", Year: "2023", ImdbID: "tt15398776"}, "Oppenheimer (2023)/Oppenheimer (2023)"},
		{s.Movie, Vars{Title: "Knives Out", Year: "2019", Collection: "Knives Out Collection"}, "Knives Out Collection/Knives Out (2019)/Knives Out (2019)"},
		{s.Movie, Vars{Title: "No Year"}, "No Year/No Year"},
		{s.Series, Vars{Title: "Frieren", Season: ip(1), Episode: ip(3)}, "Frieren/Season 01/Frieren - S01E03"},
		// Season known, episode unknown: season folder, original file name.
		{s.Series, Vars{Title: "Frieren", Season: ip(1)}, "Frieren/Season 01/<original>"},
		// Nothing known: the bare "Season" folder is skipped, not created.
		{s.Series, Vars{Title: "Frieren"}, "Frieren/<original>"},
		{s.Series, Vars{Title: "Show", Season: ip(0), Episode: ip(1)}, "Show/Season 00/Show - S00E01"},
	}
	for _, c := range cases {
		if got := join(c.tpl.Render(c.v)); got != c.want {
			t.Errorf("Render(%s, %+v)\n got  %q\n want %q", c.tpl, c.v, got, c.want)
		}
	}
}

func TestTokensAndModifiers(t *testing.T) {
	v := Vars{Title: "Show", Year: "2001", TmdbID: "12", TvdbID: "34", VideoFormat: "1080p", Season: ip(1), Episode: ip(7)}
	cases := map[string]string{
		"{n} {tmdbid} {tvdbid} {vf}/x {e}": "Show 12 34 1080p/x 7",
		"{n}/{n} {s}x{e.pad(4)}":           "Show/Show 1x0007",
		"{n}/{n} {sxe} S{s.pad(3)}E{e00}":  "Show/Show 1x07 S001E07",
		"{n} {tmdb-{tmdbid}}/{n} {s00e00}": "Show {tmdb-12}/Show S01E07",
		"{n} ({y})/{n} - {s00e00}":         "Show (2001)/Show - S01E07",
		"{n} ({imdbid})/{n} - {s00e00}":    "Show/Show - S01E07", // empty "()" tidied
	}
	for src, want := range cases {
		tpl, err := Parse(src, true)
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		if got := join(tpl.Render(v)); got != want {
			t.Errorf("Render(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestSanitizesValuesNotLiterals(t *testing.T) {
	tpl, _ := Parse("{n}/{n}", false)
	r := tpl.Render(Vars{Title: `AC/DC: Live? "1991" <Remaster>|.`})
	if strings.ContainsAny(r.Dirs[0], `/\:?"<>|`) || strings.HasSuffix(r.File, ".") {
		t.Errorf("unsafe name rendered: %+v", r)
	}
	if r.Dirs[0] != "AC-DC - Live 1991 Remaster-" {
		t.Errorf("got %q", r.Dirs[0])
	}
}

func TestRequiredFileTokenMissingKeepsOriginalName(t *testing.T) {
	tpl, _ := Parse("{n}/{n} ({y}) {imdbid}", false)
	r := tpl.Render(Vars{Title: "X", Year: "2000"})
	if r.FileOK {
		t.Errorf("file with a missing required token must report !FileOK, got %+v", r)
	}
	if len(r.Dirs) != 1 || r.Dirs[0] != "X" {
		t.Errorf("folders still render: %+v", r)
	}
}

// Only names the filesystem would reject (> 255 bytes) are cut, on a rune
// boundary; the file name is bounded together with its extension, from the
// end, so "Show - S01E01" survives a huge episode title.
func TestComponentLengthBounded(t *testing.T) {
	tpl, _ := Parse("{n}/{n} - {s00e00} - {t}", true)
	r := tpl.Render(Vars{Title: strings.Repeat("ñ", 300), EpisodeTitle: strings.Repeat("話", 200), Season: ip(1), Episode: ip(1)})
	if len(r.Dirs[0]) > maxComponentBytes || !utf8.ValidString(r.Dirs[0]) {
		t.Errorf("folder not bounded on a rune boundary: %d bytes", len(r.Dirs[0]))
	}
	name := FitFileName(r.File, ".mkv")
	if len(name) > maxComponentBytes || !utf8.ValidString(name) || !strings.HasSuffix(name, ".mkv") {
		t.Errorf("file name not bounded: %d bytes %q", len(name), name)
	}
	ok := strings.Repeat("a", 251)
	if FitFileName(ok, ".mkv") != ok+".mkv" {
		t.Error("a 255-byte name must be left untouched")
	}
	short, _ := Parse("{n}/{n} - {s00e00} - {t}", true)
	r = short.Render(Vars{Title: "Show", EpisodeTitle: strings.Repeat("x", 400), Season: ip(1), Episode: ip(2)})
	if name := FitFileName(r.File, ".mkv"); !strings.HasPrefix(name, "Show - S01E02 - x") {
		t.Errorf("episode code lost: %q", name[:20])
	}
}

func TestEpisodeTokenMustBeRequired(t *testing.T) {
	if _, err := Parse("{n}/{n}< - {s00e00}>", true); err == nil {
		t.Error("an episode token only inside <…> must be rejected")
	}
	if _, err := Parse("{n}/<{t} >{n} {s00e00}", true); err != nil {
		t.Errorf("a required episode token next to an optional one is fine: %v", err)
	}
}

// Marker bytes inside a value can't fake a missing/present token.
func TestValuesCannotForgeMarkers(t *testing.T) {
	tpl, _ := Parse("{n}/{n}", false)
	r := tpl.Render(Vars{Title: "Bad\x00Name\x01"})
	if !r.FileOK || r.File != "BadName" || r.Dirs[0] != "BadName" {
		t.Errorf("got %+v", r)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []struct {
		src    string
		series bool
		want   string
	}{
		{"", false, "empty"},
		{"/abs/{n}", false, "start or end"},
		{"{n}/", false, "start or end"},
		{"{n}/../{n}", false, `".."`},
		{"{n}//{n}", false, "empty folder"},
		{"{n}/{imdb}", false, "did you mean {imdbid}"},
		{"{n}/{foo}", false, "unknown token {foo}"},
		{"{n}/{n.pad(2)}", false, "only apply to {s} and {e}"},
		{"{n}/{e.pad(10)}", false, ".pad(N), N from 1 to 9"},
		{"{n}/{e.upper}", false, ".pad(N)"},
		{"{N}/{n}", false, "tokens are lowercase: {n}"},
		{"{n}/{Title}", false, "use {n}"},
		{"{n}/{IMDB}", false, "did you mean {imdbid}"},
		{"{n}/<{y}", false, "without closing"},
		{"{n}/{y}>", false, "without '<'"},
		{"{n}/<<{y}>>", false, "nested"},
		{"{n}: {y}/{n}", false, "invalid in file names"},
		{"{n}/static", false, "at least one token"},
		{"{n}/Season {s00}/{n} - {t}", true, "series file name must contain"},
	}
	for _, b := range bad {
		_, err := Parse(b.src, b.series)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("Parse(%q) error = %v, want containing %q", b.src, err, b.want)
		}
	}
}

func TestResolveOverridesAndErrors(t *testing.T) {
	s, err := Resolve("plex", "{n} [{tmdbid}]/{n}", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := join(s.Movie.Render(onePieceMovie)); got != "One Piece - The Movie [19576]/One Piece - The Movie" {
		t.Errorf("override not applied: %q", got)
	}
	if s.Series.String() != Presets["plex"].Series {
		t.Error("series should keep the preset template")
	}
	if _, err := Resolve("kodi", "", ""); err == nil || !strings.Contains(err.Error(), "jellyfin") {
		t.Errorf("unknown preset should list the valid ones, got %v", err)
	}
	if _, err := Resolve("", "", "{n}/{n}"); err == nil || !strings.Contains(err.Error(), "series_format") {
		t.Errorf("invalid series override should be reported, got %v", err)
	}
}

// Every documented token renders, and every rendered token is documented.
func TestTokensDocumentedAndRendered(t *testing.T) {
	for name := range Tokens {
		if _, ok := tokenValues[name]; !ok {
			t.Errorf("token {%s} is documented but never rendered", name)
		}
	}
	for name := range tokenValues {
		if _, ok := Tokens[name]; !ok {
			t.Errorf("token {%s} renders but is not documented in Tokens", name)
		}
	}
}

func TestBuiltInPresetsParse(t *testing.T) {
	for _, name := range PresetNames() {
		mustResolve(t, name)
	}
}
