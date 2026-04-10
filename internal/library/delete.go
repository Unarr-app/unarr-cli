package library

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/torrentclaw/unarr/internal/agent"
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
// Returns the IDs of items successfully deleted.
func DeleteFiles(items []agent.LibraryDeleteRequest, scanPaths []string) []int {
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
		return nil
	}

	confirmed := make([]int, 0, len(items))

	for _, item := range items {
		if err := deleteOne(item.FilePath, safe); err != nil {
			log.Printf("library: delete item %d (%q): %v", item.ItemID, item.FilePath, err)
			continue
		}
		log.Printf("library: deleted item %d: %s", item.ItemID, item.FilePath)
		confirmed = append(confirmed, item.ItemID)
	}

	return confirmed
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
			// File already gone — idempotent success.
			pruneEmptyDirs(filepath.Dir(clean), scanPaths)
			return nil
		}
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	// Security: resolved file must be within one of the configured scan paths.
	if !isWithinScanPaths(real, scanPaths) {
		return fmt.Errorf("path %q (resolved: %q) is outside all configured scan paths — refusing to delete", clean, real)
	}

	// Remove the file (idempotent: not-exist is not an error).
	if err := os.Remove(real); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}

	// Clean up empty parent directories, stopping at the scan path root.
	pruneEmptyDirs(filepath.Dir(real), scanPaths)

	return nil
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
