package mediainfo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireTrickplayLock_SingleFlight(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "sprite.jpg.lock")

	release, err := acquireTrickplayLock(lock)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Fatalf("lock file not created: %v", statErr)
	}

	// Second acquire while the first is held → skip sentinel, not a real error.
	if _, err := acquireTrickplayLock(lock); !errors.Is(err, ErrTrickplayInProgress) {
		t.Fatalf("expected ErrTrickplayInProgress, got %v", err)
	}

	// After release the lock file is gone and it can be re-acquired.
	release()
	if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
		t.Fatalf("lock file should be removed after release, stat err = %v", statErr)
	}
	release2, err := acquireTrickplayLock(lock)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

func TestAcquireTrickplayLock_ReclaimsStale(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "sprite.jpg.lock")

	// Simulate a crashed worker: a lock file older than the TTL with no live owner.
	if err := os.WriteFile(lock, []byte("deadhost pid=999 t=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-trickplayLockTTL - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	release, err := acquireTrickplayLock(lock)
	if err != nil {
		t.Fatalf("stale lock should be reclaimed, got %v", err)
	}
	release()
}

func TestAcquireTrickplayLock_FreshNotReclaimed(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "sprite.jpg.lock")
	if err := os.WriteFile(lock, []byte("livehost pid=123 t=now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fresh mtime (just written) → a live owner is assumed; must NOT be stolen.
	if _, err := acquireTrickplayLock(lock); !errors.Is(err, ErrTrickplayInProgress) {
		t.Fatalf("fresh lock must not be reclaimed, got %v", err)
	}
}
