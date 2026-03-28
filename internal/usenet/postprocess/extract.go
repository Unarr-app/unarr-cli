package postprocess

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

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
func extractUnrar(unrarPath, archivePath, outputDir, password string) ([]string, error) {
	args := []string{"x", "-o+", "-y"}
	if password != "" {
		args = append(args, "-p"+password)
	} else {
		args = append(args, "-p-") // no password, skip asking
	}
	args = append(args, archivePath, outputDir+"/")

	cmd := exec.Command(unrarPath, args...)
	cmd.Dir = outputDir
	output, err := cmd.CombinedOutput()
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
func extract7z(szPath, archivePath, outputDir, password string) ([]string, error) {
	args := []string{"x", "-y", "-o" + outputDir}
	if password != "" {
		args = append(args, "-p"+password)
	} else {
		args = append(args, "-p") // empty password
	}
	args = append(args, archivePath)

	cmd := exec.Command(szPath, args...)
	cmd.Dir = outputDir
	output, err := cmd.CombinedOutput()
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

	switch extType {
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
