package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SetDownloadDir moves downloads to dir and carries along what pointed at the
// old one. `unarr init` sets movies_dir/tv_shows_dir to <download dir>/Movies
// and /TV Shows, so changing only [downloads] dir kept moving every finished
// movie and episode back to the old drive — exactly what a user moving off a
// full C: is trying to stop. Folders the user pointed elsewhere are left alone.
//
// The old dir is also kept as library.scan_path when that is free: scan roots
// derive from the download dir, so without it everything already downloaded
// would silently drop out of the library on the next scan.
func (c *Config) SetDownloadDir(dir string) {
	old := c.Download.Dir
	c.Download.Dir = dir
	if old == "" {
		return
	}
	c.Organize.MoviesDir = rebaseUnder(c.Organize.MoviesDir, old, dir)
	c.Organize.TVShowsDir = rebaseUnder(c.Organize.TVShowsDir, old, dir)
	if c.Library.ScanPath == "" {
		c.Library.ScanPath = old
	}
}

// DriveRootError rejects a whole Windows drive (E:\) as a download folder. Only
// for places where a user picks a NEW folder (tray, config menu): the daemon's
// startup gate (ValidatePaths) refuses just C:\ and D:\, and widening that would
// stop agents already running on another drive's root from starting.
func DriveRootError(label, dir, goos string) error {
	dir = strings.TrimSpace(dir)
	if goos != "windows" || !isDriveRoot(dir) {
		return nil
	}
	return driveRootMessage(label, dir)
}

func driveRootMessage(label, root string) error {
	root = root[:2] + `\`
	return fmt.Errorf("%s: %s is a whole drive — use a folder on it, e.g. %sDownloads", label, root, root)
}

// isDriveRoot matches `E:\`, `E:/` and `E:`. Hand-rolled rather than
// filepath.VolumeName so it is testable from any GOOS.
func isDriveRoot(p string) bool {
	if len(p) < 2 || len(p) > 3 || p[1] != ':' || !isDriveLetter(p[0]) {
		return false
	}
	return len(p) == 2 || p[2] == '\\' || p[2] == '/'
}

// rebaseUnder returns p re-rooted from oldRoot to newRoot when p is strictly
// inside oldRoot, and p unchanged otherwise. A leading ~ on either side is
// expanded first: config.toml can hold a hand-written `~/Media`, and mixing it
// with an absolute path would make filepath.Rel fail and skip the move.
func rebaseUnder(p, oldRoot, newRoot string) string {
	if p == "" {
		return p
	}
	rel, err := filepath.Rel(filepath.Clean(expandTilde(oldRoot)), filepath.Clean(expandTilde(p)))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(newRoot, rel)
}

func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
