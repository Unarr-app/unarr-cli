package mediainfo

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func findTrack(tracks []SubtitleTrack, base string) *SubtitleTrack {
	for i := range tracks {
		if filepath.Base(tracks[i].Path) == base {
			return &tracks[i]
		}
	}
	return nil
}

func TestDiscoverSidecarSubtitles_Siblings(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Witch.Hat.Atelier.S01E10.mkv")
	writeFile(t, video, "x")
	writeFile(t, filepath.Join(dir, "Witch.Hat.Atelier.S01E10.srt"), "1\n00:00:01,000 --> 00:00:02,000\nhi\n")
	writeFile(t, filepath.Join(dir, "Witch.Hat.Atelier.S01E10.es.ass"), "[Script Info]")
	writeFile(t, filepath.Join(dir, "Witch.Hat.Atelier.S01E10.en.forced.srt"), "x")
	// Unrelated file with a different base must NOT be matched as a sibling.
	writeFile(t, filepath.Join(dir, "Other.Movie.srt"), "x")

	tracks := DiscoverSidecarSubtitles(video)
	if len(tracks) != 3 {
		t.Fatalf("want 3 sibling tracks, got %d: %+v", len(tracks), tracks)
	}
	for _, tr := range tracks {
		if !tr.External || tr.Path == "" {
			t.Errorf("track not marked external w/ path: %+v", tr)
		}
	}
	if es := findTrack(tracks, "Witch.Hat.Atelier.S01E10.es.ass"); es == nil || es.Lang != "es" || es.Codec != "ass" {
		t.Errorf("es.ass mis-parsed: %+v", es)
	}
	if fr := findTrack(tracks, "Witch.Hat.Atelier.S01E10.en.forced.srt"); fr == nil || fr.Lang != "en" || !fr.Forced {
		t.Errorf("forced track mis-parsed: %+v", fr)
	}
}

func TestDiscoverSidecarSubtitles_SubsFolder(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Movie.2024.1080p.mkv")
	writeFile(t, video, "x")
	subs := filepath.Join(dir, "Subs")
	if err := os.Mkdir(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(subs, "2_English.srt"), "x")
	writeFile(t, filepath.Join(subs, "spa.ass"), "x")

	tracks := DiscoverSidecarSubtitles(video)
	if len(tracks) != 2 {
		t.Fatalf("want 2 Subs/ tracks, got %d: %+v", len(tracks), tracks)
	}
	if en := findTrack(tracks, "2_English.srt"); en == nil || en.Lang != "en" {
		t.Errorf("English mis-parsed: %+v", en)
	}
	if es := findTrack(tracks, "spa.ass"); es == nil || es.Lang != "es" {
		t.Errorf("spa mis-parsed: %+v", es)
	}
}

func TestParseSidecarName_ReleaseAliases(t *testing.T) {
	cases := []struct {
		name, ext, prefix, wantLang string
	}{
		{"[DMG] Orange [01].chs.ass", ".ass", "", "zh"}, // Chinese Simplified fansub code → GBK
		{"Show.cht.srt", ".srt", "Show", "zh-Hant"},     // Chinese Traditional → Big5
		{"Movie.big5.srt", ".srt", "Movie", "zh-Hant"},  // Traditional via codepage token
		{"Movie.lat.srt", ".srt", "Movie", "es"},        // Latin-American Spanish
		{"Movie.latino.srt", ".srt", "Movie", "es"},     //
		{"Pelicula.esp.srt", ".srt", "Pelicula", "es"},  //
		{"Anime.VOSTFR.ass", ".ass", "Anime", "fr"},     // French fansub
		{"X.kan.srt", ".srt", "X", "kn"},                // Kannada via langNormalize add
		{"X.mal.srt", ".srt", "X", "ml"},                // Malayalam
	}
	for _, c := range cases {
		lang, _, _ := parseSidecarName(c.name, c.ext, c.prefix)
		if lang != c.wantLang {
			t.Errorf("%s: got lang %q, want %q", c.name, lang, c.wantLang)
		}
	}
}

func TestDiscoverSidecarSubtitles_VobSubSkipped(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Film.mkv")
	writeFile(t, video, "x")
	writeFile(t, filepath.Join(dir, "Film.idx"), "x")
	writeFile(t, filepath.Join(dir, "Film.sub"), "x") // VobSub bitmap → skip
	tracks := DiscoverSidecarSubtitles(video)
	if len(tracks) != 0 {
		t.Fatalf("VobSub .sub+.idx must be skipped, got %d: %+v", len(tracks), tracks)
	}
}

func TestDiscoverSidecarSubtitles_RemoteURLNoop(t *testing.T) {
	if tracks := DiscoverSidecarSubtitles("https://example.com/movie.mkv"); tracks != nil {
		t.Fatalf("remote URL must yield no sidecars, got %+v", tracks)
	}
}
