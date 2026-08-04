package engine

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// moveMu serializes the "pick a free destination → move onto it" pair in
// moveToDir. Without it two concurrent finalizations of the SAME title (distinct
// tasks, e.g. 1080p and 2160p) could both see the deterministic path free and
// clobber each other in the stat→rename window — exactly the overwrite this
// feature exists to prevent. Moves are cheap (a same-filesystem rename is
// instant); only the rare cross-device fallback copy holds it longer.
var moveMu sync.Mutex

var (
	yearRegex    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	seasonRegex  = regexp.MustCompile(`(?i)S(\d{2})`)
	episodeRegex = regexp.MustCompile(`(?i)S(\d{2})E(\d{2})`)
	altEpRegex   = regexp.MustCompile(`(?i)(\d{1,2})x(\d{2})`) // 1x05 format
	pathReplacer = strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", " -",
		"?", "",
		"*", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "-",
	)
)

// OrganizeConfig holds file organization settings.
type OrganizeConfig struct {
	Enabled    bool
	MoviesDir  string
	TVShowsDir string
	OutputDir  string // download directory — used to clean up torrent subdirectories after move
}

// organize moves a downloaded file into the proper directory structure.
//
// When server metadata is available (ContentType, ContentTitle, Season, CollectionName):
//   - Shows:       TVShowsDir/ContentTitle/Season XX/filename.ext
//   - Collections: MoviesDir/CollectionName/ContentTitle (Year)/filename.ext
//   - Movies:      MoviesDir/ContentTitle (Year)/filename.ext
//
// Falls back to legacy regex-based detection when metadata is missing.
func organize(result *Result, task *Task, cfg OrganizeConfig) (string, error) {
	if !cfg.Enabled || result == nil || result.FilePath == "" {
		return result.FilePath, nil
	}

	var destDir string
	var destFileName string // empty = keep original filename

	ext := filepath.Ext(result.FileName)
	if ext == "" {
		ext = filepath.Ext(result.FilePath)
	}

	if task.ContentType == "show" && cfg.TVShowsDir != "" {
		// TV show: use clean title from server, group all episodes under one folder
		showName := task.ContentTitle
		if showName == "" {
			showName = cleanTitle(task.Title) // fallback
		}
		destDir = filepath.Join(cfg.TVShowsDir, sanitizePath(showName))
		if task.Season != nil {
			destDir = filepath.Join(destDir, fmt.Sprintf("Season %02d", *task.Season))
			// Rename: "ShowName - S01E03.mkv" so media players identify it
			if task.Episode != nil {
				destFileName = fmt.Sprintf("%s - S%02dE%02d%s", sanitizePath(showName), *task.Season, *task.Episode, ext)
			}
		} else if season := detectSeason(result.FileName); season != "" {
			destDir = filepath.Join(destDir, fmt.Sprintf("Season %s", season))
		}

	} else if task.CollectionName != "" && cfg.MoviesDir != "" {
		// Collection movie: CollectionName/MovieTitle (Year)/file
		collDir := sanitizePath(task.CollectionName)
		movieName := task.ContentTitle
		if movieName == "" {
			movieName = cleanTitle(task.Title)
		}
		year := resolveYear(task)
		if year != "" {
			destDir = filepath.Join(cfg.MoviesDir, collDir, fmt.Sprintf("%s (%s)", sanitizePath(movieName), year))
			destFileName = fmt.Sprintf("%s (%s)%s", sanitizePath(movieName), year, ext)
		} else {
			destDir = filepath.Join(cfg.MoviesDir, collDir, sanitizePath(movieName))
			destFileName = fmt.Sprintf("%s%s", sanitizePath(movieName), ext)
		}

	} else if task.ContentType == "movie" && cfg.MoviesDir != "" {
		// Regular movie with server metadata
		movieName := task.ContentTitle
		if movieName == "" {
			movieName = cleanTitle(task.Title)
		}
		year := resolveYear(task)
		if year != "" {
			destDir = filepath.Join(cfg.MoviesDir, fmt.Sprintf("%s (%s)", sanitizePath(movieName), year))
			destFileName = fmt.Sprintf("%s (%s)%s", sanitizePath(movieName), year, ext)
		} else {
			destDir = filepath.Join(cfg.MoviesDir, sanitizePath(movieName))
			destFileName = fmt.Sprintf("%s%s", sanitizePath(movieName), ext)
		}

	} else {
		// No server metadata: fall back to legacy regex-based detection
		return organizeLegacy(result, task, cfg)
	}

	return moveToDir(result, task, destDir, destFileName, cfg)
}

// organizeLegacy is the original regex-based organize logic for tasks without server metadata.
func organizeLegacy(result *Result, task *Task, cfg OrganizeConfig) (string, error) {
	title := task.Title
	if title == "" {
		title = result.FileName
	}

	season := detectSeason(result.FileName)
	isTV := season != ""

	var destDir string
	if isTV && cfg.TVShowsDir != "" {
		showName := cleanTitle(title)
		destDir = filepath.Join(cfg.TVShowsDir, showName)
		if season != "" {
			destDir = filepath.Join(destDir, fmt.Sprintf("Season %s", season))
		}
	} else if cfg.MoviesDir != "" {
		movieName := cleanTitle(title)
		year := yearRegex.FindString(title)
		if year != "" {
			destDir = filepath.Join(cfg.MoviesDir, fmt.Sprintf("%s (%s)", movieName, year))
		} else {
			destDir = filepath.Join(cfg.MoviesDir, movieName)
		}
	} else {
		return result.FilePath, nil
	}

	return moveToDir(result, task, destDir, "", cfg)
}

// moveToDir handles the actual directory creation and file move, including path traversal check.
// If destFileName is non-empty, the file is renamed to that name (instead of keeping the original).
//
// Multi-version coexistence: moveToDir NEVER overwrites an existing file/dir at
// the destination — a 2160p grabbed next to a 1080p, a usenet copy next to a
// torrent one, or a Castilian cut next to an English one all coexist. When the
// deterministic destination is occupied it is redirected to a free,
// version-tagged sibling (see versionDistinctPath). This holds for upgrade tasks
// too (ReplacePath set): landing on a sibling instead of the deterministic path
// lets finalizeVerified's replaceFile back the OLD file up before swapping —
// renaming straight onto ReplacePath would destroy it first. Deliberate
// replacement is always replaceFile's job, never a silent clobber here.
func moveToDir(result *Result, task *Task, destDir, destFileName string, cfg OrganizeConfig) (string, error) {
	// Validate destination is within an expected base directory
	if !((cfg.TVShowsDir != "" && isWithinDir(cfg.TVShowsDir, destDir)) ||
		(cfg.MoviesDir != "" && isWithinDir(cfg.MoviesDir, destDir)) ||
		(cfg.OutputDir != "" && isWithinDir(cfg.OutputDir, destDir))) {
		return "", fmt.Errorf("path traversal blocked: %q is not within any configured directory", destDir)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}

	fileName := filepath.Base(result.FilePath)
	if destFileName != "" {
		fileName = destFileName
	}
	destPath := filepath.Join(destDir, fileName)

	srcInfo, err := os.Stat(result.FilePath)
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}

	// Reserve a free destination + move under a lock so the "is it free? → claim
	// it" pair is atomic against a concurrent finalization of the same title.
	moveMu.Lock()
	defer moveMu.Unlock()

	// Never destroy an existing file/dir at the destination — redirect to a free,
	// version-tagged sibling instead (see the function doc). Applies to normal AND
	// upgrade downloads: the deliberate swap is replaceFile's job afterwards.
	//
	// CAUSE-FIX for the duplicate flood (RC-8): before minting a "(N)"/version
	// sibling, check whether the source is BYTE-IDENTICAL to the file already at the
	// destination. If so this is a redundant re-download (the same episode grabbed
	// again), NOT a new version — cloning it would leave two identical copies (the
	// prod incident: 23 identical BAKI-DOU episodes). Drop the source and return the
	// existing path instead. Only distinct content falls through to a version sibling.
	if pathExists(destPath) {
		// For a single-file result, a byte-identical source is a redundant
		// re-download (same episode grabbed again), NOT a new version — drop it
		// instead of cloning a "(N)" sibling. Directories skip this (organizeDir
		// handles the multi-file case) and distinct content falls through to a
		// version-tagged sibling.
		if !srcInfo.IsDir() && sameContent(result.FilePath, destPath) {
			log.Printf("[organize] %s is byte-identical to existing %s — dropping redundant re-download", result.FilePath, destPath)
			if err := os.Remove(result.FilePath); err != nil {
				log.Printf("[organize] warning: failed to remove redundant source %s: %v", result.FilePath, err)
			}
			// Clean the now-orphaned source dir (subs/junk) just like a normal move.
			cleanupSourceDir(result.FilePath, cfg.OutputDir)
			return destPath, nil
		}
		destPath = versionDistinctPath(destDir, fileName, result, task)
	}
	// The move may have re-pointed destPath, so subtitles must follow the FINAL
	// video name (e.g. "Movie (2023) [1080p ES].es.srt"), not the original.
	finalFileName := filepath.Base(destPath)

	if srcInfo.IsDir() {
		return organizeDir(result, destDir, destFileName, cfg)
	}

	if err := moveFile(result.FilePath, destPath); err != nil {
		return "", fmt.Errorf("move file: %w", err)
	}

	// Move subtitle files alongside the video
	moveSubtitles(result.FilePath, destDir, finalFileName)

	// Clean up the source torrent directory if it's a subdirectory of OutputDir
	// and now empty or only contains junk files (nfo, txt, url, etc.)
	cleanupSourceDir(result.FilePath, cfg.OutputDir)

	return destPath, nil
}

// organizeDir normalizes a multi-file (directory) release. Instead of renaming
// the raw torrent folder into the library untouched (the old behavior — it left
// "Show.S01E02.1080p.WEB.x265-GRP/" as a folder with sample.mkv, .nfo and RARs
// beside the episode), it locates the PRINCIPAL video (largest file with a video
// extension), moves ONLY that into the canonical path, drags its sibling subs, and
// cleans the source dir of the remaining junk (sample clips, nfo, screenshots).
//
// destDir/destFileName come from the caller's canonical layout. destFileName's
// stem is authoritative (the TMDB-clean name); its extension is replaced by the
// real video's extension, since a directory result carries no meaningful ext.
//
// Falls back to the raw dir rename (and logs why) when no video is found inside —
// e.g. an audio-only or an all-archive release we can't safely pick a file from.
// Runs with moveMu already held by the caller (moveToDir).
func organizeDir(result *Result, destDir, destFileName string, cfg OrganizeConfig) (string, error) {
	srcDir := result.FilePath

	videoPath, err := largestVideoIn(srcDir)
	if err != nil {
		return "", fmt.Errorf("scan release dir: %w", err)
	}
	if videoPath == "" {
		// No video inside: don't guess — keep the current behavior (move the raw dir)
		// so nothing is lost, but log it so these releases are visible.
		fallbackDest := filepath.Join(destDir, filepath.Base(srcDir))
		if pathExists(fallbackDest) {
			fallbackDest = versionDistinctPath(destDir, filepath.Base(srcDir), result, nil)
		}
		log.Printf("[organize] no video file in release dir %s — moving folder as-is to %s", srcDir, fallbackDest)
		if err := os.Rename(srcDir, fallbackDest); err != nil {
			return "", fmt.Errorf("move directory: %w", err)
		}
		return fallbackDest, nil
	}

	// Build the canonical video name: the clean stem from destFileName (or the
	// inner video's own name in the legacy no-metadata path) + the REAL extension.
	videoExt := filepath.Ext(videoPath)
	var finalName string
	if destFileName != "" {
		finalName = strings.TrimSuffix(destFileName, filepath.Ext(destFileName)) + videoExt
	} else {
		finalName = filepath.Base(videoPath)
	}

	destPath := filepath.Join(destDir, finalName)
	if pathExists(destPath) {
		destPath = versionDistinctPath(destDir, finalName, result, nil)
	}
	finalFileName := filepath.Base(destPath)

	if err := moveFile(videoPath, destPath); err != nil {
		return "", fmt.Errorf("move principal video: %w", err)
	}

	// Drag the principal video's subtitles (same-basename siblings) alongside it.
	moveSubtitles(videoPath, destDir, finalFileName)

	// Remove the leftover source dir (sample/nfo/screens/RARs). cleanupSourceDir
	// only removes it when no video/subtitle remains — the principal video and its
	// subs are already gone, so what's left is junk.
	cleanupReleaseDir(srcDir, cfg.OutputDir)

	return destPath, nil
}

// largestVideoIn returns the path to the biggest video file directly inside dir
// (non-recursive: torrent releases keep the feature file at the top level; a
// recursive walk would risk pulling an extra from a subfolder). Returns "" when
// the dir holds no video.
func largestVideoIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestSize int64 = -1
	for _, e := range entries {
		if e.IsDir() || !isVideoFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Don't silently skip: a stat failure on a candidate could hide the real
			// feature file. Log and keep scanning the rest.
			log.Printf("[organize] stat %s in %s failed: %v", e.Name(), dir, err)
			continue
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			best = filepath.Join(dir, e.Name())
		}
	}
	return best, nil
}

// cleanupReleaseDir removes the release dir once its principal video + subs have
// been moved out. Unlike cleanupSourceDir (which keys off a file's PARENT and
// refuses when any video/subtitle remains), this targets the release dir itself
// and tolerates leftover subtitle files that didn't match the video basename —
// they're orphaned subs for a video we already relocated. It still refuses to
// delete anything holding another VIDEO (a second feature we didn't move) or a
// subdirectory, and stays confined to outputDir.
func cleanupReleaseDir(releaseDir, outputDir string) {
	if outputDir == "" {
		return
	}
	absOutput, err1 := filepath.Abs(outputDir)
	absDir, err2 := filepath.Abs(releaseDir)
	if err1 != nil || err2 != nil {
		log.Printf("[organize] cleanup: abs path failed for %s / %s", releaseDir, outputDir)
		return
	}
	if absDir == absOutput || !strings.HasPrefix(absDir, absOutput+string(os.PathSeparator)) {
		return // never delete outputDir itself or anything outside it
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		log.Printf("[organize] cleanup: read %s failed: %v", absDir, err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			return // nested content — don't touch
		}
		if isVideoFile(e.Name()) {
			// A leftover video is only a reason to keep the dir if it's a REAL video
			// (>= the anti-stub floor) — that would be a second feature we didn't move.
			// Sample clips / decoy stubs are below the floor: they are junk, so they
			// don't block cleanup (RC-5 leaves sample.mkv behind with the folder).
			info, err := e.Info()
			if err != nil {
				log.Printf("[organize] cleanup: stat %s failed, keeping dir %s: %v", e.Name(), absDir, err)
				return
			}
			if info.Size() >= minPlausibleVideoBytes {
				return // real second video — keep the dir
			}
		}
	}
	if err := os.RemoveAll(absDir); err != nil {
		log.Printf("[organize] cleanup warning: failed to remove release dir %s: %v", absDir, err)
	}
}

// cleanupSourceDir removes the parent directory of srcFile if:
//   - it's a subdirectory of outputDir (any depth, e.g. outputDir/TorrentName/ or outputDir/category/TorrentName/)
//   - it contains no video files or subdirectories after the move
//
// This cleans up leftover junk files (nfo, txt, url, jpg) from multi-file torrents.
func cleanupSourceDir(srcFile, outputDir string) {
	if outputDir == "" {
		return
	}

	srcDir := filepath.Dir(srcFile)
	absOutput, err1 := filepath.Abs(outputDir)
	absSrcDir, err2 := filepath.Abs(srcDir)
	if err1 != nil || err2 != nil {
		return
	}

	// Never delete outputDir itself
	if absSrcDir == absOutput {
		return
	}
	// Must be within outputDir
	if !strings.HasPrefix(absSrcDir, absOutput+string(os.PathSeparator)) {
		return
	}

	entries, err := os.ReadDir(absSrcDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			return // has subdirectories, don't touch
		}
		if isVideoFile(e.Name()) || isSubtitleFile(e.Name()) {
			return // still has video/subtitle files, don't clean
		}
	}

	// Only junk files remain — remove the entire directory
	if err := os.RemoveAll(absSrcDir); err != nil {
		log.Printf("[organize] cleanup warning: failed to remove %s: %v", absSrcDir, err)
	}
}

// isVideoFile checks if a filename has a common video extension. It delegates to
// library.IsVideoExt so the organizer and the library scanner / reconcile sweep
// share ONE canonical extension set — a divergence here (this list once had .m2ts
// but not .mpg/.mpeg/.vob, while library had the inverse) let reconcile misjudge a
// legitimate video's directory as "video-less" and delete it. TestVideoExtParity
// guards against future drift.
func isVideoFile(name string) bool {
	return library.IsVideoExt(name)
}

// detectSeason extracts the season number from a filename using regex (for fallback).
func detectSeason(fileName string) string {
	if m := episodeRegex.FindStringSubmatch(fileName); len(m) > 2 {
		return m[1]
	}
	if m := altEpRegex.FindStringSubmatch(fileName); len(m) > 2 {
		return fmt.Sprintf("%02s", m[1])
	}
	if m := seasonRegex.FindStringSubmatch(fileName); len(m) > 1 {
		return m[1]
	}
	return ""
}

// sanitizePath removes characters that are invalid in file/directory names.
func sanitizePath(name string) string {
	s := pathReplacer.Replace(name)
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "Unknown"
	}
	return s
}

// moveSubtitles moves subtitle files from the source directory to destDir.
// If destFileName is set (video was renamed), subtitles are renamed to match.
// Matches subtitles by video base name (e.g., "Movie.srt", "Movie.en.srt").
func moveSubtitles(srcVideoPath, destDir, destFileName string) {
	srcDir := filepath.Dir(srcVideoPath)
	videoBase := strings.TrimSuffix(filepath.Base(srcVideoPath), filepath.Ext(srcVideoPath))
	destVideoBase := ""
	if destFileName != "" {
		destVideoBase = strings.TrimSuffix(destFileName, filepath.Ext(destFileName))
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() || !isSubtitleFile(e.Name()) {
			continue
		}
		// Match: subtitle must belong to THIS video (boundary-aware). A bare
		// strings.HasPrefix(e.Name(), videoBase) also matched "Movie Extended.srt"
		// — the subtitle of a different video "Movie Extended.mkv" in the same
		// dir — and moved it away from its real owner. SidecarBelongsTo requires a
		// "." / "-" separator after videoBase ("Movie.srt", "Movie.en.srt"), so a
		// space-separated different title is excluded. Shared with library's
		// deleteSidecars so both use one boundary rule.
		if !library.SidecarBelongsTo(e.Name(), videoBase) {
			continue
		}

		subSrc := filepath.Join(srcDir, e.Name())
		subDest := e.Name()
		// Rename subtitle to match new video name if video was renamed
		// e.g., "Movie.en.srt" → "Oppenheimer (2023).en.srt"
		if destVideoBase != "" {
			suffix := strings.TrimPrefix(e.Name(), videoBase) // ".en.srt" or ".srt"
			subDest = destVideoBase + suffix
		}
		destPath := filepath.Join(destDir, subDest)

		if err := moveFile(subSrc, destPath); err != nil {
			log.Printf("[organize] warning: failed to move subtitle %s: %v", e.Name(), err)
			continue
		}
	}
}

// resolveYear returns the content year as a string.
// Prefers the server-provided ContentYear; falls back to regex extraction from the torrent title.
func resolveYear(task *Task) string {
	if task.ContentYear != nil && *task.ContentYear > 0 {
		return fmt.Sprintf("%d", *task.ContentYear)
	}
	return yearRegex.FindString(task.Title)
}

// isSubtitleFile checks if a filename has a common subtitle extension.
func isSubtitleFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".srt", ".sub", ".ass", ".ssa", ".vtt", ".idx":
		return true
	}
	return false
}

// cleanTitle extracts a clean title from a torrent title string.
func cleanTitle(title string) string {
	// Strip a leading media extension first: when the only title available is the
	// download's filename (e.g. a generic debrid "movie.mkv"), the ".mkv" must not
	// survive into the folder name — otherwise organize mints a literal "movie.mkv"
	// directory (the stub-flood folder seen in prod). Real titles have no video ext.
	if isVideoFile(title) {
		title = strings.TrimSuffix(title, filepath.Ext(title))
	}
	// Remove year and everything after common separators
	t := title
	if idx := strings.Index(t, " ("); idx > 0 {
		t = t[:idx]
	}
	// Remove resolution and codec markers
	for _, pattern := range []string{"1080p", "720p", "2160p", "480p", "BluRay", "WEB-DL", "HDTV", "x264", "x265", "HEVC"} {
		if idx := strings.Index(strings.ToLower(t), strings.ToLower(pattern)); idx > 0 {
			t = t[:idx]
		}
	}
	t = strings.TrimRight(t, " .-_")
	if t == "" {
		return title
	}
	return t
}

// replaceFile moves the old file to a backup dir, then moves the new file to the old path.
// Used by upgrade downloads to replace an existing file with a better version.
func replaceFile(oldPath, newPath, backupDir string) error {
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("original file not found: %w", err)
	}

	if backupDir == "" {
		home, _ := os.UserHomeDir()
		backupDir = filepath.Join(home, ".local", "share", "unarr", "replaced")
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	// Move old file to backup (with timestamp to avoid collisions)
	base := filepath.Base(oldPath)
	ext := filepath.Ext(base)
	nameNoExt := strings.TrimSuffix(base, ext)
	backupName := fmt.Sprintf("%s.%d%s", nameNoExt, time.Now().Unix(), ext)
	backupPath := filepath.Join(backupDir, backupName)

	if err := moveFile(oldPath, backupPath); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	// Move new file to old path
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	if err := moveFile(newPath, oldPath); err != nil {
		// Rollback: restore the backup taken above. moveFile has already removed
		// any partial destination, so this rename lands on a clean path.
		os.Rename(backupPath, oldPath)
		return fmt.Errorf("replace failed: %w", err)
	}

	return nil
}

// pathExists reports whether anything (file or directory) already lives at path.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sameContent reports whether two files are byte-identical, using the same
// fingerprint scheme as the library/server (size ‖ first 1 MiB ‖ last 1 MiB) so
// organize's dedup decision matches reconcile's. A mismatched size short-circuits
// before any hashing. On any stat/fingerprint error it returns false — never treat
// an unverifiable pair as identical (that could delete a real distinct download).
func sameContent(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		log.Printf("[organize] dedup: stat %s failed: %v", a, err)
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		log.Printf("[organize] dedup: stat %s failed: %v", b, err)
		return false
	}
	if ai.IsDir() || bi.IsDir() || ai.Size() != bi.Size() {
		return false // different size (or a dir) → definitely not identical
	}
	// Fingerprint is a CHEAP FILTER (size + first/last 1 MiB); it can collide for
	// two files that match on their extremes but differ in the middle. If the
	// fingerprints already differ, they are certainly different — cheap reject.
	fpA, err := library.ComputeFingerprint(a, ai.Size())
	if err != nil {
		log.Printf("[organize] dedup: fingerprint %s failed: %v", a, err)
		return false
	}
	fpB, err := library.ComputeFingerprint(b, bi.Size())
	if err != nil {
		log.Printf("[organize] dedup: fingerprint %s failed: %v", b, err)
		return false
	}
	if fpA != fpB {
		return false
	}
	// Fingerprints match → CONFIRM byte-for-byte before treating them as identical
	// (this decides whether organize removes one as redundant). The full read is
	// only paid on the rare fingerprint match, exactly when being wrong loses data.
	same, err := library.SameFileContent(a, b)
	if err != nil {
		log.Printf("[organize] dedup: full compare %s vs %s failed: %v", a, b, err)
		return false // can't prove identity → treat as different (never delete on doubt)
	}
	return same
}

var (
	resTagRegex  = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4k)\b`)
	hdrTagRegex  = regexp.MustCompile(`(?i)\b(hdr10\+?|hdr|dolby[ .]?vision|dovi|dv|hlg)\b`)
	langTagRegex = regexp.MustCompile(`(?i)\b(castellano|espa(?:ñ|n)ol|latino|ingl[eé]s|english|dual|multi)\b`)
)

// versionDistinctPath returns a destination under destDir that collides with no
// existing file, so a second download of the same title coexists with the first
// instead of overwriting it. The preferred sibling carries a readable version
// tag parsed from the release name (resolution, HDR, audio language) so the
// library shows "Movie (2023) [2160p HDR]" beside "Movie (2023) [1080p]". If
// tagging still collides (two same-quality grabs, e.g. torrent + usenet) the
// download method and then a numeric counter break the tie. Never returns an
// occupied path.
func versionDistinctPath(destDir, fileName string, result *Result, task *Task) string {
	ext := filepath.Ext(fileName)
	stem := strings.TrimSuffix(fileName, ext)

	tag := versionTag(task)
	method := ""
	if result != nil {
		method = string(result.Method)
	}

	// Ordered candidates, most descriptive first.
	var candidates []string
	switch {
	case tag != "" && method != "":
		candidates = append(candidates,
			fmt.Sprintf("%s [%s]", stem, tag),
			fmt.Sprintf("%s [%s %s]", stem, tag, method),
		)
	case tag != "":
		candidates = append(candidates, fmt.Sprintf("%s [%s]", stem, tag))
	case method != "":
		candidates = append(candidates, fmt.Sprintf("%s [%s]", stem, method))
	}
	for _, c := range candidates {
		p := filepath.Join(destDir, sanitizePath(c)+ext)
		if !pathExists(p) {
			return p
		}
	}

	// Numeric counter fallback on the richest stem we have.
	base := stem
	if tag != "" {
		base = sanitizePath(fmt.Sprintf("%s [%s]", stem, tag))
	}
	for i := 2; i < 1000; i++ {
		p := filepath.Join(destDir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if !pathExists(p) {
			return p
		}
	}
	// 998 identical siblings is not a real scenario; keep a stable name.
	return filepath.Join(destDir, base+ext)
}

// versionTag builds a short, human-readable label (e.g. "2160p HDR", "1080p ES")
// from the release title so coexisting downloads get self-describing filenames.
// Returns "" when nothing recognizable is present — the caller then falls back
// to the download method or a numeric counter.
func versionTag(task *Task) string {
	if task == nil {
		return ""
	}
	title := task.Title
	var parts []string
	if m := resTagRegex.FindString(title); m != "" {
		res := strings.ToLower(m)
		if res == "4k" {
			res = "2160p"
		}
		parts = append(parts, res)
	}
	if m := hdrTagRegex.FindString(title); m != "" {
		parts = append(parts, normalizeHDRTag(m))
	}
	if m := langTagRegex.FindString(title); m != "" {
		parts = append(parts, normalizeLangTag(m))
	}
	return strings.Join(parts, " ")
}

// normalizeHDRTag collapses HDR variants to a compact stable label.
func normalizeHDRTag(m string) string {
	s := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(m, ".", " "), "  ", " "))
	switch {
	case s == "dv" || strings.Contains(s, "dolby") || s == "dovi":
		return "DV"
	case s == "hdr10+":
		return "HDR10+"
	case s == "hdr10":
		return "HDR10"
	case s == "hlg":
		return "HLG"
	default:
		return "HDR"
	}
}

// normalizeLangTag maps an audio-language hint to a compact stable label.
func normalizeLangTag(m string) string {
	s := strings.ToLower(m)
	switch {
	case strings.HasPrefix(s, "castellano") || strings.HasPrefix(s, "espa"):
		return "ES"
	case s == "latino":
		return "LAT"
	case strings.HasPrefix(s, "ingl") || s == "english":
		return "EN"
	case s == "dual":
		return "DUAL"
	case s == "multi":
		return "MULTI"
	default:
		return strings.ToUpper(s)
	}
}

// copyFile and moveFile live in movefile.go.
