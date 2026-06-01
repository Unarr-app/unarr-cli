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
}

// Options configures post-processing behavior.
type Options struct {
	Password string // password for encrypted archives (empty = none)
	Cleanup  bool   // remove intermediate files after extraction
}

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
		var repairable *Par2RepairableError
		err := Par2Verify(par2File)
		switch {
		case err == nil:
			// Verified OK — nothing to surface.
		case errors.Is(err, ErrPar2NotInstalled):
			result.VerifyNote = "par2 parity present but `par2` is not installed — delivered UNVERIFIED (install par2cmdline to enable verification/repair)"
			log.Printf("[usenet] WARNING: %s", result.VerifyNote)
		case errors.As(err, &repairable):
			log.Printf("[usenet] par2: corruption detected, attempting repair...")
			repairErr := Par2Repair(par2File)
			switch {
			case repairErr == nil:
				result.Repaired = true
				log.Printf("[usenet] par2: repair successful")
			case errors.Is(repairErr, ErrPar2NotInstalled):
				result.VerifyNote = "par2 corruption detected but `par2` is not installed — cannot repair, delivered POSSIBLY CORRUPT"
				log.Printf("[usenet] WARNING: %s", result.VerifyNote)
			default:
				result.VerifyNote = fmt.Sprintf("par2 repair failed — file may be corrupt: %v", repairErr)
				log.Printf("[usenet] WARNING: %s", result.VerifyNote)
			}
		default:
			result.VerifyNote = fmt.Sprintf("par2 verification error — file may be corrupt: %v", err)
			log.Printf("[usenet] WARNING: %s", result.VerifyNote)
		}
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
		// No archives — content files are the final files
		for _, path := range downloadedFiles {
			if !isCleanupTarget(filepath.Base(path)) {
				result.Files = append(result.Files, path)
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
