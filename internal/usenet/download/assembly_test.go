package download_test

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/download"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// payload builds deterministic pseudo-random content — random so a
// wrongly-offset write can't accidentally reproduce the expected bytes, seeded
// so a failure is reproducible.
func payload(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(42))
	_, _ = r.Read(b)
	return b
}

func connect(t *testing.T, s *nntptest.FakeServer) *nntp.Client {
	t.Helper()
	c := nntp.NewClient(s.Config())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestMultipartAssemblyIsByteExact is the regression guard for the corruption
// that shipped broken usenet downloads: decoded segment data was written at
// offsets accumulated from NZB Segment.Bytes, which is the yEnc-ENCODED article
// size (~3% larger than the payload). Every segment after the first landed too
// far into the file, leaving gaps, and the closing Truncate — sized from the
// sum of DECODED bytes — then chopped the overrun off the tail. par2 saw a
// shredded file and declared it unrepairable.
//
// The fixture mirrors the real world exactly (Segment.Bytes = len(yEnc body)),
// so this test fails on the old assembler and passes only when the writer
// honours each article's own "=ypart begin=" offset.
func TestMultipartAssemblyIsByteExact(t *testing.T) {
	content := payload(700 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("movie.mkv", content, 64*1024)

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	// Guard the premise: if the fixture ever starts reporting decoded sizes,
	// this test would pass vacuously and stop protecting anything.
	var encodedTotal int64
	for _, seg := range nzbFile.Files[0].Segments {
		encodedTotal += seg.Bytes
	}
	if encodedTotal <= int64(len(content)) {
		t.Fatalf("fixture premise broken: NZB byte total %d should exceed the decoded payload %d (yEnc overhead)",
			encodedTotal, len(content))
	}

	outDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))

	files, missing, err := dl.DownloadNZB(context.Background(), nzbFile, outDir, nil, nil)
	if err != nil {
		t.Fatalf("DownloadNZB: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing segments = %d, want 0", len(missing))
	}

	got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("assembled size = %d, want %d (delta %+d — offsets or the final truncate are wrong)",
			len(got), len(content), len(got)-len(content))
	}
	if !bytes.Equal(got, content) {
		for i := range got {
			if got[i] != content[i] {
				t.Fatalf("assembled bytes differ from the payload at offset %d", i)
			}
		}
	}
	if _, ok := files["movie.mkv"]; !ok {
		t.Fatalf("result map = %v, want an entry for movie.mkv", files)
	}
}

// TestResumeAssemblesByteExact covers the same invariant across a process
// boundary: the tail segment lands in the FIRST run, so the resumed run never
// observes its =ypart offset and can only size the file correctly from the
// persisted knownSize. Truncating to the bytes fetched this run would cut the
// file short.
//
// The hole is created by withholding an article from the server rather than
// with FailNext — the NNTP client reconnects and retries once, so a transient
// injected failure is invisible here, while an absent article is a permanent
// 430 no matter how often it is retried.
func TestResumeAssemblesByteExact(t *testing.T) {
	content := payload(400 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("show.mkv", content, 32*1024)
	file := nzbFile.Files[0]

	// Withhold the FIRST segment: every other article — crucially the tail —
	// still lands in run 1, which is the whole point of the test.
	hole := file.Segments[0].MessageID
	withheld := articles[hole]
	delete(articles, hole)

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	outDir := t.TempDir()
	resumeDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))
	dl.MissingTolerance = 0.5 // keep going so the rest of the file lands

	tracker := download.NewProgressTracker("task-resume", nzbFile, resumeDir)
	_, missing, err := dl.DownloadFile(context.Background(), file, 0, outDir, tracker, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("run 1: missing = %d, want 1", len(missing))
	}
	if err := tracker.Flush(); err != nil {
		t.Fatalf("run 1: flush: %v", err)
	}
	doneAfterRun1 := tracker.CompletedSegments(0)
	if doneAfterRun1 != len(file.Segments)-1 {
		t.Fatalf("run 1: completed %d/%d segments, want all but the withheld one",
			doneAfterRun1, len(file.Segments))
	}

	// The article comes back (retention hiccup resolved / different provider).
	srv.AddArticle(hole, withheld)

	// Run 2: a fresh tracker, as a retry in a new process would build.
	resumed := download.NewProgressTracker("task-resume", nzbFile, resumeDir)
	ok, err := resumed.Load()
	if err != nil || !ok {
		t.Fatalf("run 2: Load() = %v, %v; want the run-1 progress to be reusable", ok, err)
	}
	if got := resumed.CompletedSegments(0); got != doneAfterRun1 {
		t.Fatalf("run 2: resumed with %d segments, want %d", got, doneAfterRun1)
	}

	before := srv.BodyCalls()
	if _, _, err := dl.DownloadFile(context.Background(), file, 0, outDir, resumed, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if fetched := srv.BodyCalls() - before; fetched != 1 {
		t.Fatalf("run 2 fetched %d articles, want exactly the 1 that was missing — anything more means it restarted",
			fetched)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "show.mkv"))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("resumed file differs from the payload (size %d, want %d)", len(got), len(content))
	}
}

// TestMissingSegmentsToleratedForPar2 pins the policy change: with parity
// available the download must survive dead articles and hand the holes to par2,
// instead of aborting the whole file on the first 430 and discarding everything
// already fetched.
func TestMissingSegmentsToleratedForPar2(t *testing.T) {
	content := payload(600 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("film.mkv", content, 16*1024)

	// A permanently absent article — an expired one, as on real Usenet.
	delete(articles, nzbFile.Files[0].Segments[3].MessageID)

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	outDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))
	dl.MissingTolerance = 0.05

	_, missing, err := dl.DownloadFile(context.Background(), nzbFile.Files[0], 0, outDir, nil, nil)
	if err != nil {
		t.Fatalf("one dead article inside tolerance must not fail the download: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %d, want 1", len(missing))
	}

	// The hole stays a hole (zeros for par2 to repair), but the file must still
	// be full length — a short file would put the damage past par2's reach.
	fi, err := os.Stat(filepath.Join(outDir, "film.mkv"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("size = %d, want %d — the hole must not shorten the file", fi.Size(), len(content))
	}
}

// TestMissingSegmentsBeyondToleranceFails guards the other side: tolerance is a
// budget for parity to absorb, not a licence to deliver swiss cheese.
func TestMissingSegmentsBeyondToleranceFails(t *testing.T) {
	content := payload(200 * 1024)
	nzbFile, _ := nntptest.BuildDirectFile("bad.mkv", content, 16*1024)

	// Serve nothing at all: every article is a permanent 430.
	srv := nntptest.NewFakeServer(t)

	dl := download.NewDownloader(connect(t, srv))
	dl.MissingTolerance = 0.05

	_, _, err := dl.DownloadFile(context.Background(), nzbFile.Files[0], 0, t.TempDir(), nil, nil)
	if err == nil {
		t.Fatal("expected failure when every segment is unavailable")
	}
}

// TestTailSegmentMissingKeepsFullLength guards the asymmetry the review found:
// truncating to the highest offset written is right for an INTERIOR hole, but
// when the LAST segment is the missing one that offset is short of the real end
// — and cutting there puts the damage past par2's reach, defeating the whole
// point of downloading with holes.
func TestTailSegmentMissingKeepsFullLength(t *testing.T) {
	content := payload(300 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("tail.mkv", content, 32*1024)
	segs := nzbFile.Files[0].Segments

	delete(articles, segs[len(segs)-1].MessageID) // the tail is the hole

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	outDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))
	dl.MissingTolerance = 0.5

	_, missing, err := dl.DownloadFile(context.Background(), nzbFile.Files[0], 0, outDir, nil, nil)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %d, want 1", len(missing))
	}

	fi, err := os.Stat(filepath.Join(outDir, "tail.mkv"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() < int64(len(content)) {
		t.Fatalf("size = %d, shorter than the real payload %d — the tail hole was truncated away, par2 can no longer repair it",
			fi.Size(), len(content))
	}
}

// TestRejectsOutOfRangeYpartOffset: =ypart comes from a third-party article and
// is used verbatim as a WriteAt offset. An absurd value must be refused, not
// written — it would allocate wildly on a network mount and, worse, be
// persisted as the file's size, poisoning the truncate of every later retry.
func TestRejectsOutOfRangeYpartOffset(t *testing.T) {
	content := payload(64 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("evil.mkv", content, 16*1024)
	seg := nzbFile.Files[0].Segments[1]

	// Re-post part 2 claiming it starts a terabyte into the file.
	const huge = int64(1) << 40
	articles[seg.MessageID] = yenc.Encode("evil.mkv", 2, 4, huge, huge+16*1024-1,
		int64(len(content)), content[16*1024:32*1024])

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	outDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))
	dl.MissingTolerance = 0.5 // tolerate it so we can inspect the result

	_, missing, err := dl.DownloadFile(context.Background(), nzbFile.Files[0], 0, outDir, nil, nil)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %d, want the bogus segment rejected as unusable", len(missing))
	}

	fi, err := os.Stat(filepath.Join(outDir, "evil.mkv"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() >= huge {
		t.Fatalf("file grew to %d — the bogus =ypart offset was written", fi.Size())
	}
}

// TestCompleteButMissingFromDiskRedownloads: a tracker that says "all done"
// while the file is gone must NOT hand back a pre-allocated file of zeros.
func TestCompleteButMissingFromDiskRedownloads(t *testing.T) {
	content := payload(128 * 1024)
	nzbFile, articles := nntptest.BuildDirectFile("gone.mkv", content, 32*1024)

	srv := nntptest.NewFakeServer(t)
	srv.AddArticles(articles)

	outDir := t.TempDir()
	resumeDir := t.TempDir()
	dl := download.NewDownloader(connect(t, srv))
	file := nzbFile.Files[0]

	tracker := download.NewProgressTracker("task-gone", nzbFile, resumeDir)
	if _, _, err := dl.DownloadFile(context.Background(), file, 0, outDir, tracker, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The user deletes the file; the resume state still claims it's complete.
	dest := filepath.Join(outDir, "gone.mkv")
	if err := os.Remove(dest); err != nil {
		t.Fatalf("remove: %v", err)
	}

	resumed := download.NewProgressTracker("task-gone", nzbFile, resumeDir)
	if ok, err := resumed.Load(); !ok || err != nil {
		t.Fatalf("Load() = %v, %v", ok, err)
	}
	if _, _, err := dl.DownloadFile(context.Background(), file, 0, outDir, resumed, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("re-downloaded file differs from the payload (size %d, want %d) — it was served as zeros",
			len(got), len(content))
	}
}
