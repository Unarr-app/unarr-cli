package engine

import (
	"fmt"
	"io"
	"log"
	"os"
)

// moveFile moves src onto dst, falling back to copy+delete when the two are on
// different filesystems (os.Rename returns EXDEV; on Windows, ERROR_NOT_SAME_DEVICE).
//
// This is the single implementation of a sequence that used to be inlined at
// five call sites in organize.go — the video, the principal video of a release
// directory, each subtitle, and both halves of replaceFile. Every one of them
// dropped the error from the final os.Remove, which is the interesting one:
//
//	A cross-device move that copies and then FAILS TO DELETE is not a move.
//	It is a duplication. The file lands correctly in the library and stays in
//	the downloads directory too, so the release occupies twice the disk and
//	shows up twice to anything that scans both. Nothing said a word.
//
// It is not rare on Windows, which is the platform where organize crosses
// devices most often (downloads on D:, library on a NAS share): a player with
// the file still open, an antivirus mid-scan, or an indexer holding a handle
// all make DeleteFile fail with ERROR_SHARING_VIOLATION.
//
// The leftover source is reported, not returned as an error. The destination is
// correct and complete at that point, so failing the move would push callers
// into a rollback that deletes a good file to undo a successful copy. A warning
// is the honest outcome: the move worked, the cleanup did not.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		log.Printf("[organize] warning: copied %s to %s but could not remove the source: %v "+
			"- the file now exists in BOTH places", src, dst, err)
	}
	return nil
}

// copyFile copies src to dst, leaving nothing behind at dst if it cannot finish.
//
// The two details that matter here are both about the caller's next move, which
// is to delete the source:
//
//   - The destination is flushed to disk and its Close is CHECKED. A deferred
//     Close discards its error, and that error is where a full filesystem
//     reports itself: the copy returns nil, moveFile deletes the source, and
//     the library is left holding a truncated file. Sync before Close so the
//     failure surfaces here rather than after the process exits.
//   - A partial destination is removed on any failure. Without that, a copy
//     that dies halfway leaves a playable-looking, truncated video sitting at
//     the final library path.
func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		os.Remove(dst)
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		os.Remove(dst)
		return fmt.Errorf("flush %s: %w", dst, err)
	}
	if err := d.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
