package postprocess

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// findExtractorFn is an indirection over FindExtractor so the no-extractor path
// is testable on a machine that HAS unrar/7z installed (same pattern as
// par2VerifyFn). Without it that branch could only be covered by skipping the
// test everywhere it matters — CI included.
var findExtractorFn = FindExtractor

// ExtractDirResult reports what ExtractInDir did.
type ExtractDirResult struct {
	// Extracted is true when an archive was found AND unpacked.
	Extracted bool
	// Files lists the extracted files. Empty when Extracted is false.
	Files []string
	// Note is non-empty when an archive was present but could NOT be unpacked
	// for a recoverable reason (no extractor installed, or an extractor that
	// produced nothing). The caller keeps the raw payload and surfaces this,
	// rather than failing the download — the user still gets the .rNN files and
	// can unpack them by hand.
	Note string

	// archiveParts are the volumes of the set that was unpacked. Unexported so
	// only CleanupArchives can act on it: the deletable set is decided here,
	// where the archive was identified, instead of re-derived by a caller that
	// would have to guess which files in the directory belonged to it.
	archiveParts []string
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
	return ExtractInDirTo(dir, dir, password)
}

// ExtractInDirTo is ExtractInDir with the output written to destDir instead of
// beside the archive.
//
// A seeding torrent must keep serving the EXACT bytes it downloaded, and its
// directory belongs to the swarm — so nothing may be added to it (a stray file
// makes organize's cleanup pass judge the directory differently) nor removed
// from it. Extracting to a sibling directory leaves the torrent bit-for-bit
// intact while still producing a playable file for the library.
//
// destDir == dir reproduces the in-place behaviour, which is what the
// non-seeding path wants: there the parts are deleted right after, so a sibling
// would only add a pointless cross-directory move.
func ExtractInDirTo(dir, destDir string, password string) (*ExtractDirResult, error) {
	res := &ExtractDirResult{}

	archive := findFirstRarInDir(dir)
	if archive == "" {
		return res, nil // no archive: nothing to do, not an error
	}

	if _, extPath := findExtractorFn(); extPath == "" {
		res.Note = fmt.Sprintf("archive %s left packed: no extractor found (install unrar or 7z)", filepath.Base(archive))
		log.Printf("[extract] WARNING: %s", res.Note)
		return res, nil
	}

	if password == "" && IsPasswordProtected(archive) {
		return nil, &PasswordError{Archive: archive}
	}

	if destDir != dir {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return nil, fmt.Errorf("create extract dir: %w", err)
		}
	}

	log.Printf("[extract] extracting archive: %s", filepath.Base(archive))
	files, err := Extract(archive, destDir, password)
	if err != nil {
		return nil, err // includes *PasswordError — the caller distinguishes it
	}

	// An extractor that exits 0 having produced NOTHING is not a success.
	// listExtractedFiles reports the directory minus the archive parts, so a
	// release that already held a loose video would come back non-empty even if
	// the archive yielded nothing — and the caller would then delete "leftovers"
	// that were never leftovers. Require real output before claiming extraction.
	if len(files) == 0 {
		res.Note = fmt.Sprintf("archive %s produced no files", filepath.Base(archive))
		log.Printf("[extract] WARNING: %s", res.Note)
		return res, nil
	}

	res.Extracted = true
	res.Files = files

	// The deletable set is recorded ONLY for an in-place extraction. Extracting
	// elsewhere means the source is something we must not touch — today that is a
	// seeding torrent's directory, which is the exact data this whole path exists
	// to protect. Leaving archiveParts nil makes CleanupArchives a no-op instead
	// of arming it with a deletion list aimed at the swarm's files, so the safety
	// does not depend on a caller remembering not to call it.
	if destDir == dir {
		res.archiveParts = archiveVolumesOf(dir, archive)
	}
	return res, nil
}

// CleanupArchives removes the volumes of the archive that ExtractInDir actually
// unpacked — and nothing else.
//
// It deliberately does NOT reuse Cleanup(): that one deletes by extension list
// (.nfo .txt .jpg .png .sfv .url …), which is safe for usenet, where the
// directory is a scratch space the downloader created and owns. A torrent's
// directory is what the SWARM served: the same sweep eats the user's subtitles
// (.txt), the poster (.jpg), the fanart (.png) and any notes they kept there.
// Measured on a 7-file release, only the .mkv survived.
//
// Passing an ExtractDirResult whose extraction did not happen is a no-op.
func CleanupArchives(res *ExtractDirResult) error {
	if res == nil || !res.Extracted {
		return nil
	}
	var firstErr error
	for _, part := range res.archiveParts {
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Printf("[extract] cleanup: removing %s", filepath.Base(part))
	}
	return firstErr
}

// archiveVolumesOf returns every volume belonging to the archive set entered at
// entryPath: the entry volume plus its continuation parts.
//
// Matching is anchored on the entry volume's own stem, so a directory holding
// two unrelated sets only ever loses the one that was unpacked. Nothing outside
// the set is considered, whatever its extension.
func archiveVolumesOf(dir, entryPath string) []string {
	entry := filepath.Base(entryPath)
	stem := archiveStem(entry)
	if stem == "" {
		return []string{entryPath}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{entryPath}
	}

	var parts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if archiveStem(name) != stem {
			continue
		}
		// Only ever remove things that look like archive volumes — never the
		// payload that was just extracted next to them.
		if !isArchiveFile(name) {
			continue
		}
		parts = append(parts, filepath.Join(dir, name))
	}
	if len(parts) == 0 {
		return []string{entryPath}
	}
	return parts
}

// archiveStem builds the grouping key for an archive volume: every volume of a
// set maps to the same key, and NO two sets ever share one.
//
//	show.part01.rar → "show|rar"    show.r00 → "show|rar"
//	show.zip.001    → "show|num"    show.001 → "show|num"
//
// The key carries the volume FORM, not just the base name, because the form is
// what tells two sets apart when they share a base: a directory can hold
// "Movie-GRP.zip.001/.002" beside "Movie-GRP.rar/.r00", and a base-only key
// merges them.
//
// This matters because the key decides what CleanupArchives DELETES. Merging
// two sets silently destroys the one that was never unpacked — measured twice
// on this function: first when it dropped a dotted segment of the release name
// ("Movie.2024.1080p-GRP.rar" and "Movie.2024.720p-OTHER.rar" both became
// "Movie.2024"), then when a numbered split and a rar set with the same base
// both became "Movie-GRP". Both returned 4 files for a 2-file set.
//
// Returns "" for a name that is not a recognised volume.
var volumeSuffixRe = regexp.MustCompile(`(?i)(\.part\d+)?\.(rar|r\d{2}|s\d{2}|(\d{3}))$`)

func archiveStem(name string) string {
	m := volumeSuffixRe.FindStringSubmatch(name)
	if m == nil {
		return "" // not a recognised volume name
	}
	base := name[:len(name)-len(m[0])]

	// A numbered volume (.001) is its own family. Nothing is trimmed from the
	// base: every volume of one split shares the SAME text before ".00N"
	// ("show.zip.001"/"show.zip.002" → "show.zip"), so grouping needs no
	// normalisation, and trimming would resurrect the very bug fixed above —
	// "Movie.2024.1080p-GRP.001" would lose "-GRP" and collide with
	// "Movie.2024.720p-OTHER.001". Keeping the container extension in the key is
	// what also separates "show.zip.001" from "show.7z.001", two different
	// archives of the same release.
	if m[3] != "" {
		return base + "|num"
	}
	// .rar / .rNN / .sNN / .partNN.rar all belong to ONE set — a rar archive
	// continues into .r00 and then .s00 — so they must share a key.
	return base + "|rar"
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
		// KNOWN GAP (deliberate, not an oversight): unlike priority 3 this does
		// NOT check for an archive header, so a junk ".rar" sitting next to a
		// valid split set wins on name length and the release reaches the library
		// packed. No data is lost — the failed extraction aborts before any
		// cleanup — and requiring magic here is NOT a free tightening: it would
		// discard a real archive whose first bytes cannot be read (permissions, a
		// flaky mount) and silently reclassify the release as "no archive". That
		// trade needs its own decision, so the gap is documented rather than
		// papered over.
		return filepath.Join(dir, rars[0])
	}

	// Priority 3: .001 split format — but ONLY when the file really is an
	// archive. ".001" is just a numbering convention: a release can ship
	// "video.001/.002" that are raw split DATA, not a container. Handing those
	// to 7z makes it concatenate them into a single output, report success, and
	// the caller would then delete the originals — turning a recoverable release
	// into a truncated file with no source left. The magic check is what keeps
	// "looks like a volume" from being confused with "is an archive".
	for _, name := range names {
		if !strings.HasSuffix(strings.ToLower(name), ".001") {
			continue
		}
		path := filepath.Join(dir, name)
		if hasArchiveMagic(path) {
			return path
		}
		log.Printf("[extract] %s looks like a split volume but carries no archive header - leaving it alone", name)
	}

	return ""
}

// hasArchiveMagic reports whether the file starts with a known archive
// signature (RAR4/RAR5, ZIP, 7z). Used to tell a real split archive from a set
// of raw data chunks that merely share the .00N naming convention.
func hasArchiveMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil || n < 4 {
		return false
	}
	head := string(buf[:n])

	switch {
	case strings.HasPrefix(head, "Rar!\x1a\x07"): // RAR4 and RAR5
		return true
	case strings.HasPrefix(head, "PK\x03\x04"), // zip: local file header
		strings.HasPrefix(head, "PK\x05\x06"), // zip: empty archive
		strings.HasPrefix(head, "PK\x07\x08"): // zip: spanned archive
		return true
	case n >= 6 && strings.HasPrefix(head, "7z\xbc\xaf\x27\x1c"):
		return true
	}
	return false
}
