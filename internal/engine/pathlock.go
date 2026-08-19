package engine

import (
	"context"
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

// pathLockEntry is a channel-based mutex (capacity 1) so acquisition can be
// aborted by a context — a plain sync.Mutex would park a cancelled task until
// the current holder finishes a multi-gigabyte download.
type pathLockEntry struct {
	ch   chan struct{}
	refs int
}

func newPathLocker() *pathLocker {
	return &pathLocker{locks: map[string]*pathLockEntry{}}
}

func (l *pathLocker) entry(path string) (*pathLockEntry, string) {
	key, err := filepath.Abs(path)
	if err != nil {
		key = path // best effort: an unresolvable path still gets a stable key
	}
	l.mu.Lock()
	e, ok := l.locks[key]
	if !ok {
		e = &pathLockEntry{ch: make(chan struct{}, 1)}
		l.locks[key] = e
	}
	e.refs++
	l.mu.Unlock()
	return e, key
}

func (l *pathLocker) release(e *pathLockEntry, key string) {
	l.mu.Lock()
	e.refs--
	if e.refs == 0 {
		delete(l.locks, key)
	}
	l.mu.Unlock()
}

// Lock blocks until the lock for path is held and returns its release function.
// Paths are normalized with filepath.Abs so two spellings of one location share
// a lock.
func (l *pathLocker) Lock(path string) func() {
	unlock, _ := l.LockCtx(context.Background(), path)
	return unlock
}

// LockCtx is Lock with an abort: a caller whose context is cancelled while
// WAITING (its download was cancelled/paused, the daemon is shutting down)
// returns ctx.Err() immediately instead of staying parked behind the current
// holder for the rest of a multi-hour download.
func (l *pathLocker) LockCtx(ctx context.Context, path string) (func(), error) {
	e, key := l.entry(path)
	select {
	case e.ch <- struct{}{}:
	case <-ctx.Done():
		l.release(e, key)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-e.ch
			l.release(e, key)
		})
	}, nil
}
