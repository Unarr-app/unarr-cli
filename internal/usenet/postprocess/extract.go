package postprocess

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// extractTimeout caps how long a single extractor invocation may run. Without
// a cap, an encrypted archive that triggers a TTY-only prompt (or a corrupt
// archive that confuses the tool) hangs the post-process pipeline forever.
const extractTimeout = 30 * time.Minute

// validatePassword rejects passwords containing control characters that could
// inject extra answers into unrar/7z prompts via stdin (e.g. a newline lets an
// attacker-controlled NZB password feed a second response to overwrite or
// rename prompts).
func validatePassword(password string) error {
	if strings.ContainsAny(password, "\r\n\x00") {
		return fmt.Errorf("invalid password: contains control characters")
	}
	return nil
}

// ExtractorType identifies which extraction tool is available.
type ExtractorType string

const (
	ExtractorNone  ExtractorType = ""
	ExtractorUnrar ExtractorType = "unrar"
	Extractor7z    ExtractorType = "7z"
)

// ExtractorNative is reported when no external binary is installed. It is not
// a "nothing available" answer: the in-process extractor always works.
const ExtractorNative ExtractorType = "native"

// FindExtractor reports which EXTERNAL archive extractor is available in PATH.
//
// It no longer decides whether extraction is possible at all — extraction is
// always possible via the native path — so ExtractorNone now means only "no
// shell fallback installed". Callers that used to treat ExtractorNone as fatal
// must not: see ExtractInDirTo and the doctor check.
func FindExtractor() (ExtractorType, string) {
	if path, err := exec.LookPath("unrar"); err == nil {
		return ExtractorUnrar, path
	}
	if path, err := exec.LookPath("7z"); err == nil {
		return Extractor7z, path
	}
	return ExtractorNone, ""
}

// extractNativeFn indirects the native extractor so tests can force it to fail
// and exercise the shell fallback on a machine where the native path succeeds —
// which is every machine. Without it the fallback branch would be unreachable
// in tests, i.e. unverified exactly where it matters.
var extractNativeFn = extractNative

// Extract extracts an archive, preferring the in-process (native) extractor and
// falling back to unrar/7z when one is installed.
//
// Native first, measured rather than assumed. Benchmarked against the shell
// extractors on real payloads (600 MB RAR set, 246 MB compressed):
//
//	RAR  -m0 store, 12×50 MB volumes   go 1.14-1.23s   unrar 0.97-2.16s   (go is STEADIER)
//	RAR  -m3 compressed, 246 MB        go 0.57s        unrar 0.35s        (1.6x)
//	7z   store, 600 MB                 go 0.56s        7z    0.55s        (parity, disk-bound)
//	7z   LZMA2 compressed, 246 MB      go 3.42s        7z    0.75s        (4.6x — the worst case)
//
// Output was byte-identical (sha256) in every case. The store rows are the ones
// that matter: scene releases ship -m0, so the common path is disk-bound and
// the native decoder is at worst indistinguishable. Only LZMA2 is materially
// slower, and 3.4s per 250 MB does not threaten a post-processing pipeline.
//
// The shell path is kept — not deleted — because a native decoder that chokes on
// an exotic scene RAR would otherwise leave the user with nothing, where today
// 7z rescues it. Every rescue is logged: if the log fills with them, the
// preference order was wrong and the evidence will say so.
//
// password is optional — pass "" if not needed.
// Returns the list of extracted file paths.
func Extract(archivePath string, outputDir string, password string) ([]string, error) {
	// Absolutise before anything else. Both shell extractors run with
	// cmd.Dir = outputDir, so a RELATIVE archive path is resolved from the output
	// directory instead of the caller's working directory and unrar exits 10
	// ("cannot open"). Every production caller happens to pass an absolute path,
	// which is why this never surfaced; it is fixed here rather than in each
	// extractor so the native and shell paths cannot disagree about what the
	// argument means.
	if abs, err := filepath.Abs(archivePath); err == nil {
		archivePath = abs
	}

	files, nativeErr := extractNativeFn(archivePath, outputDir, password)
	if nativeErr == nil {
		return files, nil
	}

	// A wrong password is deterministic: every extractor will reject it. Retrying
	// through the shell only doubles the time spent reaching the same answer.
	//
	// EXCEPT when the native verdict was a guess (PasswordError.Uncertain). A
	// header-encrypted 7z and a corrupt one fail identically in sevenzip, so that
	// verdict is the better of two indistinguishable readings — and 7z can
	// actually tell them apart. Letting the ambiguous case through to the
	// fallback is what turns a guess into an answer whenever a binary is present.
	var pwErr *PasswordError
	if errors.As(nativeErr, &pwErr) && !pwErr.Uncertain {
		return nil, nativeErr
	}

	extType, extPath := FindExtractor()
	if extType == ExtractorNone {
		return nil, nativeErr
	}

	log.Printf("[extract] native extractor failed (%v) - retrying with %s", nativeErr, extType)

	// unrar only speaks RAR. A .001/.002 split set is a CONTAINER-agnostic naming
	// convention — it is just as likely to be a split zip or 7z — so handing such
	// a set to unrar merely because unrar is installed fails with "is not RAR
	// archive" even though 7z sits right there and would open it. Pick by format
	// first, and only fall back to availability.
	if extType == ExtractorUnrar && !isRarArchive(archivePath) {
		if szPath, err := exec.LookPath("7z"); err == nil {
			return shellRescue(archivePath, nativeErr, func() ([]string, error) {
				return extract7z(szPath, archivePath, outputDir, password)
			})
		}
		// No 7z: let unrar try anyway — a .001 CAN be a split RAR, and a clear
		// extractor error beats refusing to attempt it.
	}

	switch extType {
	case ExtractorUnrar:
		return shellRescue(archivePath, nativeErr, func() ([]string, error) {
			return extractUnrar(extPath, archivePath, outputDir, password)
		})
	case Extractor7z:
		return shellRescue(archivePath, nativeErr, func() ([]string, error) {
			return extract7z(extPath, archivePath, outputDir, password)
		})
	default:
		return nil, nativeErr
	}
}

// shellRescue runs a shell extractor after the native one failed, logging
// loudly when it succeeds.
//
// The log line is the whole point of keeping the fallback: it is the only
// evidence that would justify reversing the preference order, and a silent
// rescue would leave that decision to guesswork forever.
//
// When the shell extractor ALSO fails, the native error is reported as the
// cause — it is the one describing the archive, whereas the shell error is
// usually a generic non-zero exit — with the shell failure attached.
func shellRescue(archivePath string, nativeErr error, run func() ([]string, error)) ([]string, error) {
	files, err := run()
	if err != nil {
		return nil, fmt.Errorf("%w (shell fallback also failed: %v)", nativeErr, err)
	}
	log.Printf("[extract] NATIVE-FALLBACK: shell extractor succeeded on %s where the native one failed (%v)",
		filepath.Base(archivePath), nativeErr)
	return files, nil
}

// isRarArchive reports whether the file carries the RAR magic signature
// (RAR4 "Rar!\x1a\x07\x00" / RAR5 "Rar!\x1a\x07\x01\x00").
//
// Content-sniffed rather than name-matched on purpose: the whole problem is
// that a .001 extension says nothing about the container inside. An unreadable
// file returns false and takes the caller down the tolerant path.
func isRarArchive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil || n < 7 {
		return false
	}
	return string(buf[:7]) == "Rar!\x1a\x07\x00" ||
		(n >= 8 && string(buf[:8]) == "Rar!\x1a\x07\x01\x00")
}

// extractUnrar extracts using unrar.
//
// Security: when a password is supplied it is sent via stdin rather than via
// the `-p<password>` switch so it does not appear in `/proc/<pid>/cmdline`
// (visible to any other process on the host). unrar prompts for the password
// when no `-p` switch is given, and reads the prompt response from stdin when
// no controlling TTY is attached (the usual case for a daemon-spawned child).
func extractUnrar(unrarPath, archivePath, outputDir, password string) ([]string, error) {
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	args := []string{"x", "-o+", "-y"}
	if password == "" {
		// Tell unrar there is no password so it skips the prompt and fails
		// fast on encrypted archives instead of hanging.
		args = append(args, "-p-")
	}
	args = append(args, archivePath, outputDir+"/")

	ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, unrarPath, args...)
	winproc.HideWindow(cmd)
	cmd.Dir = outputDir
	if password != "" {
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("unrar: timed out after %s", extractTimeout)
	}
	if err != nil {
		// Check for password error
		outStr := string(output)
		if strings.Contains(outStr, "wrong password") || strings.Contains(outStr, "Incorrect password") {
			return nil, &PasswordError{Archive: archivePath}
		}
		return nil, fmt.Errorf("unrar: %w\n%s", err, output)
	}

	return listExtractedFiles(outputDir, archivePath)
}

// extract7z extracts using 7z.
//
// Security: same rationale as extractUnrar — passwords go through stdin to
// avoid `/proc/<pid>/cmdline` exposure. 7z reads the password from stdin when
// no `-p` switch is given and the archive is encrypted.
func extract7z(szPath, archivePath, outputDir, password string) ([]string, error) {
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	args := []string{"x", "-y", "-o" + outputDir}
	if password == "" {
		// `-p` with no value tells 7z the password is empty so encrypted
		// archives fail fast instead of waiting for a prompt.
		args = append(args, "-p")
	}
	args = append(args, archivePath)

	ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, szPath, args...)
	winproc.HideWindow(cmd)
	cmd.Dir = outputDir
	if password != "" {
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("7z: timed out after %s", extractTimeout)
	}
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "Wrong password") || strings.Contains(outStr, "incorrect password") {
			return nil, &PasswordError{Archive: archivePath}
		}
		return nil, fmt.Errorf("7z: %w\n%s", err, output)
	}

	return listExtractedFiles(outputDir, archivePath)
}

// IsPasswordProtected checks if an archive requires a password.
//
// The native check runs first and answers for every supported container without
// spawning a process. It is also the only check available on a machine with no
// extractor binary — where this used to return a flat false, letting the
// pipeline walk into an extraction that could only fail.
func IsPasswordProtected(archivePath string) bool {
	if isNativePasswordProtected(archivePath) {
		return true
	}

	extType, extPath := FindExtractor()
	if extType == ExtractorNone {
		return false
	}

	switch extType { //nolint:exhaustive // ExtractorNone handled above
	case ExtractorUnrar:
		cmd := exec.Command(extPath, "t", "-p-", archivePath)
		winproc.HideWindow(cmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			outStr := string(output)
			return strings.Contains(outStr, "password") || strings.Contains(outStr, "encrypted")
		}
	case Extractor7z:
		cmd := exec.Command(extPath, "t", "-p", archivePath)
		winproc.HideWindow(cmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			outStr := string(output)
			return strings.Contains(outStr, "Wrong password") || strings.Contains(outStr, "encrypted")
		}
	}
	return false
}

// listExtractedFiles returns new files in outputDir that aren't the archive itself.
func listExtractedFiles(dir, archivePath string) ([]string, error) {
	archiveBase := filepath.Base(archivePath)
	archiveDir := filepath.Dir(archivePath)
	var files []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		// Skip archive files themselves
		if isArchiveFile(base) && filepath.Dir(path) == archiveDir {
			return nil
		}
		if base == archiveBase {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

// Cleanup removes archive and parity files from a directory.
func Cleanup(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if isCleanupTarget(name) {
			path := filepath.Join(dir, name)
			log.Printf("[usenet] cleanup: removing %s", name)
			os.Remove(path)
		}
	}
	return nil
}

// isArchiveFile returns true for rar/7z/zip/split archive files.
//
// .7z and .zip were added along with native extraction: a single-volume release
// in either format used to go unrecognised here, so it was never treated as an
// archive to skip or to clean up. Safe to widen because the two callers are
// anchored: listExtractedFiles only skips names in the archive's OWN directory,
// and archiveVolumesOf only considers names sharing the unpacked archive's stem.
// Deliberately NOT widened in cleanupRarParts, which deletes by extension with
// no such anchor — a bare ".zip" rule there would eat an unrelated archive the
// user had parked in a usenet scratch directory.
func isArchiveFile(name string) bool {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)

	if ext == ".rar" || ext == ".7z" || ext == ".zip" {
		return true
	}
	// .r00, .r01, ... .r99, .s00, etc.
	if len(ext) == 4 && (ext[1] == 'r' || ext[1] == 's') {
		return isNumeric(ext[2:])
	}
	// .001, .002, etc.
	if len(ext) == 4 && isNumeric(ext[1:]) {
		return true
	}
	return false
}

// isCleanupTarget returns true for files that should be removed after extraction.
var cleanupExts = regexp.MustCompile(`(?i)\.(par2|nfo|sfv|nzb|srr|srs|jpg|png|txt|url)$`)
var cleanupRarParts = regexp.MustCompile(`(?i)\.(rar|r\d{2}|s\d{2}|\d{3})$`)

func isCleanupTarget(name string) bool {
	if cleanupExts.MatchString(name) {
		return true
	}
	if cleanupRarParts.MatchString(name) {
		return true
	}
	return false
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// PasswordError indicates the archive requires a password.
type PasswordError struct {
	Archive string

	// Uncertain marks a verdict that was INFERRED rather than reported. A
	// header-encrypted 7z and a corrupt one produce the same parse failure, so
	// "needs a password" is the better reading of an ambiguous signal, not a
	// fact the decoder stated.
	//
	// Extract reads this to decide whether the shell fallback is still worth
	// running: a certain password error is deterministic and retrying wastes
	// time, while an uncertain one is exactly what a second extractor can
	// resolve. Without the flag the field would have to be re-derived from the
	// error message, which PasswordError does not carry.
	Uncertain bool
}

func (e *PasswordError) Error() string {
	if e.Uncertain {
		return fmt.Sprintf("archive is password protected or corrupt: %s", e.Archive)
	}
	return fmt.Sprintf("archive is password protected: %s", e.Archive)
}
