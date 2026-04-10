package library

import (
	"path/filepath"
	"strings"
)

// ResolveScanPaths returns a deduplicated list of directories to scan.
// Always includes dlDir, moviesDir, tvDir (when non-empty).
// Adds scanPath if non-empty.
// Removes paths that are subdirectories of other paths in the list,
// since a parent walk already covers them.
func ResolveScanPaths(dlDir, moviesDir, tvDir, scanPath string) []string {
	raw := make([]string, 0, 4)
	for _, p := range []string{dlDir, moviesDir, tvDir, scanPath} {
		if p != "" {
			raw = append(raw, filepath.Clean(p))
		}
	}
	return deduplicatePaths(raw)
}

// deduplicatePaths removes duplicate paths and paths that are subdirectories
// of another path already present in the list.
func deduplicatePaths(paths []string) []string {
	// Remove exact duplicates first.
	seen := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}

	// Remove paths that are subdirs of another path in the list.
	result := make([]string, 0, len(unique))
	for _, p := range unique {
		isChild := false
		for _, other := range unique {
			if other == p {
				continue
			}
			rel, err := filepath.Rel(other, p)
			if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				isChild = true
				break
			}
		}
		if !isChild {
			result = append(result, p)
		}
	}
	return result
}
