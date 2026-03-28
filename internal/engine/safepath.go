package engine

import (
	"fmt"
	"path/filepath"
	"strings"
)

// isWithinDir checks that resolved is a child of baseDir (prevents path traversal).
// Both paths must be absolute and clean.
func isWithinDir(baseDir, resolved string) bool {
	base := filepath.Clean(baseDir)
	target := filepath.Clean(resolved)
	return target == base || strings.HasPrefix(target, base+string(filepath.Separator))
}

// safePath constructs a path under baseDir and validates it doesn't escape.
// Returns an error if the resulting path is outside baseDir.
// If the resulting path exists and is a symlink that resolves outside baseDir,
// it is also rejected.
func safePath(baseDir, untrusted string) (string, error) {
	resolved := filepath.Join(baseDir, untrusted) // Join already cleans

	if !isWithinDir(baseDir, resolved) {
		return "", fmt.Errorf("path traversal blocked: %q escapes %q", untrusted, baseDir)
	}

	// Resolve symlinks if the path already exists on disk
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		if !isWithinDir(baseDir, real) {
			return "", fmt.Errorf("path traversal blocked: %q resolves outside %q via symlink", untrusted, baseDir)
		}
		return real, nil
	}

	return resolved, nil
}
