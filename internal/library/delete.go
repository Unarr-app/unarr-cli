package library

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// DeleteFiles deletes the given library items from disk and cleans up empty
// parent directories within the configured scan paths.
//
// Safety rules (all must pass before os.Remove is called):
//  1. filePath must be an absolute path.
//  2. filePath must be within one of the configured scanPaths.
//  3. Empty parent directories are removed up to (but not including) the
//     scan path root and only if they are not the scan path itself.
//
// Returns the IDs successfully deleted, plus the ones that genuinely FAILED
// (ours, but undeletable: permission, I/O). A failure MUST be reported, never
// just logged: the server keeps re-sending an unconfirmed deletion and the web
// shows a spinner until someone reads the agent's log.
//
// A path outside our scan paths is NEITHER: it is another agent's file, so we
// stay silent and let its owner handle it. Reporting it as failed would clear the
// server's pending flag and the owner would never be handed the deletion —
// one agent answering for a file it was never going to touch.
func DeleteFiles(items []agent.LibraryDeleteRequest, scanPaths []string) ([]int, []agent.LibraryDeleteError) {
	// Sanitize scan paths: reject empty or non-absolute entries.
	safe := make([]string, 0, len(scanPaths))
	for _, sp := range scanPaths {
		if filepath.IsAbs(sp) {
			safe = append(safe, sp)
		} else {
			log.Printf("library: ignoring non-absolute scan path: %q", sp)
		}
	}
	if len(safe) == 0 {
		log.Printf("library: no valid scan paths configured — refusing to delete")
		failed := make([]agent.LibraryDeleteError, 0, len(items))
		for _, item := range items {
			failed = append(failed, agent.LibraryDeleteError{
				ID:    item.ItemID,
				Error: "agent has no valid scan paths configured",
			})
		}
		return nil, failed
	}

	confirmed := make([]int, 0, len(items))
	failed := make([]agent.LibraryDeleteError, 0)

	for _, item := range items {
		// Not our file → say nothing, so the owning agent still gets it.
		if filepath.IsAbs(item.FilePath) && !isWithinScanPaths(filepath.Clean(item.FilePath), safe) {
			log.Printf("library: skipping item %d (%q): not within this agent's scan paths", item.ItemID, item.FilePath)
			continue
		}
		if err := deleteOne(item.FilePath, safe); err != nil {
			log.Printf("library: delete item %d (%q): %v", item.ItemID, item.FilePath, err)
			failed = append(failed, agent.LibraryDeleteError{ID: item.ItemID, Error: err.Error()})
			continue
		}
		log.Printf("library: deleted item %d: %s", item.ItemID, item.FilePath)
		confirmed = append(confirmed, item.ItemID)
	}

	return confirmed, failed
}

func deleteOne(filePath string, scanPaths []string) error {
	if !filepath.IsAbs(filePath) {
		return fmt.Errorf("path is not absolute: %q", filePath)
	}

	clean := filepath.Clean(filePath)

	// Resolve symlinks before validation to prevent traversal via symlinks.
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if os.IsNotExist(err) {
			// File already gone. Only OUR missing file is an idempotent success:
			// a path outside our scan paths is another agent's file, and
			// confirming it would tombstone a row whose file is alive elsewhere.
			if !isWithinScanPaths(clean, scanPaths) {
				return fmt.Errorf("path %q is outside all configured scan paths — not this agent's file", clean)
			}
			pruneEmptyDirs(filepath.Dir(clean), scanPaths)
			return nil
		}
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	// Security: resolved file must be within one of the configured scan paths.
	// The roots are resolved too so the comparison is like-for-like: a scan path
	// that merely SITS behind a symlink (macOS /var → /private/var, Windows 8.3
	// short names) is mount indirection, not an escape. Same rule as
	// confinedForRemoval in reconcile_errors.go.
	roots := resolvedRoots(scanPaths)
	if !isWithinScanPaths(real, roots) {
		return fmt.Errorf("path %q (resolved: %q) is outside all configured scan paths — refusing to delete", clean, real)
	}

	// Remove the file (idempotent: not-exist is not an error).
	if err := os.Remove(real); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}

	// Remove the video's sidecars (subtitles, .nfo, posters, .par2, etc.) that
	// share its basename BEFORE pruning — otherwise pruneEmptyDirs never fires (the
	// dir still holds the orphaned .srt/.nfo) and the library keeps a folder full of
	// dead metadata for a video that's gone.
	deleteSidecars(real)

	// Clean up empty parent directories, stopping at the scan path root. `real` is
	// resolved, so the roots must be too or the walk stops on the first comparison.
	pruneEmptyDirs(filepath.Dir(real), roots)

	return nil
}

// sidecarExts are the non-video companion files a library item accretes: subtitle
// tracks, metadata, artwork, and par2 recovery. A file in the same directory whose
// name starts with the video's basename-sans-ext AND carries one of these
// extensions belongs to that video and is deleted with it. Mirrors organize.go's
// moveSubtitles prefix-match, extended past subtitles to the full sidecar set.
var sidecarExts = map[string]bool{
	".srt": true, ".sub": true, ".ass": true, ".ssa": true, ".vtt": true,
	".idx": true, ".nfo": true, ".jpg": true, ".jpeg": true, ".png": true,
	".par2": true,
}

// deleteSidecars removes companion files of videoPath in the same directory:
// files whose name begins with the video's basename-sans-ext (e.g.
// "Movie (2023).es.srt", "Movie (2023).nfo", "Movie (2023)-poster.jpg") and whose
// extension is a known sidecar type. It NEVER deletes another video (a different
// feature/episode filed in the same folder), only metadata/subtitle/artwork.
// Best-effort: a sidecar that can't be removed is logged, never silently dropped,
// but doesn't fail the primary deletion (the video is already gone).
func deleteSidecars(videoPath string) {
	dir := filepath.Dir(videoPath)
	base := filepath.Base(videoPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("library: sidecar scan of %s failed: %v", dir, err)
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == base {
			continue // the video itself (already removed)
		}
		ext := strings.ToLower(filepath.Ext(name))
		// Only same-title companions of a known sidecar type. Never touch another
		// video, even if its name happens to share the prefix.
		if videoExts[ext] {
			continue
		}
		if !sidecarExts[ext] {
			continue
		}
		// Boundary-aware prefix match — see SidecarBelongsTo. A bare
		// strings.HasPrefix(name, stem) matched "Movie Extended.srt" (the subtitle
		// of a DIFFERENT, still-present video "Movie Extended.mkv") and deleted it.
		if !SidecarBelongsTo(name, stem) {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("library: failed to remove sidecar %s: %v", p, err)
			continue
		}
		log.Printf("library: removed sidecar %s", p)
	}
}

// SidecarBelongsTo reports whether a companion file named `name` belongs to the
// video whose basename-sans-ext is `stem`. A sidecar belongs iff, after the stem,
// the next character is a SEPARATOR the naming conventions use:
//   - "." for language/type chains: "Movie.srt", "Movie.es.srt", "Movie.forced.eng.srt", "Movie.nfo"
//   - "-" for artwork: "Movie-poster.jpg", "Movie-fanart.jpg"
//
// Requiring a separator draws the title boundary a bare prefix match lacked:
// "Movie Extended.srt" continues with a SPACE (it is a different title,
// "Movie Extended"), so it is correctly excluded and its video's subtitle survives.
//
// Exported so the organizer (engine.moveSubtitles) shares the SAME boundary rule
// instead of duplicating a prefix match — both had the unbounded-prefix bug.
func SidecarBelongsTo(name, stem string) bool {
	if !strings.HasPrefix(name, stem) {
		return false
	}
	rest := name[len(stem):]
	return strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "-")
}

// isWithinScanPaths returns true if p is a child of any scan path.
func isWithinScanPaths(p string, scanPaths []string) bool {
	for _, sp := range scanPaths {
		sp = filepath.Clean(sp)
		rel, err := filepath.Rel(sp, p)
		if err != nil {
			continue
		}
		// rel must not be "." (exact match = root itself) and must not start with ".."
		if rel != "." && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

// pruneEmptyDirs walks upward from dir, removing empty directories until it
// reaches a scan path root (which is never removed).
// Max 10 levels to guard against infinite loops on unexpected path shapes.
func pruneEmptyDirs(dir string, scanPaths []string) {
	const maxLevels = 10
	for i := 0; i < maxLevels; i++ {
		dir = filepath.Clean(dir)

		// Single pass: stop if dir is a scan root or outside all scan paths.
		if !dirEligibleForPrune(dir, scanPaths) {
			return
		}

		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return // non-empty or unreadable — stop
		}

		if err := os.Remove(dir); err != nil {
			log.Printf("library: prune dir %s: %v", dir, err)
			return
		}
		log.Printf("library: removed empty dir: %s", dir)

		dir = filepath.Dir(dir)
	}
}

// dirEligibleForPrune returns true if dir is a strict child of any scan path
// (i.e. it is inside a scan path but is not the scan root itself).
// Combines the former isScanPathRoot + isWithinScanPaths checks into one loop.
func dirEligibleForPrune(dir string, scanPaths []string) bool {
	for _, sp := range scanPaths {
		sp = filepath.Clean(sp)
		if sp == dir {
			return false // dir IS the scan root — never remove it
		}
		rel, err := filepath.Rel(sp, dir)
		if err != nil {
			continue
		}
		if rel != "." && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
