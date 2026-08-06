package postprocess

import (
	"context"
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

// passwordProbeTimeout caps the encryption probe. It only reads the archive's
// headers, so anything longer means a hung process (typically a volume on an
// unresponsive network mount) rather than slow work.
const passwordProbeTimeout = 60 * time.Second

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

// FindExtractor checks which archive extractor is available in PATH.
func FindExtractor() (ExtractorType, string) {
	if path, err := exec.LookPath("unrar"); err == nil {
		return ExtractorUnrar, path
	}
	if path, err := exec.LookPath("7z"); err == nil {
		return Extractor7z, path
	}
	return ExtractorNone, ""
}

// Extract extracts an archive using the best available tool.
// password is optional — pass "" if not needed.
// Returns the list of extracted file paths.
func Extract(archivePath string, outputDir string, password string) ([]string, error) {
	extType, extPath := FindExtractor()
	if extType == ExtractorNone {
		return nil, fmt.Errorf("no archive extractor found (install unrar or 7z)")
	}

	// unrar only speaks RAR. A .001/.002 split set is a CONTAINER-agnostic naming
	// convention — it is just as likely to be a split zip or 7z — so handing such
	// a set to unrar merely because unrar is installed fails with "is not RAR
	// archive" even though 7z sits right there and would open it. Pick by format
	// first, and only fall back to availability.
	if extType == ExtractorUnrar && !isRarArchive(archivePath) {
		if szPath, err := exec.LookPath("7z"); err == nil {
			return extract7z(szPath, archivePath, outputDir, password)
		}
		// No 7z: let unrar try anyway — a .001 CAN be a split RAR, and a clear
		// extractor error beats refusing to attempt it.
	}

	switch extType {
	case ExtractorUnrar:
		return extractUnrar(extPath, archivePath, outputDir, password)
	case Extractor7z:
		return extract7z(extPath, archivePath, outputDir, password)
	default:
		return nil, fmt.Errorf("unknown extractor: %s", extType)
	}
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

// IsPasswordProtected checks if a rar archive requires a password.
func IsPasswordProtected(archivePath string) bool {
	extType, extPath := FindExtractor()
	if extType == ExtractorNone {
		return false
	}

	// Bounded, and much tighter than extractTimeout: this only has to read the
	// archive's headers, so a probe that runs for minutes is a hung process, not
	// slow work. It used to have no timeout at all — tolerable while only usenet
	// reached it, but this is now on the path of every multi-file torrent, and a
	// volume on a wedged NFS mount would block the task goroutine forever while
	// holding the release-directory lock.
	//
	// A timeout answers "not password protected": the caller then attempts the
	// extraction, which has its own timeout and reports a real error. Answering
	// "protected" instead would silently skip a release nobody locked.
	ctx, cancel := context.WithTimeout(context.Background(), passwordProbeTimeout)
	defer cancel()

	switch extType { //nolint:exhaustive // ExtractorNone handled above
	case ExtractorUnrar:
		cmd := exec.CommandContext(ctx, extPath, "t", "-p-", archivePath)
		winproc.HideWindow(cmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("[extract] password probe timed out after %s on %s", passwordProbeTimeout, filepath.Base(archivePath))
				return false
			}
			outStr := string(output)
			return strings.Contains(outStr, "password") || strings.Contains(outStr, "encrypted")
		}
	case Extractor7z:
		cmd := exec.CommandContext(ctx, extPath, "t", "-p", archivePath)
		winproc.HideWindow(cmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("[extract] password probe timed out after %s on %s", passwordProbeTimeout, filepath.Base(archivePath))
				return false
			}
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

// isArchiveFile returns true for rar/split archive files.
func isArchiveFile(name string) bool {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)

	if ext == ".rar" {
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
}

func (e *PasswordError) Error() string {
	return fmt.Sprintf("archive is password protected: %s", e.Archive)
}
