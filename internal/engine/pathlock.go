package engine

import (
	"path/filepath"
	"sync"
)

// pathLocker serializes work keyed by filesystem path. Entries are
// reference-counted and dropped when the last holder releases, so the map does
// not grow with every path the process ever touched.
//
// Used by the manager to serialize post-processing per release directory and by
// the debrid downloader to guarantee a single writer per destination file (two
// tasks can resolve to the same DirectFileName; interleaved writes into one
// partial produce a corrupt file).
type pathLocker struct {
	mu    sync.Mutex
	locks map[string]*pathLockEntry
}

type pathLockEntry struct {
	mu   sync.Mutex
	refs int
}

func newPathLocker() *pathLocker {
	return &pathLocker{locks: map[string]*pathLockEntry{}}
}

// Lock blocks until the lock for path is held and returns its release function.
// Paths are normalized with filepath.Abs so two spellings of one location share
// a lock; an unresolvable path still gets a stable key (best effort).
func (l *pathLocker) Lock(path string) func() {
	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}

	l.mu.Lock()
	e, ok := l.locks[key]
	if !ok {
		e = &pathLockEntry{}
		l.locks[key] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
