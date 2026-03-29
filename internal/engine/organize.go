package engine

import (
	"fmt"
	"io"
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
)

// OrganizeConfig holds file organization settings.
type OrganizeConfig struct {
	Enabled    bool
	MoviesDir  string
	TVShowsDir string
}

// organize moves a downloaded file into the proper directory structure.
// Movies: MoviesDir/Title (Year)/filename.ext
// TV:     TVShowsDir/Title/Season XX/filename.ext
func organize(result *Result, task *Task, cfg OrganizeConfig) (string, error) {
	if !cfg.Enabled || result == nil || result.FilePath == "" {
		return result.FilePath, nil
	}

	title := task.Title
	if title == "" {
		title = result.FileName
	}

	isTV := strings.Contains(strings.ToLower(task.PreferredMethod), "show") ||
		seasonRegex.MatchString(result.FileName)

	// Detect season for TV (S01E05 or 1x05 format)
	var season string
	if m := episodeRegex.FindStringSubmatch(result.FileName); len(m) > 2 {
		season = m[1]
		isTV = true
	} else if m := altEpRegex.FindStringSubmatch(result.FileName); len(m) > 2 {
		season = fmt.Sprintf("%02s", m[1])
		isTV = true
	} else if m := seasonRegex.FindStringSubmatch(result.FileName); len(m) > 1 {
		season = m[1]
		isTV = true
	}

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
		return result.FilePath, nil // no organize dirs configured
	}

	// Validate destination is within the expected base directory
	var baseDir string
	if isTV && cfg.TVShowsDir != "" {
		baseDir = cfg.TVShowsDir
	} else {
		baseDir = cfg.MoviesDir
	}
	if !isWithinDir(baseDir, destDir) {
		return "", fmt.Errorf("path traversal blocked: %q escapes %q", destDir, baseDir)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}

	destPath := filepath.Join(destDir, filepath.Base(result.FilePath))

	// Check if source is a directory (multi-file torrent)
	srcInfo, err := os.Stat(result.FilePath)
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}

	if srcInfo.IsDir() {
		// For directories: remove existing destination if present, then rename
		if _, err := os.Stat(destPath); err == nil {
			os.RemoveAll(destPath)
		}
		if err := os.Rename(result.FilePath, destPath); err != nil {
			return "", fmt.Errorf("move directory: %w", err)
		}
		return destPath, nil
	}

	// Try rename first (same filesystem), fall back to copy+delete
	if err := os.Rename(result.FilePath, destPath); err != nil {
		if err := copyFile(result.FilePath, destPath); err != nil {
			return "", fmt.Errorf("move file: %w", err)
		}
		os.Remove(result.FilePath)
	}

	return destPath, nil
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
