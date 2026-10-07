package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

const gb = int64(1) << 30

// The 2026-09-23 NAS case: a 1.5 GB 1080p torrent fell back to usenet and the
// search took the largest result, a 58.8 GB 2160p remux.
func TestPickNzbReproducesTheChosenRelease(t *testing.T) {
	results := []agent.NzbSearchResult{
		{NzbID: "remux", Title: "Constantine.City.of.Demons.2018.UHD.BluRay.2160p.DTS-HD.MA.5.1.DV.HEVC.HYBRID.REMUX-FraMeSToR", Size: 58 * gb},
		{NzbID: "1080-big", Title: "Constantine.City.of.Demons.2018.1080p.BluRay.x264-GRP", Size: 9 * gb, Grabs: 50},
		{NzbID: "1080-close", Title: "Constantine.City.of.Demons.2018.1080p.WEB-DL.x265", Size: 2 * gb, Grabs: 3},
		{NzbID: "720", Title: "Constantine.City.of.Demons.2018.720p.WEB", Size: 1500 << 20},
	}
	target := nzbTarget{resolution: "1080p", size: 1500 << 20}
	got := pickNzb(results, target)
	if got == nil || got.NzbID != "1080-close" {
		t.Fatalf("picked %+v, want the 1080p release closest in size", got)
	}
}

func TestPickNzbRefusesRatherThanGuess(t *testing.T) {
	cases := []struct {
		name    string
		results []agent.NzbSearchResult
		target  nzbTarget
	}{
		{"only another resolution", []agent.NzbSearchResult{{Title: "Film.2160p.REMUX", Size: 2 * gb}}, nzbTarget{resolution: "1080p", size: 2 * gb}},
		{"resolution not stated", []agent.NzbSearchResult{{Title: "Film.BluRay.x264", Size: 2 * gb}}, nzbTarget{resolution: "1080p", size: 2 * gb}},
		{"far too big", []agent.NzbSearchResult{{Title: "Film.1080p.REMUX", Size: 30 * gb}}, nzbTarget{resolution: "1080p", size: 2 * gb}},
		{"far too small", []agent.NzbSearchResult{{Title: "Film.1080p.sample", Size: 100 << 20}}, nzbTarget{resolution: "1080p", size: 2 * gb}},
		{"size unknown against a known target", []agent.NzbSearchResult{{Title: "Film.1080p", Size: 0}}, nzbTarget{resolution: "1080p", size: 2 * gb}},
		{"no results", nil, nzbTarget{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickNzb(tc.results, tc.target); got != nil {
				t.Errorf("picked %+v, want none", got)
			}
		})
	}
}

func TestPickNzbPartialTargets(t *testing.T) {
	results := []agent.NzbSearchResult{
		{NzbID: "a", Title: "Film.1080p", Size: 4 * gb, Grabs: 1},
		{NzbID: "b", Title: "Film.2160p", Size: 20 * gb, Grabs: 1},
		{NzbID: "c", Title: "Film.1080p", Size: 8 * gb, Grabs: 9},
	}
	if got := pickNzb(results, nzbTarget{}); got.NzbID != "b" {
		t.Errorf("nothing known: picked %s, want the largest (old rule)", got.NzbID)
	}
	if got := pickNzb(results, nzbTarget{resolution: "1080p"}); got.NzbID != "c" {
		t.Errorf("resolution only: picked %s, want largest 1080p", got.NzbID)
	}
	if got := pickNzb(results, nzbTarget{size: 5 * gb}); got.NzbID != "a" {
		t.Errorf("size only: picked %s, want closest size", got.NzbID)
	}
	tie := []agent.NzbSearchResult{{NzbID: "few", Title: "F.1080p", Size: 2 * gb, Grabs: 1}, {NzbID: "many", Title: "F.1080p", Size: 2 * gb, Grabs: 7}}
	if got := pickNzb(tie, nzbTarget{resolution: "1080p", size: 2 * gb}); got.NzbID != "many" {
		t.Errorf("tie: picked %s, want most grabbed", got.NzbID)
	}
	uhd := []agent.NzbSearchResult{{NzbID: "4k", Title: "Film.4K.HDR", Size: 20 * gb}}
	if got := pickNzb(uhd, nzbTarget{resolution: "2160p", size: 15 * gb}); got == nil {
		t.Error("4K in the NZB title must count as 2160p")
	}
}

func TestNzbTargetForReadsTitleAndSize(t *testing.T) {
	task := NewTaskFromAgent(agent.Task{ID: "t", Title: "Film (2018) [1080P][Castellano].mp4", DirectFileSize: 3 * gb})
	if got := nzbTargetFor(task, "720p"); got.resolution != "1080p" || got.size != 3*gb {
		t.Errorf("target = %+v, want 1080p (title beats preference) / DirectFileSize", got)
	}
	task.SetTotalBytes(1500 << 20)
	if got := nzbTargetFor(task, ""); got.size != 1500<<20 {
		t.Errorf("size = %d, want the torrent's TotalBytes once known", got.size)
	}
	// A magnet that stalled before metadata: no size, no resolution in the
	// title. The configured quality keeps it from "largest wins".
	bare := NewTaskFromAgent(agent.Task{ID: "t2", Title: "Constantine City of Demons"})
	if got := nzbTargetFor(bare, "1080p"); got.resolution != "1080p" || got.size != 0 {
		t.Errorf("bare target = %+v, want preferred 1080p", got)
	}
	// Same dead magnet, but the server knows the torrent's size.
	known := NewTaskFromAgent(agent.Task{ID: "t3", Title: "Constantine City of Demons", ReleaseSize: 1500 << 20})
	if got := nzbTargetFor(known, "1080p"); got.size != 1500<<20 {
		t.Errorf("size = %d, want the server's releaseSize", got.size)
	}
}

// The wire field the web sends (claimPendingTasks → releaseSize) must land in
// the engine task.
func TestReleaseSizeDecodesFromTheWire(t *testing.T) {
	var at agent.Task
	if err := json.Unmarshal([]byte(`{"id":"t","title":"x","releaseSize":1610612736}`), &at); err != nil {
		t.Fatal(err)
	}
	if got := NewTaskFromAgent(at).ReleaseSize; got != 1610612736 {
		t.Errorf("ReleaseSize = %d", got)
	}
	if err := json.Unmarshal([]byte(`{"id":"t","releaseSize":null}`), &at); err != nil {
		t.Errorf("null releaseSize (old tasks) must decode: %v", err)
	}
}

func TestReleaseResolution(t *testing.T) {
	cases := map[string]string{
		"Film.2018.1080p.BluRay.x264":                "1080p",
		"Film (2018) [1080P][Castellano].mp4":        "1080p",
		"Movie.2019.UHD.BluRay.1080p.x264":           "1080p", // explicit pixels beat the UHD edition tag
		"Film.2018.2160p.UHD.REMUX":                  "2160p",
		"Film.2018.4K.HDR":                           "2160p",
		"The.4Kings.2018.1080p":                      "1080p",
		"The.4Kings.2018":                            "",
		"a8f94kd2e1b.mkv":                            "",
		"Film.1720p.x264":                            "",
		"Show.S01E01.1080i.HDTV":                     "1080p",
		"Constantine.City.of.Demons.2018.UHD.BluRay": "2160p",
		"Film.2018.WEB-DL":                           "",
		"Film.2018.1080p60.WEB":                      "1080p",
		"Film.2018.2160p60.HDR":                      "2160p",
		"Film.2018.BD1080P.x264":                     "1080p",
		"Film.2018.HD1080p":                          "1080p",
		"Film.2018.1920x1080.x264":                   "1080p",
		"Film.2018.1280x720":                         "720p",
		"Film.2018.1080px":                           "",
	}
	for title, want := range cases {
		if got := releaseResolution(title); got != want {
			t.Errorf("releaseResolution(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestNzbResolutionPrefersTheServerParse(t *testing.T) {
	r := agent.NzbSearchResult{Title: "obfuscated-4kd9e2"}
	r.Parsed.Resolution = "1080p"
	if got := nzbResolution(&r); got != "1080p" {
		t.Errorf("got %q, want the server's 1080p", got)
	}
	r.Parsed.Resolution = "4k"
	if got := nzbResolution(&r); got != "2160p" {
		t.Errorf("got %q, want 4k normalised to 2160p", got)
	}
}

func TestNzbTargetString(t *testing.T) {
	if got := (nzbTarget{}).String(); got != "resolution unknown, size unknown" {
		t.Errorf("empty target = %q", got)
	}
	if got := (nzbTarget{resolution: "1080p", size: 1500 << 20}).String(); !strings.HasPrefix(got, "1080p, 1.5") {
		t.Errorf("target = %q", got)
	}
}

// A known size already identifies the release: the configured quality must not
// turn into a filter that rejects it (a 1.5 GB 1080p with preferred 2160p).
func TestPreferredQualityYieldsToAKnownSize(t *testing.T) {
	task := NewTaskFromAgent(agent.Task{ID: "t", Title: "Constantine City of Demons", ReleaseSize: 1500 << 20})
	got := nzbTargetFor(task, "2160p")
	if got.resolution != "" || got.size != 1500<<20 {
		t.Fatalf("target = %+v, want size only", got)
	}
	results := []agent.NzbSearchResult{
		{NzbID: "remux", Title: "Constantine.2018.2160p.REMUX", Size: 58 * gb},
		{NzbID: "right", Title: "Constantine.2018.1080p.x265", Size: 1600 << 20},
	}
	if best := pickNzb(results, got); best == nil || best.NzbID != "right" {
		t.Errorf("picked %+v, want the 1.6 GB release", best)
	}
}

func TestSetPreferredQualityIgnoresNonResolutions(t *testing.T) {
	u := NewUsenetDownloader(nil)
	for _, q := range []string{"auto", "best", "", "banana"} {
		u.SetPreferredQuality(q)
		if u.preferredQuality != "" {
			t.Errorf("SetPreferredQuality(%q) = %q, want none", q, u.preferredQuality)
		}
	}
	u.SetPreferredQuality("4K")
	if u.preferredQuality != "2160p" {
		t.Errorf("4K = %q, want 2160p", u.preferredQuality)
	}
}

func remuxOnlyIndexer(t *testing.T) *UsenetDownloader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agent.NzbSearchResponse{Results: []agent.NzbSearchResult{
			{NzbID: "remux", Title: "Film.2018.2160p.REMUX", Size: 58 * gb},
		}})
	}))
	t.Cleanup(srv.Close)
	return NewUsenetDownloader(agent.NewClient(srv.URL, "k", "test"))
}

// No matching NZB makes usenet unavailable — never the wrong release — and
// says why.
func TestAvailableIsFalseWhenNoNzbMatchesTheRelease(t *testing.T) {
	u := remuxOnlyIndexer(t)
	task := NewTaskFromAgent(agent.Task{ID: "t", Title: "Film (2018) [1080P].mp4", IMDbID: "tt1"})
	task.SetTotalBytes(1500 << 20)
	ok, err := u.Available(context.Background(), task)
	if ok || !errors.Is(err, ErrNoMatchingNzb) {
		t.Errorf("Available = %v, %v; want false + ErrNoMatchingNzb (only a 2160p remux for a 1080p torrent)", ok, err)
	}
}

// A usenet-only agent used to take the largest NZB; now the task fails, and
// its error must say why instead of a bare "no download method available".
func TestResolveMethodCarriesTheNoMatchReason(t *testing.T) {
	u := remuxOnlyIndexer(t)
	task := NewTaskFromAgent(agent.Task{ID: "t", Title: "Film (2018) [1080P].mp4", IMDbID: "tt1", InfoHash: "abc"})
	task.SetTotalBytes(1500 << 20)
	_, err := resolveMethod(context.Background(), task, map[DownloadMethod]Downloader{MethodUsenet: u}, []string{"usenet"})
	if !errors.Is(err, ErrNoMatchingNzb) || !strings.Contains(err.Error(), "1080p") {
		t.Errorf("err = %v, want the no-match reason with the target", err)
	}
}

// An episode's IMDb id is the show's: the search must carry season/episode or
// it returns the whole series.
func TestSearchBestNzbSendsSeasonAndEpisode(t *testing.T) {
	var got agent.NzbSearchParams
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(agent.NzbSearchResponse{Results: []agent.NzbSearchResult{{NzbID: "ep", Title: "Show.S02E05.1080p", Size: gb}}})
	}))
	defer srv.Close()
	u := NewUsenetDownloader(agent.NewClient(srv.URL, "k", "test"))
	s, e := 2, 5
	task := NewTaskFromAgent(agent.Task{ID: "t", Title: "Show S02E05 1080p", IMDbID: "tt123", Season: &s, Episode: &e})
	best, err := u.searchBestNzb(context.Background(), task)
	if err != nil || best == nil || best.NzbID != "ep" {
		t.Fatalf("best = %+v, err = %v", best, err)
	}
	if got.Season == nil || *got.Season != 2 || got.Episode == nil || *got.Episode != 5 {
		t.Errorf("search params = %+v, want season 2 episode 5", got)
	}
}

func TestLoadOrFetchNzbCacheIsKeyedByNzbID(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte("nzb:" + r.URL.Query().Get("nzbId")))
	}))
	defer srv.Close()
	u := NewUsenetDownloader(agent.NewClient(srv.URL, "k", "test"))
	dir := t.TempDir()
	ctx := context.Background()

	// A cache written before the id sidecar existed — it held the wrong NZB.
	if err := os.WriteFile(filepath.Join(dir, "task.nzb"), []byte("nzb:remux"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, cached, err := u.loadOrFetchNzb(ctx, "task", dir, "task", "right")
	if err != nil || string(data) != "nzb:right" || cached {
		t.Fatalf("legacy cache: got %q (cached %v), %v - want a fresh fetch of the chosen NZB", data, cached, err)
	}
	data, cached, _ = u.loadOrFetchNzb(ctx, "task", dir, "task", "right")
	if string(data) != "nzb:right" || !cached || fetches.Load() != 1 {
		t.Errorf("same NZB again: got %q after %d fetches, want the cache (1 fetch)", data, fetches.Load())
	}
	data, _, _ = u.loadOrFetchNzb(ctx, "task", dir, "task", "other")
	if string(data) != "nzb:other" || fetches.Load() != 2 {
		t.Errorf("different NZB: got %q after %d fetches, want a refetch", data, fetches.Load())
	}

	dropNzbCache(dir, "task")
	for _, f := range []string{"task.nzb", "task.nzb.id"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("dropNzbCache left %s behind", f)
		}
	}
}

func TestUsenetTaskDirAvoidsAFileWithTheSameName(t *testing.T) {
	out := t.TempDir()
	title := "Constantine City Of Demons (2018) [1080P][Castellano].mp4"
	if got := usenetTaskDir(out, title, "70027b50"); got != filepath.Join(out, title) {
		t.Errorf("free name: dir = %s", got)
	}
	// The torrent attempt left its single file under that exact name.
	if err := os.WriteFile(filepath.Join(out, title), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := usenetTaskDir(out, title, "70027b50")
	if dir == filepath.Join(out, title) {
		t.Fatal("dir collides with the torrent's file")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if again := usenetTaskDir(out, title, "70027b50"); again != dir {
		t.Errorf("resume must land in the same folder: %s vs %s", again, dir)
	}
}

func TestUsenetTaskDirAndSymlinks(t *testing.T) {
	out := t.TempDir()
	real := filepath.Join(out, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(out, "to-folder")); err != nil {
		t.Skip("no symlinks here")
	}
	if got := usenetTaskDir(out, "to-folder", "id"); got != filepath.Join(out, "to-folder") {
		t.Errorf("a symlink to a folder is reused, got %s", got)
	}
	if err := os.Symlink(filepath.Join(out, "gone"), filepath.Join(out, "dangling")); err != nil {
		t.Fatal(err)
	}
	dir := usenetTaskDir(out, "dangling", "id")
	if dir == filepath.Join(out, "dangling") {
		t.Fatal("a dangling link is not a folder to write in")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("mkdir %s: %v", dir, err)
	}
}

func TestNameClashIsNotAStorageOutage(t *testing.T) {
	out := t.TempDir()
	file := filepath.Join(out, "taken.mp4")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := os.MkdirAll(file, 0o755)
	if !isNameClash(err, out) {
		t.Errorf("mkdir over a file in a healthy dir (%v) must be a name clash", err)
	}
	if isNameClash(err, filepath.Join(out, "gone")) {
		t.Error("an output dir that is gone is a storage outage")
	}
	if isNameClash(errors.New("permission denied"), out) {
		t.Error("only ENOTDIR is a name clash")
	}
}
