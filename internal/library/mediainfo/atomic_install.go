package mediainfo

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	if err := renameWithRetry(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("cannot install %s: %w", filepath.Base(dest), err)
	}
	return nil
}

// renameRetryWindow / renameRetryStep bound the wait for a Windows rename that
// somebody else is momentarily blocking. Short: the holders this waits out are
// brief (another installer's rename, an AV scanning a file written a
// millisecond ago), and a caller is a tool download that has already spent far
// longer fetching the bytes.
const (
	renameRetryWindow = 2 * time.Second
	renameRetryStep   = 50 * time.Millisecond
)

// renameWithRetry is os.Rename plus a bounded wait for the Windows case where
// the destination is held open by someone else.
//
// Without it the doc comment above is a promise this function does not keep on
// Windows: "a loser of the race simply replaces dest with identical bytes" is
// POSIX behaviour, and MoveFileEx instead fails the loser outright with
// ERROR_ACCESS_DENIED. Every caller propagates that, so a user whose antivirus
// happened to be reading the file got a failed ffmpeg/ffprobe/fpcalc download.
// It also made TestInstallBinaryAtomically flaky on windows-latest, which
// gates the release workflow — a red CI run there blocked publishing v1.11.4
// entirely.
//
// On POSIX isTransientRenameBlock is constant false, so this compiles down to a
// single os.Rename with no retry and no sleep.
func renameWithRetry(src, dst string) error {
	deadline := time.Now().Add(renameRetryWindow)
	for {
		err := os.Rename(src, dst)
		if err == nil || !isTransientRenameBlock(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(renameRetryStep)
	}
}
