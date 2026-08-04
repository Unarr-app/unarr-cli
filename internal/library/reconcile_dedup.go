package library

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// videoFile pairs a path with its size for the dedup grouping.
type videoFile struct {
	path string
	size int64
}

// findDuplicateVideos detects byte-identical copies of the same video within a
// single directory and flags all but one canonical copy for removal (RC-8).
//
// Cheap by design: it groups candidates by EXACT size first and only fingerprints
// (ComputeFingerprint = sha256 of size‖first1MiB‖last1MiB, the server's scheme)
// when ≥2 videos in a dir share a size — a healthy library with unique files never
// hashes anything. Files whose fingerprints DIFFER are a real upgrade (2160p next
// to 1080p that happen to match on size — vanishingly rare, but handled): they are
// left alone. The kept copy is the most canonical name (no "(N)", "[torrent]",
// "[debrid]", "[usenet]" suffix); the rest are flagged KindDuplicate.
//
// Returns the findings and the set of paths flagged (for callers/tests).
func findDuplicateVideos(roots []string, floor int64) ([]Finding, map[string]bool) {
	byDirSize := collectVideosByDirSize(roots, floor)

	var findings []Finding
	flagged := map[string]bool{}

	for _, sizes := range byDirSize {
		for _, group := range sizes {
			if len(group) < 2 {
				continue // a unique size can't be a duplicate
			}
			findings = append(findings, dedupGroup(group, flagged)...)
		}
	}
	return findings, flagged
}

// collectVideosByDirSize walks the roots and buckets real videos (>= floor) by
// their directory and exact size. Only same-dir, same-size candidates can be
// byte-identical duplicates, so this is the cheap pre-filter before any hashing.
func collectVideosByDirSize(roots []string, floor int64) map[string]map[int64][]videoFile {
	// dir -> size -> []videoFile
	byDirSize := map[string]map[int64][]videoFile{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isVideoFile(path) {
				return nil
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() < floor {
				return nil // stubs are handled by classifyFile, not here
			}
			dir := filepath.Dir(path)
			if byDirSize[dir] == nil {
				byDirSize[dir] = map[int64][]videoFile{}
			}
			byDirSize[dir][info.Size()] = append(byDirSize[dir][info.Size()], videoFile{path, info.Size()})
			return nil
		})
	}
	return byDirSize
}

// dedupGroup fingerprints a same-size group as a cheap FILTER, then CONFIRMS
// byte-for-byte identity with a full compare before flagging any copy for removal.
// The fingerprint (size + first/last 1 MiB) can collide for files that differ only
// in the middle; for an unconfirmed AUTO-delete that optimism is data loss, so the
// full SameFileContent check is the real proof. One canonical copy per confirmed
// identical set is kept; the rest are flagged.
func dedupGroup(group []videoFile, flagged map[string]bool) []Finding {
	// fingerprint -> paths (candidate buckets — same extremes, maybe identical)
	byFP := map[string][]string{}
	for _, vf := range group {
		fp, err := ComputeFingerprint(vf.path, vf.size)
		if err != nil {
			// Can't even fingerprint → never delete. Log and skip this file.
			log.Printf("reconcile: fingerprint %s failed, not deduping: %v", vf.path, err)
			continue
		}
		byFP[fp] = append(byFP[fp], vf.path)
	}

	var findings []Finding
	for _, paths := range byFP {
		if len(paths) < 2 {
			continue // only one file with these extremes — not a duplicate
		}
		findings = append(findings, confirmAndFlagDuplicates(paths, flagged)...)
	}
	return findings
}

// confirmAndFlagDuplicates keeps the canonical copy and flags every OTHER file
// that a full byte compare confirms is identical to it. A fingerprint-collision
// (extremes match, middle differs) fails the compare and is left untouched.
func confirmAndFlagDuplicates(paths []string, flagged map[string]bool) []Finding {
	keep := pickCanonical(paths)
	var findings []Finding
	for _, p := range paths {
		if p == keep || flagged[p] {
			continue
		}
		same, err := SameFileContent(keep, p)
		if err != nil {
			log.Printf("reconcile: full compare %s vs %s failed, not deduping: %v", keep, p, err)
			continue
		}
		if !same {
			// Fingerprints matched but the bytes differ (collision on the extremes).
			// Do NOT delete — this is a genuinely different file.
			log.Printf("reconcile: %s shares a fingerprint with %s but differs on full compare - keeping both", p, keep)
			continue
		}
		flagged[p] = true
		info, _ := os.Stat(p)
		var bytes, apparent int64
		if info != nil {
			bytes = diskUsage(info) // real on-disk usage freed by removing the dup
			apparent = info.Size()
		}
		findings = append(findings, Finding{
			Path:     p,
			Kind:     KindDuplicate,
			Reason:   "byte-identical copy of " + filepath.Base(keep) + " (confirmed by full compare)",
			Bytes:    bytes,
			Apparent: apparent,
			IsDir:    false,
		})
	}
	return findings
}

// dupSuffixRegex matches the version-tagging suffixes versionDistinctPath adds
// (" (2)", " [torrent]", " [1080p ES]", …) just before the extension. The copy
// WITHOUT such a suffix is the canonical one to keep.
var dupSuffixRegex = regexp.MustCompile(`(?i)\s*(\(\d+\)|\[[^\]]+\])\s*$`)

// pickCanonical chooses which identical copy to keep: prefer the name with no
// version/counter suffix; tie-break on the shortest, then lexicographically
// smallest name so the choice is deterministic (idempotent across runs).
func pickCanonical(paths []string) string {
	best := paths[0]
	bestStem := suffixlessStem(best)
	for _, p := range paths[1:] {
		stem := suffixlessStem(p)
		pName := filepath.Base(p)
		bName := filepath.Base(best)
		switch {
		case !stem.tagged && bestStem.tagged:
			best, bestStem = p, stem // untagged beats tagged
		case stem.tagged != bestStem.tagged:
			// best is untagged, p tagged → keep best
		case len(pName) < len(bName):
			best, bestStem = p, stem
		case len(pName) == len(bName) && pName < bName:
			best, bestStem = p, stem
		}
	}
	return best
}

type stemInfo struct {
	tagged bool // whether the name carried a version/counter suffix
}

func suffixlessStem(path string) stemInfo {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return stemInfo{tagged: dupSuffixRegex.MatchString(stem)}
}
