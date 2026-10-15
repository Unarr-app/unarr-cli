package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// errBlocked stands in for Windows' sharing violation: on POSIX nothing blocks
// a read that way, so tests teach isTransientBlock to recognise this instead.
var errBlocked = errors.New("the process cannot access the file because it is being used by another process")

// blockOpens makes the first n opens of path fail with cause (errBlocked when
// nil) and returns the count of opens of path.
func blockOpens(t *testing.T, path string, n int, cause error) *int {
	t.Helper()
	oldOpen, oldTransient := openFile, isTransientBlock
	t.Cleanup(func() { openFile, isTransientBlock = oldOpen, oldTransient })
	isTransientBlock = func(err error) bool { return errors.Is(err, errBlocked) }
	if cause == nil {
		cause = errBlocked
	}
	opens := 0
	openFile = func(name string) (*os.File, error) {
		if name != path {
			return os.Open(name)
		}
		if opens++; opens <= n {
			return nil, &os.PathError{Op: "open", Path: name, Err: cause}
		}
		return os.Open(name)
	}
	return &opens
}

// The daemon replaces active-tasks.json by rename; on Windows an open that
// lands at that instant fails with a sharing violation that clears in
// milliseconds. create waits it out, as writeFileAtomic's rename does.
func TestCreateRetriesATransientlyBlockedRead(t *testing.T) {
	l := newLayout(t)
	opens := blockOpens(t, filepath.Join(l.DataDir, nameTasks), 2, nil)
	res := mustCreate(t, l, CreateOptions{})
	if *opens != 3 || len(res.Manifest.Files) != 2 {
		t.Fatalf("opens = %d, files = %+v: want 2 blocked opens, then the read", *opens, res.Manifest.Files)
	}
}

// Any other error fails at once; a block that does not clear is given up on
// with the last error.
func TestCreateRetriesOnlyTransientBlocks(t *testing.T) {
	l := newLayout(t)
	opens := blockOpens(t, filepath.Join(l.DataDir, nameTasks), 1, os.ErrPermission)
	if _, err := Create(CreateOptions{Layout: l, Dir: t.TempDir()}); !errors.Is(err, os.ErrPermission) || *opens != 1 {
		t.Fatalf("err = %v after %d opens, want the permission error at once", err, *opens)
	}
	calls := 0
	_, err := retryTransient(func() ([]byte, error) { calls++; return nil, errBlocked }, 30*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errBlocked) || calls < 2 {
		t.Fatalf("err = %v after %d calls, want the block retried, then reported", err, calls)
	}
}
