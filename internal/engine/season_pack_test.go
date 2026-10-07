package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// The Discord report (2026-10-03, v1.15.2): a 15-file season pack queued as
// "Season 2" downloaded ONE episode — selectFiles kept only the largest video —
// and the task went COMPLETED with the other 14 left as sparse stubs.

const episodeBytes = minPlausibleVideoBytes + 64<<10

// openPackTorrent builds a torrent from the given files (name → size) and opens
// it against an EMPTY data dir: selection only reads the file list.
func openPackTorrent(t *testing.T, root string, files map[string]int) *torrent.Torrent {
	t.Helper()
	src := filepath.Join(t.TempDir(), root)
	for name, size := range files {
		mustMkdir(t, filepath.Dir(filepath.Join(src, name)))
		mustWrite(t, filepath.Join(src, name), bytesPattern(size))
	}
	var info metainfo.Info
	info.PieceLength = 32 << 10
	if err := info.BuildFromFilePath(src); err != nil {
		t.Fatalf("build info: %v", err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	tor, closeClient := openVerifiedTorrent(t, t.TempDir(), &mi)
	t.Cleanup(closeClient)
	return tor
}

func selectedNames(sel selection) []string {
	var names []string
	for _, f := range sel.files {
		names = append(names, filepath.Base(f.DisplayPath()))
	}
	sort.Strings(names)
	return names
}

func seasonPackFiles() map[string]int {
	return map[string]int{
		"Show.S02E01.1080p.mkv":    episodeBytes,
		"Show.S02E02.1080p.mkv":    episodeBytes + 32<<10, // largest: the old pick
		"Show.S02E03.1080p.mkv":    episodeBytes,
		"Show.S02E03.1080p.en.srt": 2 << 10,
		"Sample/show.sample.mkv":   episodeBytes, // big enough to pass the floor
		"RARBG.txt":                100,
	}
}

func TestSelectFiles_SeasonPackTakesEveryEpisode(t *testing.T) {
	tor := openPackTorrent(t, "Show.S02.1080p", seasonPackFiles())
	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, &Task{ID: "pack", ContentType: "show", Season: intPtr(2)})

	want := []string{"Show.S02E01.1080p.mkv", "Show.S02E02.1080p.mkv", "Show.S02E03.1080p.en.srt", "Show.S02E03.1080p.mkv"}
	got := selectedNames(sel)
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected %v, want %v", got, want)
		}
	}
	if sel.fileName != tor.Name() {
		t.Errorf("fileName = %q, want the release dir %q so organize files every episode", sel.fileName, tor.Name())
	}
	var sum int64
	for _, f := range sel.files {
		sum += f.Length()
	}
	if sel.totalBytes != sum {
		t.Errorf("totalBytes = %d, want the selection's %d", sel.totalBytes, sum)
	}
}

func TestSelectFiles_EpisodeTaskPicksItsEpisodeNotTheLargest(t *testing.T) {
	tor := openPackTorrent(t, "Show.S02.1080p", seasonPackFiles())
	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, &Task{ID: "ep", ContentType: "show", Season: intPtr(2), Episode: intPtr(3)})

	got := selectedNames(sel)
	if len(got) == 0 || got[len(got)-1] != "Show.S02E03.1080p.mkv" {
		t.Fatalf("selected %v, want S02E03 (the episode asked for), not the largest S02E02", got)
	}
}

func TestSelectFiles_MovieKeepsTheLargestVideo(t *testing.T) {
	tor := openPackTorrent(t, "Movie.2020.1080p", map[string]int{
		"Movie.2020.1080p.mkv":   episodeBytes * 2,
		"Featurettes/Making.mkv": episodeBytes,
	})
	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, &Task{ID: "movie", ContentType: "movie"})

	got := selectedNames(sel)
	if len(got) != 1 || got[0] != "Movie.2020.1080p.mkv" {
		t.Fatalf("selected %v, want only the feature", got)
	}
}

func TestPackEpisodes(t *testing.T) {
	big := int64(episodeBytes)
	cases := []struct {
		name       string
		cands      []packCandidate
		wantSeason int
		want       int // number of episodes; 0 = not a pack
	}{
		{"single episode + sample is not a pack", []packCandidate{
			{"Show.S01E01.mkv", big}, {"Show.S01E01.sample.mkv", big},
		}, 0, 0},
		{"stubs below the floor do not count", []packCandidate{
			{"Show.S01E01.mkv", big}, {"Show.S01E02.mkv", 10},
		}, 1, 0},
		{"complete series narrowed to the asked season", []packCandidate{
			{"S01/Show.S01E01.mkv", big}, {"S01/Show.S01E02.mkv", big},
			{"S02/Show.S02E01.mkv", big}, {"S02/Show.S02E02.mkv", big}, {"S02/Show.S02E03.mkv", big},
		}, 2, 3},
		{"unnumbered files are all episodes", []packCandidate{
			{"01.mkv", big}, {"02.mkv", big}, {"03.mkv", big},
		}, 4, 3},
		{"season not in the names keeps everything", []packCandidate{
			{"Show.S01E01.mkv", big}, {"Show.S01E02.mkv", big},
		}, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			episodes, _ := packEpisodes(tc.cands, tc.wantSeason)
			if got := len(episodes); got != tc.want {
				t.Fatalf("episodes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOrganize_SeasonPackFilesEveryEpisode(t *testing.T) {
	root := t.TempDir()
	cfg := OrganizeConfig{
		Enabled:    true,
		TVShowsDir: filepath.Join(root, "TV"),
		OutputDir:  filepath.Join(root, "downloads"),
	}
	release := filepath.Join(cfg.OutputDir, "His.Dark.Materials.S02.1080p.BluRay.x265-RARBG")
	mustMkdir(t, release)
	for i, name := range []string{
		"His.Dark.Materials.S02E01.1080p.BluRay.x265-RARBG.mp4",
		"His.Dark.Materials.S02E02.1080p.BluRay.x265-RARBG.mp4",
		"His.Dark.Materials.S02E03.1080p.BluRay.x265-RARBG.mp4",
	} {
		mustWrite(t, filepath.Join(release, name), bytesPattern(episodeBytes+i))
	}
	mustWrite(t, filepath.Join(release, "His.Dark.Materials.S02E02.1080p.BluRay.x265-RARBG.en.srt"), []byte("1"))
	mustWrite(t, filepath.Join(release, "RARBG.txt"), []byte("junk"))

	task := &Task{ID: "pack", ContentType: "show", ContentTitle: "His Dark Materials", Season: intPtr(2)}
	result := &Result{FilePath: release, FileName: filepath.Base(release), Method: MethodTorrent}
	final, err := organize(result, task, cfg)
	if err != nil {
		t.Fatalf("organize: %v", err)
	}

	seasonDir := filepath.Join(cfg.TVShowsDir, "His Dark Materials", "Season 02")
	if final != seasonDir {
		t.Errorf("final path = %q, want the season folder %q", final, seasonDir)
	}
	for _, name := range []string{
		"His Dark Materials - S02E01.mp4",
		"His Dark Materials - S02E02.mp4",
		"His Dark Materials - S02E02.en.srt",
		"His Dark Materials - S02E03.mp4",
	} {
		if _, err := os.Stat(filepath.Join(seasonDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(release); !os.IsNotExist(err) {
		t.Errorf("release dir should be cleaned up once every episode is filed (stat err = %v)", err)
	}
}

func TestOrganize_SingleEpisodeDirKeepsThePrincipalVideoPath(t *testing.T) {
	root := t.TempDir()
	cfg := OrganizeConfig{
		Enabled:    true,
		TVShowsDir: filepath.Join(root, "TV"),
		OutputDir:  filepath.Join(root, "downloads"),
	}
	release := filepath.Join(cfg.OutputDir, "Show.S01E04.1080p")
	mustMkdir(t, release)
	mustWrite(t, filepath.Join(release, "Show.S01E04.1080p.mkv"), bytesPattern(episodeBytes))
	mustWrite(t, filepath.Join(release, "sample.mkv"), bytesPattern(1024))

	// No episode on the task (row lacked it): still one real video → single-file path.
	task := &Task{ID: "one", ContentType: "show", ContentTitle: "Show", Season: intPtr(1)}
	result := &Result{FilePath: release, FileName: filepath.Base(release), Method: MethodTorrent}
	final, err := organize(result, task, cfg)
	if err != nil {
		t.Fatalf("organize: %v", err)
	}
	if want := filepath.Join(cfg.TVShowsDir, "Show", "Season 01", "Show.S01E04.1080p.mkv"); final != want {
		t.Errorf("final path = %q, want %q", final, want)
	}
}
