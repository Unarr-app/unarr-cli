package library

import (
	"context"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// pendingItem builds a scanned item whose deep probe timed out during the
// concurrent pass: full metadata, no verdict yet.
func pendingItem(path string, dur float64) LibraryItem {
	return LibraryItem{
		FilePath:         path,
		FileName:         path,
		TailProbePending: true,
		MediaInfo: &mediainfo.MediaInfo{
			Video: &mediainfo.VideoInfo{Duration: dur},
		},
	}
}

// stubAssess swaps the deep check for the duration of a test.
func stubAssess(t *testing.T, fn func(path string) (*mediainfo.IntegrityInfo, error)) {
	t.Helper()
	orig := assessTruncation
	assessTruncation = func(_ context.Context, _, _, filePath string, _ float64) (*mediainfo.IntegrityInfo, error) {
		return fn(filePath)
	}
	t.Cleanup(func() { assessTruncation = orig })
}

// THE regression guard. A retry that STILL can't check the file must record it
// as unverified — never as damaged. Marking an unchecked file corrupt is what
// flagged ~1.4k healthy files fleet-wide (2026-07-21).
func TestRetryPendingTailsExpiredAgainIsUnverifiedNotDamaged(t *testing.T) {
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		return nil, mediainfo.ErrProbeExpired // slow even with no contention
	})

	items := []LibraryItem{pendingItem("/media/slow4k.mkv", 7200)}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	integ := items[0].MediaInfo.Integrity
	if integ == nil {
		t.Fatal("a file that stayed unchecked must be recorded, not silently passed off as healthy")
	}
	if integ.Damaged {
		t.Fatal("an expired probe was marked DAMAGED — this is the 1451-false-positive regression")
	}
	if !integ.Unverified {
		t.Fatalf("want Unverified=true, got %+v", integ)
	}
	if items[0].TailProbePending {
		t.Error("item must not stay queued after the retry pass")
	}
}

// The whole point of the retry: a file that timed out under concurrency but
// probes fine on its own gets a real verdict. Measured 107s → 0.8s on NFS.
func TestRetryPendingTailsRecoversOnSerialProbe(t *testing.T) {
	calls := 0
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		calls++
		return nil, nil // ran to completion, nothing wrong
	})

	items := []LibraryItem{pendingItem("/media/a.mkv", 1440)}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	if calls != 1 {
		t.Fatalf("expected exactly one serial re-probe, got %d", calls)
	}
	if items[0].MediaInfo.Integrity != nil {
		t.Fatalf("a clean re-probe must leave no verdict, got %+v", items[0].MediaInfo.Integrity)
	}
	if items[0].TailProbePending {
		t.Error("item must be cleared once checked")
	}
}

// A retry that finds real truncation must still flag it — the fix must not
// blunt genuine detection.
func TestRetryPendingTailsStillDetectsRealDamage(t *testing.T) {
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		return &mediainfo.IntegrityInfo{Damaged: true, Reason: "truncated"}, nil
	})

	items := []LibraryItem{pendingItem("/media/half.mkv", 1440)}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	integ := items[0].MediaInfo.Integrity
	if integ == nil || !integ.Damaged || integ.Reason != "truncated" {
		t.Fatalf("a corroborated truncation must survive the retry path, got %+v", integ)
	}
}

// Only the pending items are re-probed; a file already checked in the
// concurrent pass must not be touched again.
func TestRetryPendingTailsOnlyProbesPendingItems(t *testing.T) {
	var probed []string
	stubAssess(t, func(path string) (*mediainfo.IntegrityInfo, error) {
		probed = append(probed, path)
		return nil, nil
	})

	items := []LibraryItem{
		pendingItem("/media/pending.mkv", 1440),
		{FilePath: "/media/clean.mkv", MediaInfo: &mediainfo.MediaInfo{Video: &mediainfo.VideoInfo{Duration: 1440}}},
	}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	if len(probed) != 1 || probed[0] != "/media/pending.mkv" {
		t.Fatalf("retry must touch only pending items, probed: %v", probed)
	}
}

// A cancelled context must stop the pass without mislabelling the files it
// never got to: they stay pending, exactly as if the pass hadn't run.
func TestRetryPendingTailsStopsOnCancel(t *testing.T) {
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		t.Error("no probe may run once the context is cancelled")
		return nil, nil
	})

	items := []LibraryItem{
		pendingItem("/media/a.mkv", 1440),
		pendingItem("/media/b.mkv", 1440),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	retryPendingTails(ctx, "ffprobe", "", items)

	for _, it := range items {
		if !it.TailProbePending {
			t.Errorf("%s was resolved despite a cancelled context", it.FilePath)
		}
		if it.MediaInfo.Integrity != nil {
			t.Errorf("%s got a verdict during a cancelled retry: %+v", it.FilePath, it.MediaInfo.Integrity)
		}
	}
}

// An item with no usable media info can't be re-probed; it must be cleared
// rather than looping forever or panicking on a nil Video.
func TestRetryPendingTailsHandlesMissingMediaInfo(t *testing.T) {
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		t.Error("must not probe an item with no video info")
		return nil, nil
	})

	items := []LibraryItem{
		{FilePath: "/media/x.mkv", TailProbePending: true},
		{FilePath: "/media/y.mkv", TailProbePending: true, MediaInfo: &mediainfo.MediaInfo{}},
	}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	for _, it := range items {
		if it.TailProbePending {
			t.Errorf("%s still pending; retry must not leave un-probeable items queued", it.FilePath)
		}
	}
}

// No pending items → the pass probes nothing at all.
func TestRetryPendingTailsNoopWhenNothingPending(t *testing.T) {
	stubAssess(t, func(string) (*mediainfo.IntegrityInfo, error) {
		t.Error("retry must be a no-op when nothing is pending")
		return nil, nil
	})

	items := []LibraryItem{
		{FilePath: "/media/clean.mkv", MediaInfo: &mediainfo.MediaInfo{}},
	}
	retryPendingTails(context.Background(), "ffprobe", "", items)

	if items[0].MediaInfo.Integrity != nil {
		t.Fatal("a clean item must be left untouched by the retry pass")
	}
}
