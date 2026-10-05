package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/Unarr-app/unarr-cli/internal/naming"
)

func plexCfg(t *testing.T, root string) OrganizeConfig {
	t.Helper()
	scheme, err := naming.Resolve("plex", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return OrganizeConfig{
		Enabled:    true,
		MoviesDir:  filepath.Join(root, "Movies"),
		TVShowsDir: filepath.Join(root, "TV"),
		OutputDir:  filepath.Join(root, "downloads"),
		Naming:     scheme,
	}
}

func organizeOne(t *testing.T, cfg OrganizeConfig, fileName string, task *Task) string {
	t.Helper()
	mustMkdir(t, cfg.OutputDir)
	src := filepath.Join(cfg.OutputDir, fileName)
	mustWrite(t, src, bytesPattern(episodeBytes))
	got, err := organize(&Result{FilePath: src, FileName: fileName, Method: MethodTorrent}, task, cfg)
	if err != nil {
		t.Fatalf("organize: %v", err)
	}
	return got
}

// Chase's report: the One Piece anime (1999), the live action (2023) and the
// 2000 movie must land in distinct, id-tagged folders Infuse/Plex can't confuse.
func TestOrganizePlexNamingSeparatesOnePieces(t *testing.T) {
	root := t.TempDir()
	cfg := plexCfg(t, root)

	anime := organizeOne(t, cfg, "One.Piece.S01E01.1080p.WEB.mkv", &Task{
		Title: "One.Piece.S01E01.1080p.WEB", ContentType: "show", ContentTitle: "One Piece",
		ContentYear: intPtr(1999), IMDbID: "tt0388629", Season: intPtr(1), Episode: intPtr(1),
		EpisodeTitle: "I'm Luffy! The Man Who Will Become the Pirate King!",
	})
	want := filepath.Join(cfg.TVShowsDir, "One Piece (1999) {imdb-tt0388629}", "Season 01",
		"One Piece (1999) - S01E01 - I'm Luffy! The Man Who Will Become the Pirate King!.mkv")
	if anime != want {
		t.Errorf("anime episode:\n got  %q\n want %q", anime, want)
	}

	live := organizeOne(t, cfg, "ONE.PIECE.2023.S01E01.2160p.NF.mkv", &Task{
		Title: "ONE.PIECE.2023.S01E01.2160p.NF", ContentType: "show", ContentTitle: "ONE PIECE",
		ContentYear: intPtr(2023), IMDbID: "tt11737520", Season: intPtr(1), Episode: intPtr(1),
	})
	if filepath.Dir(filepath.Dir(live)) == filepath.Dir(filepath.Dir(anime)) {
		t.Errorf("live action shares the anime's show folder: %q", live)
	}

	movie := organizeOne(t, cfg, "One.Piece.The.Movie.2000.1080p.mkv", &Task{
		Title: "One.Piece.The.Movie.2000.1080p", ContentType: "movie", ContentTitle: "One Piece: The Movie",
		ContentYear: intPtr(2000), IMDbID: "tt0814243",
	})
	want = filepath.Join(cfg.MoviesDir, "One Piece - The Movie (2000) {imdb-tt0814243}",
		"One Piece - The Movie (2000) {imdb-tt0814243}.mkv")
	if movie != want {
		t.Errorf("movie:\n got  %q\n want %q", movie, want)
	}

	// What the library scanner reads back from the renamed file still matches.
	if s, e := library.ParseSeasonEpisode(filepath.Base(anime)); s != 1 || e != 1 {
		t.Errorf("scanner parses %d/%d from %q", s, e, filepath.Base(anime))
	}
	if got := library.CleanTitle(filepath.Base(movie)); got != "One Piece The Movie" {
		t.Errorf("scanner title = %q", got)
	}
}

// Wire contract with the web's claimPendingTasks (AgentClaimedTask): the JSON
// field names it sends must reach the template. null ids (a movie has no tvdb)
// decode as unknown.
func TestNamingFieldsSurviveTheTaskPayload(t *testing.T) {
	payload := `{"id":"t1","infoHash":"abc","title":"One.Piece.S01E01.1080p","preferredMethod":"auto",
		"imdbId":"tt0388629","tmdbId":37854,"tvdbId":81797,"episodeTitle":"I'm Luffy!",
		"episodeTitles":{"1":"I'm Luffy!"},"contentType":"show","contentTitle":"One Piece",
		"contentYear":1999,"season":1,"episode":1,"collectionName":null}`
	var at agent.Task
	if err := json.Unmarshal([]byte(payload), &at); err != nil {
		t.Fatal(err)
	}
	task := NewTaskFromAgent(at)
	if task.TmdbID != 37854 || task.TvdbID != 81797 || task.EpisodeTitle != "I'm Luffy!" || task.EpisodeTitles["1"] != "I'm Luffy!" {
		t.Fatalf("naming fields lost: tmdb=%d tvdb=%d title=%q titles=%v", task.TmdbID, task.TvdbID, task.EpisodeTitle, task.EpisodeTitles)
	}
	var movie agent.Task
	if err := json.Unmarshal([]byte(`{"id":"m","tmdbId":19576,"tvdbId":null}`), &movie); err != nil || movie.TvdbID != 0 {
		t.Fatalf("null tvdbId should decode as 0, got %d (%v)", movie.TvdbID, err)
	}

	cfg := plexCfg(t, t.TempDir())
	got := organizeOne(t, cfg, "One.Piece.S01E01.1080p.mkv", task)
	if want := "One Piece (1999) - S01E01 - I'm Luffy!.mkv"; filepath.Base(got) != want {
		t.Errorf("got %q want %q", filepath.Base(got), want)
	}
}

// Missing metadata (old server, unmatched id) drops the optional segments
// instead of writing "{imdb-}" or "()".
func TestOrganizePlexNamingWithoutIDs(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	got := organizeOne(t, cfg, "Some.Film.2021.1080p.mkv", &Task{
		Title: "Some.Film.2021.1080p", ContentType: "movie", ContentTitle: "Some Film", ContentYear: intPtr(2021),
	})
	if want := filepath.Join(cfg.MoviesDir, "Some Film (2021)", "Some Film (2021).mkv"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// A second version of the same title still coexists: the version tag goes
// after the id tag.
func TestOrganizePlexNamingKeepsVersionsApart(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	task := func(title string) *Task {
		return &Task{Title: title, ContentType: "movie", ContentTitle: "Dune", ContentYear: intPtr(2021), IMDbID: "tt1160419"}
	}
	first := organizeOne(t, cfg, "Dune.2021.1080p.mkv", task("Dune.2021.1080p"))
	mustMkdir(t, cfg.OutputDir)
	src := filepath.Join(cfg.OutputDir, "Dune.2021.2160p.HDR.mkv")
	mustWrite(t, src, bytesPattern(episodeBytes+1))
	second, err := organize(&Result{FilePath: src, FileName: filepath.Base(src), Method: MethodTorrent}, task("Dune.2021.2160p.HDR"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "Dune (2021) {imdb-tt1160419}.mkv" ||
		filepath.Base(second) != "Dune (2021) {imdb-tt1160419} [2160p HDR].mkv" {
		t.Errorf("versions: %q / %q", first, second)
	}
}

// A season pack names every episode with its own title from the server's map,
// and a 4-digit absolute episode keeps all its digits.
func TestOrganizePlexNamingSeasonPackEpisodeTitles(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	release := filepath.Join(cfg.OutputDir, "One.Piece.S22.1080p.WEB")
	mustMkdir(t, release)
	for i, name := range []string{"One.Piece.S22E1089.1080p.WEB.mkv", "One.Piece.S22E1090.1080p.WEB.mkv"} {
		mustWrite(t, filepath.Join(release, name), bytesPattern(episodeBytes+i))
	}
	task := &Task{
		ID: "pack", ContentType: "show", ContentTitle: "One Piece", ContentYear: intPtr(1999),
		IMDbID: "tt0388629", Season: intPtr(22),
		EpisodeTitles: map[string]string{"1089": "Entering a New Chapter! Luffy and Sabo's Paths!"},
	}
	final, err := organize(&Result{FilePath: release, FileName: filepath.Base(release), Method: MethodTorrent}, task, cfg)
	if err != nil {
		t.Fatal(err)
	}
	seasonDir := filepath.Join(cfg.TVShowsDir, "One Piece (1999) {imdb-tt0388629}", "Season 22")
	if final != seasonDir {
		t.Errorf("final = %q, want %q", final, seasonDir)
	}
	for _, name := range []string{
		"One Piece (1999) - S22E1089 - Entering a New Chapter! Luffy and Sabo's Paths!.mkv",
		"One Piece (1999) - S22E1090.mkv", // no title in the map → segment dropped
	} {
		if _, err := os.Stat(filepath.Join(seasonDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

// A show without a TMDB year must not take one from the release name: it
// would differ per episode and split the show across folders.
func TestOrganizePlexNamingShowYearOnlyFromServer(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	got := organizeOne(t, cfg, "Daily.Show.2024.10.05.S2024E10.mkv", &Task{
		Title: "Daily.Show.2024.10.05.S01E10", ContentType: "show", ContentTitle: "Daily Show", Season: intPtr(1), Episode: intPtr(10),
	})
	if want := filepath.Join(cfg.TVShowsDir, "Daily Show", "Season 01", "Daily Show - S01E10.mkv"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestOrganizePlexNamingCollectionNeverVanishes(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	got := organizeOne(t, cfg, "a.mkv", &Task{Title: "a", ContentType: "movie", ContentTitle: "Film", CollectionName: "???", ContentYear: intPtr(2001)})
	if want := filepath.Join(cfg.MoviesDir, "Unknown", "Film (2001)", "Film (2001).mkv"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// A pack holding a file of ANOTHER season must not give it a title from the
// task season's map; and a season-first custom layout never reports the
// library root as the task's path.
func TestOrganizePackTitlesAndSeasonFirstLayout(t *testing.T) {
	root := t.TempDir()
	cfg := plexCfg(t, root)
	scheme, err := naming.Resolve("plex", "", "Season {s00}/{n}/{n} - {s00e00}< - {t}>")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Naming = scheme
	release := filepath.Join(cfg.OutputDir, "Show.S01")
	mustMkdir(t, release)
	for i, f := range []string{"Show.S01E01.mkv", "Show.S01E02.mkv"} {
		mustWrite(t, filepath.Join(release, f), bytesPattern(episodeBytes+i))
	}
	task := &Task{ID: "p", ContentType: "show", ContentTitle: "Show", Season: intPtr(1),
		EpisodeTitles: map[string]string{"1": "Pilot", "2": "Second"}}
	final, err := organize(&Result{FilePath: release, FileName: "Show.S01", Method: MethodTorrent}, task, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg.TVShowsDir, "Season 01", "Show"); final != want {
		t.Errorf("final = %q, want %q", final, want)
	}

	// Multi-season pack: the folders share only the library root.
	multi := filepath.Join(cfg.OutputDir, "Other.COMPLETE")
	mustMkdir(t, multi)
	for i, f := range []string{"Other.S01E01.mkv", "Other.S02E01.mkv"} {
		mustWrite(t, filepath.Join(multi, f), bytesPattern(episodeBytes+10+i))
	}
	final, err = organize(&Result{FilePath: multi, FileName: "Other.COMPLETE", Method: MethodTorrent},
		&Task{ID: "m", ContentType: "show", ContentTitle: "Other"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg.TVShowsDir, "Season 01", "Other"); final != want {
		t.Errorf("multi-season final = %q, want %q (never the library root)", final, want)
	}

	dir, name := packEpisodeDest("Show.S02E01.mkv", cfg, naming.Vars{Title: "Show"}, task.EpisodeTitles, 1)
	if name != "Show - S02E01.mkv" || filepath.Base(dir) != "Show" {
		t.Errorf("other-season file borrowed a title: %q / %q", dir, name)
	}
}

// A multi-file release whose rendered stem contains a dot (episode title
// "Mr. 3's Plan") keeps its whole name: only a real video extension is cut.
func TestOrganizeDirKeepsDottedStem(t *testing.T) {
	cfg := plexCfg(t, t.TempDir())
	release := filepath.Join(cfg.OutputDir, "One.Piece.S01E10.1080p.WEB.x265-GRP")
	mustMkdir(t, release)
	mustWrite(t, filepath.Join(release, "One.Piece.S01E10.1080p.WEB.x265-GRP.mkv"), bytesPattern(episodeBytes))
	task := &Task{
		Title: filepath.Base(release), ContentType: "show", ContentTitle: "One Piece", ContentYear: intPtr(1999),
		Season: intPtr(1), Episode: intPtr(10), EpisodeTitle: "Mr. 3's Plan",
	}
	got, err := organize(&Result{FilePath: release, FileName: filepath.Base(release), Method: MethodTorrent}, task, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := "One Piece (1999) - S01E10 - Mr. 3's Plan.mkv"; filepath.Base(got) != want {
		t.Errorf("got %q want %q", filepath.Base(got), want)
	}
}
