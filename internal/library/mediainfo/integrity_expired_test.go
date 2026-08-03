package mediainfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/testutil"
)

// tempVideo writes a stand-in file so os.Stat succeeds and the probe path runs.
func tempVideo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(p, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A retry is only worth queueing for OUR timeout. A probe that could never run
// (missing ffprobe, unreadable container) fails identically on a retry, so
// classifying it as expired would drag the whole library through the serial
// pass for nothing.
func TestNonTimeoutFailureDoesNotRequestRetry(t *testing.T) {
	integ, err := AssessTruncation(context.Background(), "/nonexistent/ffprobe", "", tempVideo(t), 1440)
	if integ != nil {
		t.Fatalf("a probe that never ran must not produce a verdict, got %+v", integ)
	}
	if errors.Is(err, ErrProbeExpired) {
		t.Fatal("a missing ffprobe was classified as a retryable timeout")
	}
}

// A cancelled scan must neither flag the file nor queue a retry — the daemon is
// shutting down and nobody would run that work.
func TestCancelledScanYieldsNoVerdictAndNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	integ, err := AssessTruncation(ctx, "/nonexistent/ffprobe", "", tempVideo(t), 1440)
	if integ != nil {
		t.Fatalf("cancelled probe must never produce a verdict, got %+v", integ)
	}
	if errors.Is(err, ErrProbeExpired) {
		t.Fatal("a cancelled scan must not queue a retry")
	}
}

// A probe that exhausts our own per-probe deadline IS retryable, and still must
// not yield a verdict. This is the case the whole fix exists for: 70/70 of the
// fleet's "tail probe failed" entries were exactly this.
//
// The parent context carries NO deadline here, matching production — the scan
// ctx is cancellation-only, so the sole deadline is truncProbeTimeout's.
func TestExpiredProbeRequestsRetryWithoutVerdict(t *testing.T) {
	testutil.RequireShellStubs(t)
	// A fake ffprobe that outlives the (shortened) per-probe deadline.
	fake := filepath.Join(t.TempDir(), "slowprobe")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	restore := truncProbeTimeout
	truncProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { truncProbeTimeout = restore })

	integ, err := AssessTruncation(context.Background(), fake, "", tempVideo(t), 1440)
	if integ != nil {
		t.Fatalf("a timed-out probe must never produce a verdict, got %+v", integ)
	}
	if !errors.Is(err, ErrProbeExpired) {
		t.Fatalf("an exhausted per-probe deadline must request a retry, got err=%v", err)
	}
}

// The tail decode (check C) fails OPEN, so a timed-out decode looks exactly
// like a clean one. Without tracking it, such a file would be silently called
// healthy and never re-checked — the same gap this fix closes for the demux,
// one level down. It must request a retry, and still never flag the file.
func TestExpiredDecodeRequestsRetryWithoutVerdict(t *testing.T) {
	testutil.RequireShellStubs(t)
	slow := filepath.Join(t.TempDir(), "slowffmpeg")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	restore := truncDecodeTimeout
	truncDecodeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { truncDecodeTimeout = restore })

	failed, expired := tailDecodeFails(context.Background(), slow, tempVideo(t), 1440)
	if failed {
		t.Fatal("a timed-out decode must never report a failure — it fails open by design")
	}
	if !expired {
		t.Fatal("a timed-out decode must be reported as expired so the file is retried")
	}
}

// A cancelled scan must not queue a decode retry either.
func TestCancelledDecodeDoesNotRequestRetry(t *testing.T) {
	slow := filepath.Join(t.TempDir(), "slowffmpeg")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	failed, expired := tailDecodeFails(ctx, slow, tempVideo(t), 1440)
	if failed {
		t.Fatal("a cancelled decode must never report a failure")
	}
	if expired {
		t.Fatal("a cancelled scan must not queue a decode retry")
	}
}

// An Unverified verdict must never be a damaged one — the sync layer keys off
// Damaged, so this is the invariant that keeps "unchecked" from becoming
// "corrupt" upstream.
func TestUnverifiedIsNotDamaged(t *testing.T) {
	unver := &IntegrityInfo{Unverified: true, Reason: "probe_timeout"}
	if unver.Damaged {
		t.Fatal("Unverified must leave Damaged false so it can never sync as corrupt")
	}
}

// The probe timeout must stay well above what a healthy 4K file needs on slow
// network storage. Measured on a real NFS library: 8-47s serial, 7-107s with
// Workers=8. A 30s cap turned all of those into false "signal: killed".
func TestProbeTimeoutCoversSlowNetworkStorage(t *testing.T) {
	const worstMeasuredSerial = 47 * time.Second
	if truncProbeTimeout <= worstMeasuredSerial {
		t.Fatalf("truncProbeTimeout %s is below the measured worst-case serial tail probe (%s) — "+
			"healthy files on NFS will be killed and left unchecked", truncProbeTimeout, worstMeasuredSerial)
	}
}
