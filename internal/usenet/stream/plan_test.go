package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// planFake posts an NZB's articles to a fake NNTP server and runs the classifier
// through the real fetch path — the same client the streaming reader uses.
func planFake(t *testing.T, n *nzb.NZB, articles map[string][]byte) *StreamPlan {
	t.Helper()
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)
	return StreamPlanFromNZB(context.Background(), c, n)
}

// assertStreams verifies a plan is streamable, reports the expected kind/size, and
// streams the video back byte-for-byte through a freshly opened reader.
func assertStreams(t *testing.T, p *StreamPlan, want Kind, name string, content []byte) {
	t.Helper()
	if !p.Streamable() {
		t.Fatalf("plan not streamable: kind=%s reason=%q", p.Kind, p.Reason)
	}
	if p.Kind != want {
		t.Fatalf("Kind = %s, want %s", p.Kind, want)
	}
	if p.VideoName != name {
		t.Errorf("VideoName = %q, want %q", p.VideoName, name)
	}
	if p.VideoSize != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d", p.VideoSize, len(content))
	}
	rd := p.Open(context.Background())
	if rd == nil {
		t.Fatal("Open returned nil for a streamable plan")
	}
	defer func() { _ = rd.Close() }()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed %d bytes; content mismatch", len(got))
	}
}

// assertUnsupported verifies a plan fell back and mentions the expected cause.
func assertUnsupported(t *testing.T, p *StreamPlan, substr string) {
	t.Helper()
	if p.Streamable() {
		t.Fatalf("expected unsupported, got streamable kind=%s", p.Kind)
	}
	if p.Kind != KindUnsupported {
		t.Fatalf("Kind = %s, want unsupported", p.Kind)
	}
	if p.Open(context.Background()) != nil {
		t.Fatal("unsupported plan must Open to nil")
	}
	if substr != "" && !strings.Contains(p.Reason, substr) {
		t.Fatalf("reason %q does not mention %q", p.Reason, substr)
	}
}

func TestStreamPlanDirectFile(t *testing.T) {
	content := patternBytes(20_000)
	n, articles := nntptest.BuildDirectFile("movie.2024.1080p.mkv", content, 4096)
	p := planFake(t, n, articles)
	assertStreams(t, p, KindDirect, "movie.2024.1080p.mkv", content)
}

func TestStreamPlanDirectFileMp4(t *testing.T) {
	content := patternBytes(9_999)
	n, articles := nntptest.BuildDirectFile("clip.mp4", content, 1500)
	p := planFake(t, n, articles)
	assertStreams(t, p, KindDirect, "clip.mp4", content)
}

func TestStreamPlanRar4Store(t *testing.T) {
	content := patternBytes(25_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, 8000, 1200)
	p := planFake(t, n, articles)
	assertStreams(t, p, KindRarStore, "show.s01e01.mkv", content)
}

func TestStreamPlanRar5Store(t *testing.T) {
	content := patternBytes(16_384)
	n, articles := nntptest.BuildRarStore5("movie.2024.mkv", content, 6000, 1000)
	p := planFake(t, n, articles)
	assertStreams(t, p, KindRarStore, "movie.2024.mkv", content)
}

func TestStreamPlanPasswordUnsupported(t *testing.T) {
	content := patternBytes(4_000)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, 1000)
	n.Password = "hunter2" // NZB-level password meta => never stream
	p := planFake(t, n, articles)
	assertUnsupported(t, p, "password")
}

func TestStreamPlanRarCompressedUnsupported(t *testing.T) {
	content := patternBytes(12_000)
	n, articles := nntptest.BuildRarCompressed("movie.mkv", content, 5000, 1000)
	p := planFake(t, n, articles)
	assertUnsupported(t, p, "compressed")
}

func TestStreamPlanRarEncryptedUnsupported(t *testing.T) {
	content := patternBytes(12_000)
	n, articles := nntptest.BuildRarEncrypted("movie.mkv", content, 5000, 1000)
	p := planFake(t, n, articles)
	assertUnsupported(t, p, "encrypted")
}

func TestStreamPlanNoVideoUnsupported(t *testing.T) {
	// A directly-posted non-video content file (mp3) has no streamable video.
	content := patternBytes(3_000)
	n, articles := nntptest.BuildDirectFile("soundtrack.mp3", content, 800)
	p := planFake(t, n, articles)
	assertUnsupported(t, p, "no video")
}

func TestStreamPlanMultipleVideosUnsupported(t *testing.T) {
	// Two directly-posted videos is ambiguous — which one do we play? Fall back.
	a, aa := nntptest.BuildDirectFile("movieA.mkv", patternBytes(2_000), 700)
	b, ba := nntptest.BuildDirectFile("movieB.mkv", patternBytes(2_500), 700)
	n, articles := mergeNZBs(a, b, aa, ba)
	p := planFake(t, n, articles)
	assertUnsupported(t, p, "multiple video")
}

func TestStreamPlanEmptyUnsupported(t *testing.T) {
	if got := StreamPlanFromNZB(context.Background(), nil, nil); got.Streamable() {
		t.Fatal("nil NZB must be unsupported")
	}
	empty := &nzb.NZB{Meta: map[string]string{}}
	if got := StreamPlanFromNZB(context.Background(), nil, empty); got.Streamable() {
		t.Fatal("empty NZB must be unsupported")
	}
}

func TestStreamPlanDirectUnreachableFallsBack(t *testing.T) {
	// The video's first article is permanently unreachable: the size probe fails,
	// so the classifier degrades to unsupported (caller downloads via batch)
	// instead of committing to a stream that would immediately stall.
	content := patternBytes(4_096)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, 2048)

	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	s.FailNext(100, 430) // every fetch 430s → the size probe's retries are exhausted
	c := dialFake(t, s)

	p := StreamPlanFromNZB(context.Background(), c, n)
	assertUnsupported(t, p, "establish size")
}

// mergeNZBs concatenates two synthetic NZBs and their article maps into one.
func mergeNZBs(a, b *nzb.NZB, aa, ba map[string][]byte) (*nzb.NZB, map[string][]byte) {
	n := &nzb.NZB{Meta: map[string]string{}}
	n.Files = append(n.Files, a.Files...)
	n.Files = append(n.Files, b.Files...)
	arts := make(map[string][]byte, len(aa)+len(ba))
	for k, v := range aa {
		arts[k] = v
	}
	for k, v := range ba {
		arts[k] = v
	}
	return n, arts
}
