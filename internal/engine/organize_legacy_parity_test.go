package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// Differential guard for naming templates: with no naming configured, organize
// must file every download exactly where the pre-template code did — a single
// byte of difference splits a user's existing show or movie into two folders.
// legacyDest is the destination computation of organize() as it was before
// templates (v1.15.4), copied verbatim with its own sanitizer so a later change
// to sanitizePath can't make both sides drift together.

var legacyReplacer = strings.NewReplacer(
	"/", "-", "\\", "-", ":", " -", "?", "", "*", "", "\"", "", "<", "", ">", "", "|", "-",
)

func legacySanitize(name string) string {
	s := legacyReplacer.Replace(name)
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "Unknown"
	}
	return s
}

func legacyDest(result *Result, task *Task, cfg OrganizeConfig) (destDir, destFileName string) {
	ext := filepath.Ext(result.FileName)
	if ext == "" {
		ext = filepath.Ext(result.FilePath)
	}
	if task.ContentType == "show" && cfg.TVShowsDir != "" {
		showName := task.ContentTitle
		if showName == "" {
			showName = cleanTitle(task.Title)
		}
		destDir = filepath.Join(cfg.TVShowsDir, legacySanitize(showName))
		if task.Season != nil {
			destDir = filepath.Join(destDir, fmt.Sprintf("Season %02d", *task.Season))
			if task.Episode != nil {
				destFileName = fmt.Sprintf("%s - S%02dE%02d%s", legacySanitize(showName), *task.Season, *task.Episode, ext)
			}
		} else if season := detectSeason(result.FileName); season != "" {
			destDir = filepath.Join(destDir, fmt.Sprintf("Season %s", season))
		}
		return destDir, destFileName
	}
	movieName := task.ContentTitle
	if movieName == "" {
		movieName = cleanTitle(task.Title)
	}
	year := resolveYear(task)
	base := cfg.MoviesDir
	if task.CollectionName != "" {
		base = filepath.Join(base, legacySanitize(task.CollectionName))
	}
	if year != "" {
		name := fmt.Sprintf("%s (%s)", legacySanitize(movieName), year)
		return filepath.Join(base, name), name + ext
	}
	return filepath.Join(base, legacySanitize(movieName)), legacySanitize(movieName) + ext
}

// legacyPackEpisodeDest is v1.15.4's packEpisodeDest, verbatim.
func legacyPackEpisodeDest(src, showDir, showName string, fallbackSeason int) (dir, name string) {
	season, episode := library.ParseSeasonEpisode(filepath.Base(src))
	if season == 0 {
		season = fallbackSeason
	}
	if season <= 0 {
		return showDir, ""
	}
	dir = filepath.Join(showDir, fmt.Sprintf("Season %02d", season))
	if episode > 0 {
		name = fmt.Sprintf("%s - S%02dE%02d%s", legacySanitize(showName), season, episode, filepath.Ext(src))
	}
	return dir, name
}

func TestDefaultNamingPackMatchesLegacyLayout(t *testing.T) {
	for _, season := range []*int{nil, intPtr(2)} {
		root := t.TempDir()
		cfg := OrganizeConfig{Enabled: true, TVShowsDir: filepath.Join(root, "TV"), OutputDir: filepath.Join(root, "dl")}
		release := filepath.Join(cfg.OutputDir, "Odd.Show.COMPLETE")
		mustMkdir(t, release)
		files := []string{"Odd.Show.S01E01.mkv", "Odd.Show.S01E02.mkv", "Odd.Show.S02E01.mkv", "Odd.Show.S02E02.mkv"}
		for i, f := range files {
			mustWrite(t, filepath.Join(release, f), bytesPattern(episodeBytes+i))
		}
		task := &Task{ID: "p", ContentType: "show", ContentTitle: "Odd  Show: Redux", Season: season}
		showDir := filepath.Join(cfg.TVShowsDir, legacySanitize(task.ContentTitle))
		var want []string
		for _, f := range files {
			if s, _ := library.ParseSeasonEpisode(f); season != nil && s != *season {
				continue
			}
			dir, name := legacyPackEpisodeDest(f, showDir, task.ContentTitle, taskSeason(task))
			want = append(want, filepath.Join(dir, name))
		}
		final, err := organize(&Result{FilePath: release, FileName: filepath.Base(release), Method: MethodTorrent}, task, cfg)
		if err != nil {
			t.Fatal(err)
		}
		wantFinal := showDir
		if season != nil {
			wantFinal = filepath.Join(showDir, "Season 02")
		}
		if final != wantFinal {
			t.Errorf("season %v: final = %q, legacy %q", season, final, wantFinal)
		}
		for _, p := range want {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("season %v: legacy path missing: %v", season, err)
			}
		}
	}
}

func TestDefaultNamingMatchesLegacyLayout(t *testing.T) {
	long := strings.Repeat("Long Title ", 22) // 242 bytes: fits a name, must not be cut
	type tc struct {
		name, file string
		task       *Task
	}
	cases := []tc{
		{"movie", "Dune.2021.1080p.mkv", &Task{Title: "Dune.2021.1080p", ContentType: "movie", ContentTitle: "Dune", ContentYear: intPtr(2021)}},
		{"movie no year", "Primer.mkv", &Task{Title: "Primer", ContentType: "movie", ContentTitle: "Primer"}},
		{"movie year from title", "Heat.1995.mkv", &Task{Title: "Heat.1995.BluRay", ContentType: "movie", ContentTitle: "Heat"}},
		{"collection", "Knives.Out.2019.mkv", &Task{Title: "Knives.Out.2019", ContentType: "movie", ContentTitle: "Knives Out", CollectionName: "Knives Out: Collection", ContentYear: intPtr(2019)}},
		{"collection without type", "X.mkv", &Task{Title: "X", CollectionName: "Saga", ContentTitle: "X"}},
		{"colon and slash", "AC.mkv", &Task{Title: "AC", ContentType: "movie", ContentTitle: `AC/DC: Live? "1991" <Rem>|`, ContentYear: intPtr(1991)}},
		{"double spaces", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Two  Spaces   Here", ContentYear: intPtr(2001)}},
		{"leading dash", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "-Ism", ContentYear: intPtr(2001)}},
		{"trailing dot space", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Mr. ."}},
		{"literal empty parens", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Title ()", ContentYear: intPtr(2001)}},
		{"literal braces", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Set {} Go"}},
		{"long title", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: long}},
		{"unicode", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Amélie: Le Fabuleux Destin d'Amélie Poulain", ContentYear: intPtr(2001)}},
		{"empty titles", "movie.mkv", &Task{Title: "", ContentType: "movie"}},
		{"episode", "Show.S01E03.mkv", &Task{Title: "Show.S01E03", ContentType: "show", ContentTitle: "Frieren: Beyond Journey's End", Season: intPtr(1), Episode: intPtr(3)}},
		{"episode 4 digits", "OP.S22E1089.mkv", &Task{Title: "OP", ContentType: "show", ContentTitle: "One Piece", Season: intPtr(22), Episode: intPtr(1089)}},
		{"specials", "S.S00E01.mkv", &Task{Title: "S", ContentType: "show", ContentTitle: "Show", Season: intPtr(0), Episode: intPtr(1)}},
		{"season no episode", "Show.S02.mkv", &Task{Title: "Show.S02", ContentType: "show", ContentTitle: "Show", Season: intPtr(2)}},
		{"season from filename", "Show.S03E04.720p.mkv", &Task{Title: "Show", ContentType: "show", ContentTitle: "Show"}},
		{"alt episode form", "show.2x05.mkv", &Task{Title: "show", ContentType: "show", ContentTitle: "Show"}},
		{"no season at all", "show.mkv", &Task{Title: "show", ContentType: "show", ContentTitle: "Show"}},
		{"show title from release", "The.Bear.S01E01.mkv", &Task{Title: "The.Bear.S01E01.1080p", ContentType: "show", Season: intPtr(1), Episode: intPtr(1)}},
		{"show double spaces", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: "Odd  Show ", Season: intPtr(1), Episode: intPtr(1)}},
		// Edge titles the first review pass caught drifting.
		{"french colon", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: "Astérix : Le Domaine", Season: intPtr(1), Episode: intPtr(1)}},
		{"question mark", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Who ? What", ContentYear: intPtr(2001)}},
		{"dash title no year", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "-"}},
		{"parens title no year", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "()"}},
		{"dash title with year", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "-", ContentYear: intPtr(2000)}},
		{"collection sanitizes empty", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Knives Out", CollectionName: "???", ContentYear: intPtr(2019)}},
		{"collection dash", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Knives Out", CollectionName: "-", ContentYear: intPtr(2019)}},
		{"dots and spaces", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: "Foo . .", Season: intPtr(1), Episode: intPtr(1)}},
		{"249-byte title", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: strings.Repeat("b", 249)}},
		{"195-byte title with year", "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: strings.Repeat("c", 195), ContentYear: intPtr(2005)}},
		{"195-byte show episode", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: strings.Repeat("d", 195), Season: intPtr(1), Episode: intPtr(1)}},
		{"cjk show", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: strings.Repeat("進", 70), Season: intPtr(1), Episode: intPtr(1)}},
		{"show year ignored", "a.S01E01.mkv", &Task{Title: "a", ContentType: "show", ContentTitle: "Show", ContentYear: intPtr(1999), IMDbID: "tt1", Season: intPtr(1), Episode: intPtr(1), EpisodeTitle: "Pilot"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := OrganizeConfig{
				Enabled:    true,
				MoviesDir:  filepath.Join(root, "Movies"),
				TVShowsDir: filepath.Join(root, "TV"),
				OutputDir:  filepath.Join(root, "dl"),
			}
			mustMkdir(t, cfg.OutputDir)
			src := filepath.Join(cfg.OutputDir, c.file)
			mustWrite(t, src, []byte("x"))
			result := &Result{FilePath: src, FileName: c.file, Method: MethodTorrent}
			task := c.task

			dir, file := legacyDest(result, task, cfg)
			if file == "" {
				file = c.file
			}
			want := filepath.Join(dir, file)

			got, err := organize(result, task, cfg)
			if err != nil {
				t.Fatalf("organize: %v", err)
			}
			if got != want {
				t.Errorf("default layout drifted from the legacy one:\n got  %q\n want %q", got, want)
			}
			if _, err := os.Stat(got); err != nil {
				t.Errorf("file not at the returned path: %v", err)
			}
		})
	}
}
