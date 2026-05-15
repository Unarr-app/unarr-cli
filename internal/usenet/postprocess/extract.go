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

	switch extType {
	case ExtractorUnrar:
		return extractUnrar(extPath, archivePath, outputDir, password)
	case Extractor7z:
		return extract7z(extPath, archivePath, outputDir, password)
	default:
		return nil, fmt.Errorf("unknown extractor: %s", extType)
	}
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

	switch extType { //nolint:exhaustive // ExtractorNone handled above
	case ExtractorUnrar:
		cmd := exec.Command(extPath, "t", "-p-", archivePath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			outStr := string(output)
			return strings.Contains(outStr, "password") || strings.Contains(outStr, "encrypted")
		}
	case Extractor7z:
		cmd := exec.Command(extPath, "t", "-p", archivePath)
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
