package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	yearRegex   = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	seasonRegex = regexp.MustCompile(`(?i)S(\d{2})`)
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

	// Detect season for TV
	var season string
	if m := seasonRegex.FindStringSubmatch(result.FileName); len(m) > 1 {
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
