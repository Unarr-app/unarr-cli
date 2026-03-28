package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// verify checks that a downloaded file or directory is valid.
func verify(result *Result) error {
	if result == nil || result.FilePath == "" {
		return fmt.Errorf("no file path in result")
	}

	fi, err := os.Stat(result.FilePath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	// Get actual size — handle both files and directories (multi-file torrents)
	var actualSize int64
	if fi.IsDir() {
		actualSize, err = dirSize(result.FilePath)
		if err != nil {
			return fmt.Errorf("could not calculate dir size: %w", err)
		}
	} else {
		actualSize = fi.Size()
	}

	if actualSize == 0 {
		return fmt.Errorf("download is empty: %s", result.FilePath)
	}

	// If we know the expected size, check within 2% tolerance
	if result.Size > 0 {
		tolerance := int64(float64(result.Size) * 0.02)
		if actualSize < result.Size-tolerance {
			return fmt.Errorf("size mismatch: expected %d, got %d", result.Size, actualSize)
		}
	}

	return nil
}

// dirSize returns total size of all files in a directory.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}
