package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// minPlausibleVideoBytes is the anti-stub floor: any single video file smaller
// than this is not real media. A debrid/stream CDN can answer 200 with no
// Content-Length and a tiny (often all-NUL) body when a link is expired or not
// yet cached; without a size floor such a stub passes verify() and organize()
// files it as a movie (the movie.mkv/movie (N).mkv flood). 1 MiB is far below any
// genuine video yet far above every stub observed.
const minPlausibleVideoBytes = 1 << 20 // 1 MiB

// isStorageStatErr reports whether a stat/read error means the DESTINATION mount
// faulted rather than the file being genuinely absent: an I/O error (EIO) or a
// stale NFS handle (ESTALE) is a dropped/stalled mount, not corruption or a
// missing source. Those must be classified as storage (retry once, then fail
// with the storage message) instead of a generic "file not found".
func isStorageStatErr(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE)
}

// verify checks that a downloaded file or directory is valid.
func verify(result *Result) error {
	if result == nil || result.FilePath == "" {
		return fmt.Errorf("no file path in result")
	}

	fi, err := os.Stat(result.FilePath)
	if err != nil {
		// A mount that dropped between the download and this stat (EIO/ESTALE) is a
		// storage failure, not a missing/corrupt file — route it to failStorage.
		if isStorageStatErr(err) {
			return storageErr("stat_failed", filepath.Dir(result.FilePath), "could not read back the download in %s — is your drive/NAS still connected? (%v)", filepath.Dir(result.FilePath), err)
		}
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
		// Integrity, not transport: a zero-byte result is corrupt — let the manager
		// re-download clean rather than surface an empty file as completed.
		return integrityErr("empty", "download is empty: %s", result.FilePath)
	}

	// Anti-stub floor (defense in depth for the debrid stub flood): a single video
	// file below minPlausibleVideoBytes is a CDN error/expired-link stub, not media.
	// This is independent of result.Size — for a debrid download result.Size is the
	// stub's own byte count (self-referential), so the 2% check below can't catch it.
	// Directories (multi-file torrents) are exempt: their per-file sizes vary.
	if !fi.IsDir() && isVideoFile(filepath.Base(result.FilePath)) && actualSize < minPlausibleVideoBytes {
		return integrityErr("stub", "video file too small to be valid: %d bytes (%s)", actualSize, result.FilePath)
	}

	// If we know the expected size, check within 2% tolerance (container/muxing
	// overhead). A shortfall beyond that is a truncated/corrupt file — classify it
	// as an IntegrityError so the manager re-downloads clean instead of completing
	// a half file (the last line of defense across every backend).
	if result.Size > 0 {
		tolerance := int64(float64(result.Size) * 0.02)
		if actualSize < result.Size-tolerance {
			return integrityErr("size_mismatch", "size mismatch: expected %d, got %d", result.Size, actualSize)
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
