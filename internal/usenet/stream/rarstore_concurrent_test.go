package stream

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// jitterFetcher wraps an ArticleFetcher and adds a small random delay to every
// Body call so concurrent header probes complete OUT OF ORDER. It deliberately
// does NOT implement concurrencyHinter, exercising the default fan-out.
type jitterFetcher struct{ inner ArticleFetcher }

func (j jitterFetcher) Body(ctx context.Context, messageID string) ([]byte, error) {
	time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond) //nolint:gosec // test jitter only
	return j.inner.Body(ctx, messageID)
}

// hintedFetcher wraps an ArticleFetcher and advertises a fixed MaxConcurrency so
// probeConcurrency uses the hinted value.
type hintedFetcher struct {
	inner ArticleFetcher
	max   int
}

func (h hintedFetcher) Body(ctx context.Context, messageID string) ([]byte, error) {
	return h.inner.Body(ctx, messageID)
}
func (h hintedFetcher) MaxConcurrency() int { return h.max }

// TestProbeVolumesConcurrentPreservesOrder builds a many-volume STORE release and
// probes it through a fetcher whose Body calls finish out of order. If the
// concurrent probeVolumes scrambled the per-volume chunk order, the reconstructed
// video would not byte-match the original. Run under -race, this also guards the
// per-volume slot writes against a data race.
func TestProbeVolumesConcurrentPreservesOrder(t *testing.T) {
	content := patternBytes(200_000)
	// ~40 volumes at 5000 bytes/volume — well past the default fan-out of 10, so
	// the probe genuinely runs in parallel across many rounds.
	n, articles := nntptest.BuildRarStore("movie.2160p.mkv", content, 5_000, 900)

	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	rs, err := Probe(context.Background(), jitterFetcher{inner: c}, n.RarFiles())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if rs.VideoSize() != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d (chunks likely mis-ordered)", rs.VideoSize(), len(content))
	}

	rd := rs.OpenVideo(context.Background())
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("reconstructed video mismatch (%d bytes) — concurrent probe scrambled chunk order", len(got))
	}
}

// TestProbeVolumesConcurrentMatchesSequential probes the SAME release twice — once
// forced serial (concurrency=1) and once concurrently — and asserts identical
// reconstructed content, proving the parallel path is a pure speed-up with no
// semantic change.
func TestProbeVolumesConcurrentMatchesSequential(t *testing.T) {
	content := patternBytes(120_000)
	n, articles := nntptest.BuildRarStore("show.s01e02.mkv", content, 4_000, 800)

	read := func(maxConc int) []byte {
		t.Helper()
		s := nntptest.NewFakeServer(t)
		s.AddArticles(articles)
		c := dialFake(t, s)
		rs, err := Probe(context.Background(), hintedFetcher{inner: c, max: maxConc}, n.RarFiles())
		if err != nil {
			t.Fatalf("Probe(maxConc=%d): %v", maxConc, err)
		}
		rd := rs.OpenVideo(context.Background())
		defer rd.Close()
		got, err := io.ReadAll(rd)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		return got
	}

	serial := read(1)
	parallel := read(8)
	if !bytes.Equal(serial, parallel) {
		t.Fatal("serial and concurrent probes produced different video content")
	}
	if !bytes.Equal(serial, content) {
		t.Fatal("serial probe did not reconstruct the original content")
	}
}

// TestProbeConcurrencySizing checks the fan-out chooser: hinted pool size wins,
// capped by the volume count, with a sane default and a floor of 1.
func TestProbeConcurrencySizing(t *testing.T) {
	plain := jitterFetcher{} // no hint → default
	if got := probeConcurrency(plain, 100); got != probeConcurrencyDefault {
		t.Errorf("no-hint fan-out = %d, want %d", got, probeConcurrencyDefault)
	}
	if got := probeConcurrency(hintedFetcher{max: 20}, 100); got != 20 {
		t.Errorf("hinted fan-out = %d, want 20", got)
	}
	if got := probeConcurrency(hintedFetcher{max: 20}, 5); got != 5 {
		t.Errorf("fan-out should cap at volume count: got %d, want 5", got)
	}
	if got := probeConcurrency(hintedFetcher{max: 0}, 3); got != 3 {
		t.Errorf("zero hint should fall back to default then cap: got %d, want 3", got)
	}
}
