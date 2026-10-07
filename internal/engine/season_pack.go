package engine

import (
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent"

	"github.com/Unarr-app/unarr-cli/internal/library"
	"github.com/Unarr-app/unarr-cli/internal/naming"
)

// Season packs: a task that names a show (or a season) but NO episode is a
// request for the whole release. The torrent path used to keep only the largest
// video of any multi-file torrent, so a 15-file season pack landed one episode,
// left the rest as sparse stubs and reported COMPLETED (Discord report
// 2026-10-03, v1.15.2: "His.Dark.Materials.S02 … selected S02E02 + 0 subs,
// skipped 14 files"). The server's debrid fan-out already assumes the torrent
// path takes everything (web: download-tasks.ts maybeFanOutPack).
//
// The pack decision is made from the FILES, not only from the task: a
// single-episode release whose row lacks an episode number still holds one real
// video (plus maybe a sample), and stays on the single-file path.

// wantsWholePack reports whether the task asks for every episode of a release
// rather than one file: a show — or any task carrying a season — with no episode.
// "Download S02E05" is a one-file intent even when the release behind it is a pack.
func wantsWholePack(task *Task) bool {
	if task == nil || task.Episode != nil {
		return false
	}
	return task.ContentType == "show" || task.Season != nil
}

// sampleRegex matches "sample" as a whole word anywhere in a path
// ("Sample/x.mkv", "x.sample.mkv", "x-sample.mkv") but not inside a word
// ("resampled").
var sampleRegex = regexp.MustCompile(`(?i)(^|[^a-z0-9])sample([^a-z0-9]|$)`)

func isSamplePath(p string) bool { return sampleRegex.MatchString(p) }

// packCandidate is one file of a release, by path and size — the common shape
// of a torrent's file list (selection) and a finished release dir (organize), so
// both decide what an episode is with the same rule.
type packCandidate struct {
	path string
	size int64
}

// isEpisodeVideo: a real video — not a sample, not below the stub floor.
func (c packCandidate) isEpisodeVideo() bool {
	return isVideoFile(c.path) && !isSamplePath(c.path) && c.size >= minPlausibleVideoBytes
}

// fileSeason is the season a file name carries (0 = none).
func fileSeason(path string) int {
	s, _ := library.ParseSeasonEpisode(filepath.Base(path))
	return s
}

// packEpisodes returns the indexes of the candidates that are episodes of the
// pack, and the season the pack was narrowed to (0 = not narrowed). When
// wantSeason > 0 and at least one episode parses to it, episodes parsed to OTHER
// seasons are dropped — a complete-series pack queued as "Season 2" takes season
// 2 only. Returns nil when fewer than two remain: one video is the single-file
// case.
func packEpisodes(cands []packCandidate, wantSeason int) ([]int, int) {
	var episodes []int
	narrowTo := 0
	for i, c := range cands {
		if !c.isEpisodeVideo() {
			continue
		}
		episodes = append(episodes, i)
		if wantSeason > 0 && fileSeason(c.path) == wantSeason {
			narrowTo = wantSeason
		}
	}
	if narrowTo > 0 {
		kept := episodes[:0]
		for _, i := range episodes {
			if inSeason(cands[i].path, narrowTo) {
				kept = append(kept, i)
			}
		}
		episodes = kept
	}
	if len(episodes) < 2 {
		return nil, 0
	}
	return episodes, narrowTo
}

// inSeason: an unnumbered file belongs to any season.
func inSeason(path string, season int) bool {
	s := fileSeason(path)
	return season <= 0 || s == 0 || s == season
}

func taskSeason(task *Task) int {
	if task != nil && task.Season != nil {
		return *task.Season
	}
	return 0
}

// selectPack selects every episode of a season pack plus its subtitles.
// ok=false when the torrent holds fewer than two episode videos — the caller
// then takes the single-file path. fileName is the torrent's root dir, so the
// result is the release directory and organize files each episode.
func selectPack(t *torrent.Torrent, files []*torrent.File, task *Task) (selection, bool) {
	cands := make([]packCandidate, len(files))
	for i, f := range files {
		cands[i] = packCandidate{path: f.DisplayPath(), size: f.Length()}
	}
	episodes, season := packEpisodes(cands, taskSeason(task))
	if episodes == nil {
		return selection{}, false
	}
	for i, c := range cands {
		if subExts[strings.ToLower(filepath.Ext(c.path))] && !isSamplePath(c.path) && inSeason(c.path, season) {
			episodes = append(episodes, i)
		}
	}

	sel := selection{fileName: t.Name(), files: make([]*torrent.File, 0, len(episodes))}
	for _, i := range episodes {
		files[i].Download()
		sel.totalBytes += files[i].Length()
		sel.files = append(sel.files, files[i])
	}
	log.Printf("[%s] season pack: %d files (%s), skipped %d",
		task.ShortID(), len(sel.files), formatBytes(sel.totalBytes), len(files)-len(sel.files))
	return sel, true
}

// pickEpisodeVideo returns the video of a multi-file torrent that a
// single-episode task asked for: the largest file whose name parses to the
// task's episode (and season, when both are known). nil when the task names no
// episode or no file matches — the caller then keeps the largest video, which in
// a pack is merely the longest episode, not the one asked for.
func pickEpisodeVideo(files []*torrent.File, task *Task) *torrent.File {
	if task == nil || task.Episode == nil {
		return nil
	}
	want := taskSeason(task)
	var best *torrent.File
	for _, f := range files {
		p := f.DisplayPath()
		if !isVideoFile(p) || isSamplePath(p) {
			continue
		}
		s, e := library.ParseSeasonEpisode(filepath.Base(p))
		if e != *task.Episode || (want > 0 && s > 0 && s != want) {
			continue
		}
		if best == nil || f.Length() > best.Length() {
			best = f
		}
	}
	return best
}

// organizeShowPack files every episode of a finished season-pack directory into
// the show's library folder — "Show/Season 02/Show - S02E03.mkv" each, subtitles
// alongside — instead of organizeDir's single principal video. Each episode goes
// through moveToDir, so multi-version coexistence, the byte-identical dedup and
// the subtitle drag apply per episode exactly as for a single-episode download.
//
// Returns ok=false (nothing moved) when the directory is not a pack after all —
// fewer than two episode videos — so the caller keeps the single-video path.
// The returned path is the season folder, or the show folder when the pack spans
// several seasons.
func organizeShowPack(result *Result, task *Task, show naming.Vars, cfg OrganizeConfig) (string, bool, error) {
	cands, err := scanVideos(result.FilePath)
	if err != nil {
		return "", false, fmt.Errorf("scan pack dir: %w", err)
	}
	wantSeason := taskSeason(task)
	episodes, _ := packEpisodes(cands, wantSeason)
	if episodes == nil {
		return "", false, nil
	}

	landed := map[string]bool{}
	for _, i := range episodes {
		src := cands[i].path
		destDir, destName := packEpisodeDest(src, cfg, show, task.EpisodeTitles, wantSeason)
		file := &Result{FilePath: src, FileName: filepath.Base(src), Method: result.Method, Size: cands[i].size}
		finalPath, err := moveToDir(file, task, destDir, destName, cfg)
		if err != nil {
			return "", true, fmt.Errorf("file %s: %w", filepath.Base(src), err)
		}
		landed[filepath.Dir(finalPath)] = true
	}

	// The episodes and their subs are out; what is left is samples/nfo/screens.
	cleanupReleaseDir(result.FilePath, cfg.OutputDir)

	// The season folder, or — for a pack spanning several seasons — the folder
	// they all share (the show folder in every built-in layout).
	dirs := make([]string, 0, len(landed))
	for dir := range landed {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	final := commonDir(dirs)
	// A custom season-first layout ("Season {s00}/{n}/…") shares no folder
	// below the library root. Never report the root itself as this task's
	// path — the server matches library files under it — report the first
	// season folder instead.
	if !strings.HasPrefix(final, filepath.Clean(cfg.TVShowsDir)+string(filepath.Separator)) {
		final = dirs[0]
	}
	return final, true, nil
}

// packEpisodeDest is where one pack episode lands: its season folder, named by
// the series template ("Show - SxxEyy.ext" by default), with the episode's title
// from the server's per-season map when the template uses {t}. Unnumbered files
// keep their own name — inventing an episode number would mislabel them in the
// media player.
func packEpisodeDest(src string, cfg OrganizeConfig, show naming.Vars, titles map[string]string, fallbackSeason int) (dir, name string) {
	season, episode := library.ParseSeasonEpisode(filepath.Base(src))
	if season == 0 {
		season = fallbackSeason
	}
	v := show
	v.Season, v.Episode, v.EpisodeTitle = nil, nil, ""
	if season > 0 {
		v.Season = &season
		if episode > 0 {
			v.Episode = &episode
			// The server's map covers the task's season only: a stray file of
			// another season must not borrow a title from it.
			if season == fallbackSeason {
				v.EpisodeTitle = titles[strconv.Itoa(episode)]
			}
		}
	}
	return renderDest(cfg.scheme().Series, cfg.TVShowsDir, v, filepath.Ext(src))
}

// scanVideos lists every video file under dir, recursively: some packs keep
// each episode in its own subfolder.
func scanVideos(dir string) ([]packCandidate, error) {
	var cands []packCandidate
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isVideoFile(d.Name()) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		cands = append(cands, packCandidate{path: p, size: info.Size()})
		return nil
	})
	return cands, err
}
