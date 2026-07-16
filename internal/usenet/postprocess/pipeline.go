package postprocess

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Result holds the outcome of post-processing.
type Result struct {
	FinalPath string   // path to the main content file (e.g., the video)
	Files     []string // all final files
	Repaired  bool     // whether par2 repair was needed
	Extracted bool     // whether archive extraction was performed
	// VerifyNote is non-empty when par2 verification was DEGRADED — parity shipped
	// but could not be confirmed (par2 missing, repair failed, verify error). The
	// download is still delivered, but the caller surfaces this so the user knows
	// the file is unverified rather than silently assuming it's good. Empty means
	// either "verified OK" or "no parity shipped" — both are non-degraded.
	VerifyNote string
	// Corrupt is true when par2 DEFINITIVELY confirmed the data is damaged and it
	// could not be repaired (repair failed, or corruption detected with no par2
	// binary to fix it). The engine treats this as an integrity failure and
	// re-downloads — distinct from VerifyNote's softer "unverified but delivered"
	// (e.g. no parity shipped, or a transient probe error).
	Corrupt bool
}

// Options configures post-processing behavior.
type Options struct {
	Password string // password for encrypted archives (empty = none)
	Cleanup  bool   // remove intermediate files after extraction
	// FetchParity downloads the par2 recovery volumes into the task dir when
	// verification detects damage — the index alone carries checksums but no
	// recovery blocks, so "repair is not possible" against index-only parity
	// just means the blocks aren't local yet, NOT that the release is beyond
	// saving. par2 discovers the volumes by name next to the index, so the
	// pipeline only needs the call to succeed. nil = no more parity available;
	// verification runs with what's on disk.
	FetchParity func() (map[string]string, error)
}

// par2VerifyFn/par2RepairFn are indirections over Par2Verify/Par2Repair so the
// fetch-retry logic is testable without a real par2 binary (same pattern as
// par2Lookup).
var (
	par2VerifyFn = Par2Verify
	par2RepairFn = Par2Repair
)

// Process runs the full post-processing pipeline on downloaded usenet files.
// Steps: par2 verify → par2 repair → extract archives → cleanup → find main file.
func Process(dir string, downloadedFiles map[string]string, opts Options) (*Result, error) {
	result := &Result{}

	// Step 1: Par2 verification and repair. Parity is optional, so a missing
	// binary or a failed repair does NOT abort the download — but it MUST be
	// surfaced (result.VerifyNote + a WARNING) instead of silently delivering an
	// unverified file as if it had passed.
	par2File := findPar2File(downloadedFiles)
	if par2File != "" {
		runPar2Step(par2File, downloadedFiles, opts, result)
	}

	// Step 2: Find and extract archives
	rarFile := findFirstRar(downloadedFiles)
	if rarFile != "" {
		log.Printf("[usenet] extracting archive: %s", filepath.Base(rarFile))

		// Check if password-protected
		if opts.Password == "" && IsPasswordProtected(rarFile) {
			return nil, &PasswordError{Archive: rarFile}
		}

		extracted, err := Extract(rarFile, dir, opts.Password)
		if err != nil {
			if _, ok := err.(*PasswordError); ok {
				return nil, err
			}
			return nil, fmt.Errorf("extraction failed: %w", err)
		}

		result.Extracted = true
		result.Files = extracted

		// Step 3: Cleanup archive + par2 files
		if opts.Cleanup {
			Cleanup(dir)
		}
	} else {
		// No archives — content files are the final files. Skip paths that no
		// longer exist: par2 repair renames misnamed/obfuscated files, so the
		// original map entries can go stale; their renamed forms are picked up
		// from a directory scan below.
		seen := make(map[string]bool)
		for _, path := range downloadedFiles {
			if isCleanupTarget(filepath.Base(path)) {
				continue
			}
			if _, statErr := os.Stat(path); statErr != nil {
				continue
			}
			result.Files = append(result.Files, path)
			seen[path] = true
		}
		if result.Repaired {
			if entries, readErr := os.ReadDir(dir); readErr == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					name := entry.Name()
					// par2 renames the damaged original aside as "<name>.1"
					if isCleanupTarget(name) || strings.HasSuffix(name, ".1") {
						continue
					}
					p := filepath.Join(dir, name)
					if !seen[p] {
						result.Files = append(result.Files, p)
					}
				}
			}
		}

		// Cleanup metadata files
		if opts.Cleanup {
			for name, path := range downloadedFiles {
				lower := strings.ToLower(name)
				ext := filepath.Ext(lower)
				if ext == ".par2" || ext == ".nfo" || ext == ".sfv" || ext == ".nzb" {
					log.Printf("[usenet] cleanup: removing %s", name)
					os.Remove(path)
				}
			}
		}
	}

	// Step 4: Find main content file (largest video file)
	result.FinalPath = findMainFile(dir, result.Files)

	if result.FinalPath == "" && len(result.Files) > 0 {
		result.FinalPath = result.Files[0]
	}

	return result, nil
}

// runPar2Step verifies (and if needed repairs) the delivery against par2
// parity. With lazy volumes, damage detected against an index-only set first
// triggers ONE opts.FetchParity round (downloads the recovery volumes next to
// the index) and a re-verify; only after full parity is local does a failure
// classify as Corrupt. Fetched volumes are merged into downloadedFiles so the
// cleanup step removes them. par2 repair also renames misnamed/obfuscated
// files back to their original names as a side effect.
func runPar2Step(par2File string, downloadedFiles map[string]string, opts Options, result *Result) {
	err := par2VerifyFn(par2File)
	if err == nil {
		return
	}

	var repairable *Par2RepairableError
	damaged := errors.As(err, &repairable) || errors.Is(err, ErrPar2Unrepairable)
	// Only an "insufficient recovery blocks" verdict (Unrepairable against the
	// index-only set) warrants downloading the volumes. A Repairable verdict
	// already has enough local parity — repair directly instead of fetching the
	// (potentially 10-20% of payload) volumes for nothing.
	needMoreBlocks := errors.Is(err, ErrPar2Unrepairable)
	if needMoreBlocks && opts.FetchParity != nil {
		log.Printf("[usenet] par2: insufficient recovery blocks, fetching volumes...")
		fetched, ferr := opts.FetchParity()
		if ferr != nil {
			// Damage is confirmed by the index checksums, and the blocks to fix
			// it can't be fetched — the delivery is corrupt, not "unverified".
			result.Corrupt = true
			result.VerifyNote = fmt.Sprintf("par2 detected damage but recovery volumes could not be downloaded: %v", ferr)
			log.Printf("[usenet] WARNING: %s", result.VerifyNote)
			return
		}
		for name, path := range fetched {
			downloadedFiles[name] = path
		}
		err = par2VerifyFn(par2File)
		if err == nil {
			return
		}
	}

	repairable = nil
	switch {
	case errors.Is(err, ErrPar2NotInstalled):
		result.VerifyNote = "par2 parity present but `par2` is not installed — delivered UNVERIFIED (install par2cmdline to enable verification/repair)"
		log.Printf("[usenet] WARNING: %s", result.VerifyNote)
	case errors.As(err, &repairable):
		log.Printf("[usenet] par2: corruption detected, attempting repair...")
		repairErr := par2RepairFn(par2File)
		switch {
		case repairErr == nil:
			result.Repaired = true // Par2Repair already logged the success
		case errors.Is(repairErr, ErrPar2NotInstalled):
			// Damage confirmed by parity, but no binary to repair it — the
			// delivered file IS corrupt. Mark it so the engine re-downloads.
			result.Corrupt = true
			result.VerifyNote = "par2 corruption detected but `par2` is not installed — cannot repair, file is CORRUPT"
			log.Printf("[usenet] WARNING: %s", result.VerifyNote)
		default:
			// Repair attempted and failed — the data is damaged beyond recovery.
			result.Corrupt = true
			result.VerifyNote = fmt.Sprintf("par2 repair failed — file is corrupt: %v", repairErr)
			log.Printf("[usenet] WARNING: %s", result.VerifyNote)
		}
	case errors.Is(err, ErrPar2Unrepairable):
		// Parity confirmed the data is damaged and unrepairable — definitively
		// corrupt (NOT a transient probe error). Engine re-downloads.
		result.Corrupt = true
		result.VerifyNote = fmt.Sprintf("par2: file is corrupt and cannot be repaired: %v", err)
		log.Printf("[usenet] WARNING: %s", result.VerifyNote)
	default:
		if damaged {
			// verify #1 already PROVED the content is damaged; a transient
			// failure of the post-fetch re-verify (binary crash, I/O hiccup, a
			// truncated volume) must NOT downgrade a confirmed-corrupt delivery
			// to "unverified" — that would ship the broken file and skip the
			// re-download. Treat it as corrupt.
			result.Corrupt = true
			result.VerifyNote = fmt.Sprintf("par2 confirmed damage but the re-verify failed after fetching parity — treated as corrupt: %v", err)
		} else {
			// A transient par2 probe/exec error on the FIRST verify — can't
			// confirm corruption, so deliver UNVERIFIED with a loud note rather
			// than nuking a possibly-good file.
			result.VerifyNote = fmt.Sprintf("par2 verification error — file unverified: %v", err)
		}
		log.Printf("[usenet] WARNING: %s", result.VerifyNote)
	}
}

// findPar2File returns the path of the main .par2 file (not volume sets).
func findPar2File(files map[string]string) string {
	var mainPar2 string
	var smallestSize int64 = -1

	for name, path := range files {
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".par2" {
			continue
		}
		// The main par2 file is typically the smallest one (index file)
		// Volume par2 files are larger (contain recovery data)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if smallestSize < 0 || fi.Size() < smallestSize {
			smallestSize = fi.Size()
			mainPar2 = path
		}
	}
	return mainPar2
}

// firstRarRe matches the first volume of a multi-part rar set.
// Patterns: .part01.rar, .part1.rar, or just .rar (single/first volume)
var firstRarRe = regexp.MustCompile(`(?i)\.part0*1\.rar$`)

// findFirstRar returns the path to the first rar volume.
// For multi-part rars (part01.rar, part02.rar...), returns part01 specifically.
func findFirstRar(files map[string]string) string {
	// Priority 1: Find explicitly named first part (part01.rar, part1.rar)
	for _, path := range files {
		if firstRarRe.MatchString(path) {
			return path
		}
	}

	// Priority 2: Find the shortest-named .rar file (usually the first volume)
	var rarFiles []struct {
		name string
		path string
	}
	for name, path := range files {
		if strings.HasSuffix(strings.ToLower(name), ".rar") {
			rarFiles = append(rarFiles, struct {
				name string
				path string
			}{name, path})
		}
	}
	if len(rarFiles) > 0 {
		sort.Slice(rarFiles, func(i, j int) bool {
			return len(rarFiles[i].name) < len(rarFiles[j].name)
		})
		return rarFiles[0].path
	}

	// Priority 3: .001 split format
	for name, path := range files {
		if strings.HasSuffix(strings.ToLower(name), ".001") {
			return path
		}
	}
	return ""
}

// findMainFile finds the largest video file in the directory or file list.
func findMainFile(dir string, files []string) string {
	videoExts := map[string]bool{
		".mkv": true, ".mp4": true, ".avi": true, ".mov": true,
		".wmv": true, ".flv": true, ".m4v": true, ".ts": true,
		".webm": true,
	}

	var bestPath string
	var bestSize int64

	// First try from the explicit file list
	for _, path := range files {
		ext := strings.ToLower(filepath.Ext(path))
		if !videoExts[ext] {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() > bestSize {
			bestSize = fi.Size()
			bestPath = path
		}
	}

	if bestPath != "" {
		return bestPath
	}

	// Fallback: scan directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !videoExts[ext] {
			continue
		}
		fi, err := entry.Info()
		if err != nil {
			continue
		}
		if fi.Size() > bestSize {
			bestSize = fi.Size()
			bestPath = filepath.Join(dir, entry.Name())
		}
	}

	return bestPath
}
