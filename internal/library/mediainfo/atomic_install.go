package mediainfo

import (
	"fmt"
	"os"
	"path/filepath"
)

// installBinaryAtomically places a freshly downloaded executable at dest so
// that NO reader ever sees a half-written file: the bytes go to a sibling temp
// file first and land at dest with one rename.
//
// Why it matters: several processes can download the same tool at once — the
// daemon and a CLI invocation, or (the case that broke CI) parallel `go test`
// package binaries that each noticed ffprobe was missing. With a plain
// WriteFile, the second process opened the SAME path for writing while the
// first was already exec'ing it: `fork/exec …/ffprobe: text file busy`, or a
// truncated binary run mid-write. A rename swaps the whole file in one step;
// a process that already started the old inode keeps running it, and a loser
// of the race simply replaces dest with identical bytes.
func installBinaryAtomically(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Any failure below leaves no stray temp file behind.
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("cannot write %s: %w", filepath.Base(dest), err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("cannot chmod %s: %w", filepath.Base(dest), err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("cannot close %s: %w", filepath.Base(dest), err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("cannot install %s: %w", filepath.Base(dest), err)
	}
	return nil
}
