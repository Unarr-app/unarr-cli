package engine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// usenetTaskDir is the folder a usenet release is assembled in: the task title
// under outputDir. The title is often a FILE name — a torrent that fell back to
// usenet is named after its single file, "Film (2018).mp4" — and that file may
// already sit in outputDir, written by the torrent attempt. MkdirAll over it
// fails with "not a directory", which read as an unmounted drive and failed the
// task as a storage outage (2026-09-23). So a title taken by a non-directory
// gets the task's short id appended. Deterministic: a resume lands in the same
// folder as long as the blocking file is still there, and an existing folder is
// always reused as is.
func usenetTaskDir(outputDir, title, shortID string) string {
	dir := filepath.Join(outputDir, sanitizeDir(title))
	if _, err := os.Lstat(dir); err != nil {
		return dir // free
	}
	// Taken. Stat follows links: a symlink to a folder is a folder and is
	// reused; a file, or a link to nothing, is not.
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return dir + " (" + shortID + ")"
}

// isNameClash reports whether a MkdirAll error is a path taken by a file
// inside a healthy outputDir (os.MkdirAll returns ENOTDIR for that on every
// platform). That is a naming problem, not an outage, and must not trigger the
// storage retry/cooldown that blocks re-dispatch. Anything else — permission
// denied on a read-only mount, an outputDir that vanished — stays a storage
// failure.
func isNameClash(err error, outputDir string) bool {
	if !errors.Is(err, syscall.ENOTDIR) {
		return false
	}
	fi, statErr := os.Stat(outputDir)
	return statErr == nil && fi.IsDir()
}
