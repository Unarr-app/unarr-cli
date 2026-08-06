package postprocess

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExtractDirResult reports what ExtractInDir did.
type ExtractDirResult struct {
	// Extracted is true when an archive was found AND unpacked.
	Extracted bool
	// Files lists the extracted files. Empty when Extracted is false.
	Files []string
	// Note is non-empty when an archive was present but could NOT be unpacked
	// for a recoverable reason (no extractor installed). The caller keeps the
	// raw payload and surfaces this, rather than failing the download — the
	// user still gets the .rNN files and can unpack them by hand.
	Note string
}

// ExtractInDir unpacks a RAR/split archive sitting in dir, when there is one.
//
// It exists because Process() — the usenet post-processing pipeline — cannot be
// reused for torrents: its signature is usenet-native (a segment→path map from
// the NNTP downloader, plus lazy par2 fetching), and its par2 steps are
// meaningless for a torrent, which arrives as a plain directory on disk.
// ExtractInDir is the archive half of that pipeline, addressed by directory,
// so both download methods share the extractor without sharing usenet
// semantics.
//
// Contract: it is a NO-OP when the release ships no archive (the common case —
// most torrents are already a plain .mkv), so callers may invoke it
// unconditionally before organizing.
//
// A missing extractor binary is NOT an error: unlike usenet — where the payload
// is USELESS unextracted, since the archive IS the delivery format — a torrent's
// raw .rNN files are still what the user was given. Failing the download there
// would turn "cannot improve this" into "you get nothing". Reported via Note.
func ExtractInDir(dir string, password string) (*ExtractDirResult, error) {
	res := &ExtractDirResult{}

	archive := findFirstRarInDir(dir)
	if archive == "" {
		return res, nil // no archive: nothing to do, not an error
	}

	if _, extPath := FindExtractor(); extPath == "" {
		res.Note = fmt.Sprintf("archive %s left packed: no extractor found (install unrar or 7z)", filepath.Base(archive))
		log.Printf("[extract] WARNING: %s", res.Note)
		return res, nil
	}

	if password == "" && IsPasswordProtected(archive) {
		return nil, &PasswordError{Archive: archive}
	}

	log.Printf("[extract] extracting archive: %s", filepath.Base(archive))
	files, err := Extract(archive, dir, password)
	if err != nil {
		return nil, err // includes *PasswordError — the caller distinguishes it
	}

	res.Extracted = true
	res.Files = files
	return res, nil
}

// CleanupArchives removes the archive parts and parity/junk left behind after a
// successful extraction. Separate from ExtractInDir so a caller can inspect the
// extraction before destroying the source.
func CleanupArchives(dir string) error {
	return Cleanup(dir)
}

// findFirstRarInDir is findFirstRar addressed by directory instead of by the
// usenet segment map. Same priority order (explicit .part01 → shortest .rar →
// .001 split) so both download methods pick the same entry volume.
//
// Only the top level is scanned: a scene release keeps its volumes beside the
// .nfo, and descending would risk picking an archive out of a nested extras
// folder as if it were the main payload.
func findFirstRarInDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}

	// Priority 1: explicitly named first part (.part01.rar / .part1.rar).
	for _, name := range names {
		if firstRarRe.MatchString(name) {
			return filepath.Join(dir, name)
		}
	}

	// Priority 2: shortest-named .rar (usually the first volume). Skip any
	// .partNN.rar here — if a part-numbered set exists without a part01, its
	// entry volume is missing and unrar would fail on a middle part anyway.
	var rars []string
	for _, name := range names {
		if !strings.HasSuffix(strings.ToLower(name), ".rar") {
			continue
		}
		if partRarRe.MatchString(name) {
			continue
		}
		rars = append(rars, name)
	}
	if len(rars) > 0 {
		sort.Slice(rars, func(i, j int) bool { return len(rars[i]) < len(rars[j]) })
		return filepath.Join(dir, rars[0])
	}

	// Priority 3: .001 split format.
	for _, name := range names {
		if strings.HasSuffix(strings.ToLower(name), ".001") {
			return filepath.Join(dir, name)
		}
	}

	return ""
}
