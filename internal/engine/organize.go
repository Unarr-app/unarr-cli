package engine

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

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

	return moveToDir(result, destDir, destFileName, cfg)
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

	return moveToDir(result, destDir, "", cfg)
}

// moveToDir handles the actual directory creation and file move, including path traversal check.
// If destFileName is non-empty, the file is renamed to that name (instead of keeping the original).
func moveToDir(result *Result, destDir, destFileName string, cfg OrganizeConfig) (string, error) {
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

	if srcInfo.IsDir() {
		if _, err := os.Stat(destPath); err == nil {
			os.RemoveAll(destPath)
		}
		if err := os.Rename(result.FilePath, destPath); err != nil {
			return "", fmt.Errorf("move directory: %w", err)
		}
		return destPath, nil
	}

	if err := os.Rename(result.FilePath, destPath); err != nil {
		if err := copyFile(result.FilePath, destPath); err != nil {
			return "", fmt.Errorf("move file: %w", err)
		}
		os.Remove(result.FilePath)
	}

	// Move subtitle files alongside the video
	moveSubtitles(result.FilePath, destDir, destFileName)

	// Clean up the source torrent directory if it's a subdirectory of OutputDir
	// and now empty or only contains junk files (nfo, txt, url, etc.)
	cleanupSourceDir(result.FilePath, cfg.OutputDir)

	return destPath, nil
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

// isVideoFile checks if a filename has a common video extension.
func isVideoFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mkv", ".mp4", ".avi", ".wmv", ".mov", ".flv", ".webm", ".m4v", ".ts", ".m2ts":
		return true
	}
	return false
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
		// Match: subtitle must start with the video base name
		// e.g., "Movie.srt", "Movie.en.srt", "Movie.forced.eng.srt"
		if !strings.HasPrefix(e.Name(), videoBase) {
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

		if err := os.Rename(subSrc, destPath); err != nil {
			if err := copyFile(subSrc, destPath); err != nil {
				log.Printf("[organize] warning: failed to move subtitle %s: %v", e.Name(), err)
				continue
			}
			os.Remove(subSrc)
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

	if err := os.Rename(oldPath, backupPath); err != nil {
		// Cross-device: copy + delete
		if err := copyFile(oldPath, backupPath); err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
		os.Remove(oldPath)
	}

	// Move new file to old path
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	if err := os.Rename(newPath, oldPath); err != nil {
		// Cross-device: copy + delete
		if err := copyFile(newPath, oldPath); err != nil {
			// Rollback: restore backup
			os.Rename(backupPath, oldPath)
			return fmt.Errorf("replace failed: %w", err)
		}
		os.Remove(newPath)
	}

	return nil
}

func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return err
}
