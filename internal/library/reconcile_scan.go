package library

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// This file holds reconcile's directory/video scanning helpers — the sidecar
// ownership check, the video-less-dir sweep, and the shared video-file predicate.
// Split out of reconcile.go to keep each file single-responsibility and under the
// architectural line limit.

// sidecarHasOwner decides whether a sidecar still belongs to a video. A sidecar is
// owned if a real video (>= floor) lives in ITS OWN directory OR — when the sidecar
// sits inside a per-track ".unarr" cache dir — in the PARENT release directory.
//
// The .unarr case is why the naive "video in same dir" check produced 107 false
// orphans in the field: the scanner extracts per-track WebVTT/thumbnails into
// "<release>/.unarr/*.vtt", where there is no video beside them. Those are only
// orphaned when the release itself no longer holds the video (it was moved to TV
// Shows and just .nfo/.txt/.unarr remain).
func sidecarHasOwner(path string, floor int64) bool {
	dir := filepath.Dir(path)
	if dirHasImmediateVideo(dir, floor) {
		return true
	}
	// Per-track sidecars live in a ".unarr" cache dir — fall back to the parent
	// release dir, which is where the actual media file lives.
	if strings.EqualFold(filepath.Base(dir), ".unarr") {
		if dirHasImmediateVideo(filepath.Dir(dir), floor) {
			return true
		}
	}
	return false
}

// dirHasImmediateVideo reports whether a real video (>= floor) sits directly in dir
// (non-recursive). On a read error it returns true (conservative: don't flag a
// sidecar we can't prove is orphaned).
func dirHasImmediateVideo(dir string, floor int64) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("reconcile: read %s failed, treating sidecars as attached: %v", dir, err)
		return true
	}
	for _, e := range entries {
		if e.IsDir() || !isVideoFile(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() >= floor {
			return true
		}
	}
	return false
}

// findVideolessDirs returns dirs (strictly inside a root, never a CONFIGURED dir
// itself, never already-flagged) that contain no real video anywhere beneath them.
//
// "Configured dir" means any of the reconcile roots (download/movies/tv). Guarding
// only `path == root` of the current walk is NOT enough: when the download dir is
// the PARENT of the movies/tv dirs (e.g. download=/media, movies=/media/Movies),
// the movies/tv dirs are ordinary sub-entries of the download walk, so an empty
// library would have its Movies/ and TV Shows/ target dirs deleted — a real bug the
// e2e caught (the daemon auto-sweep removed both on a fresh empty library). These
// dirs are organize TARGETS and must survive even when momentarily empty.
func findVideolessDirs(roots []string, skip map[string]bool, floor int64) []Finding {
	protected := map[string]bool{}
	for _, r := range roots {
		protected[filepath.Clean(r)] = true
	}
	var out []Finding
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || path == root || protected[filepath.Clean(path)] || skip[path] {
				return nil
			}
			// A ".unarr" cache dir legitimately holds only sidecars for the video in
			// its PARENT release dir — it is not a videoless orphan when that parent
			// still has the video. (Orphaned .unarr sidecars are handled per-file by
			// sidecarHasOwner, which removes them and lets the empty .unarr get pruned.)
			if strings.EqualFold(d.Name(), ".unarr") && dirHasRealVideo(filepath.Dir(path), floor) {
				return filepath.SkipDir
			}
			if dirHasRealVideo(path, floor) {
				return nil
			}
			out = append(out, dirFinding(path, KindEmptyDir,
				"directory contains no valid video (empty or only junk/stubs)"))
			skip[path] = true
			return filepath.SkipDir // its (also video-less) children are covered by removing this
		})
	}
	return out
}

// dirHasRealVideo reports whether a real video (>= floor) exists anywhere under dir.
func dirHasRealVideo(dir string, floor int64) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isVideoFile(path) {
			if info, statErr := os.Stat(path); statErr == nil && info.Size() >= floor {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// isVideoFile mirrors engine.isVideoFile using the library's videoExts set (from
// scanner.go). Kept here so reconcile does not depend on the engine package.
func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}
