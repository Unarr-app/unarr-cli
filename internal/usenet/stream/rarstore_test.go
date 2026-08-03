package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// probeFake posts an NZB's articles to a fake NNTP server and probes its RAR
// volumes through the real fetch path — the same client the streaming reader uses.
func probeFake(t *testing.T, n *nzb.NZB, articles map[string][]byte) (*RarStore, error) {
	t.Helper()
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)
	return Probe(context.Background(), c, n.RarFiles())
}

func TestProbeRar4StoreStreamsVideoExact(t *testing.T) {
	content := patternBytes(20_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, 7000, 1200)

	rs, err := probeFake(t, n, articles)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if rs.VideoName() != "show.s01e01.mkv" {
		t.Errorf("VideoName = %q", rs.VideoName())
	}
	if rs.VideoSize() != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d", rs.VideoSize(), len(content))
	}

	rd := rs.OpenVideo(context.Background())
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed %d bytes; content mismatch across volumes", len(got))
	}
}

func TestProbeRar4StoreSingleVolume(t *testing.T) {
	content := patternBytes(5_000)
	n, articles := nntptest.BuildRarStore("clip.mkv", content, 1<<20, 900)

	rs, err := probeFake(t, n, articles)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	rd := rs.OpenVideo(context.Background())
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("single-volume store did not stream verbatim")
	}
}

func TestProbeRar5StoreStreamsVideoExact(t *testing.T) {
	content := patternBytes(16_384)
	n, articles := nntptest.BuildRarStore5("movie.2024.mkv", content, 6000, 1000)

	rs, err := probeFake(t, n, articles)
	if err != nil {
		t.Fatalf("Probe RAR5: %v", err)
	}
	if rs.VideoName() != "movie.2024.mkv" {
		t.Errorf("VideoName = %q", rs.VideoName())
	}
	if rs.VideoSize() != int64(len(content)) {
		t.Fatalf("VideoSize = %d, want %d", rs.VideoSize(), len(content))
	}
	rd := rs.OpenVideo(context.Background())
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll RAR5: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("RAR5 store did not stream verbatim across volumes")
	}
}

func TestRarVideoReaderSeek(t *testing.T) {
	content := patternBytes(18_000)
	n, articles := nntptest.BuildRarStore("show.s01e02.mkv", content, 5000, 1100)
	rs, err := probeFake(t, n, articles)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	rd := rs.OpenVideo(context.Background())
	defer rd.Close()

	// ServeContent's opening move: Seek(0,End) must report the exact video size.
	end, err := rd.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if end != int64(len(content)) {
		t.Fatalf("Seek(0,End) = %d, want %d", end, len(content))
	}

	// A seek deep into a later volume must return exact bytes (crossing the video
	// offset -> container offset translation).
	const off, span = 12_345, 800
	if _, err := rd.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek mid: %v", err)
	}
	buf := make([]byte, span)
	if _, err := io.ReadFull(rd, buf); err != nil {
		t.Fatalf("ReadFull mid: %v", err)
	}
	if !bytes.Equal(buf, content[off:off+span]) {
		t.Fatal("mid seek returned wrong bytes")
	}

	// Seek backwards into an earlier volume.
	if _, err := rd.Seek(100, io.SeekStart); err != nil {
		t.Fatalf("Seek back: %v", err)
	}
	if _, err := io.ReadFull(rd, buf); err != nil {
		t.Fatalf("ReadFull back: %v", err)
	}
	if !bytes.Equal(buf, content[100:100+span]) {
		t.Fatal("backward seek returned wrong bytes")
	}
}

func TestProbeRejectsCompressed(t *testing.T) {
	content := patternBytes(12_000)
	n, articles := nntptest.BuildRarCompressed("movie.mkv", content, 5000, 1000)

	_, err := probeFake(t, n, articles)
	assertNotStreamable(t, err, "compressed")
}

func TestProbeRejectsEncrypted(t *testing.T) {
	content := patternBytes(12_000)
	n, articles := nntptest.BuildRarEncrypted("movie.mkv", content, 5000, 1000)

	_, err := probeFake(t, n, articles)
	assertNotStreamable(t, err, "encrypted")
}

func TestProbeRejectsNoVolumes(t *testing.T) {
	_, err := Probe(context.Background(), nil, nil)
	assertNotStreamable(t, err, "")
}

// TestReaderVolumeReadAtBoundsHugeLength guards the resilience invariant that an
// untrusted, oversized length (e.g. a corrupt RAR5 HeaderSize vint) is rejected
// as a clean read error instead of reaching make([]byte, n) and OOM-crashing the
// daemon. A near-maxInt64 n must NOT allocate.
func TestReaderVolumeReadAtBoundsHugeLength(t *testing.T) {
	content := patternBytes(4_000)
	n, articles := nntptest.BuildDirectFile("vol.rar", content, 1000)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	v, err := newReaderVolume(context.Background(), c, n.Files[0], nil)
	if err != nil {
		t.Fatalf("newReaderVolume: %v", err)
	}
	defer v.close()

	if got := v.size(); got != int64(len(content)) {
		t.Fatalf("size = %d, want %d", got, len(content))
	}

	// A length far past the volume (would allocate ~9 EiB) must be an error.
	if _, err := v.readAt(0, 1<<62); err == nil {
		t.Fatal("readAt with huge n returned nil error; expected rejection before allocation")
	}
	// off+n overflowing int64 must also be rejected, not wrap negative.
	if _, err := v.readAt(v.size()-1, 1<<62); err == nil {
		t.Fatal("readAt with overflowing off+n returned nil error")
	}
	// A read that runs one byte off the end is still rejected.
	if _, err := v.readAt(0, v.size()+1); err == nil {
		t.Fatal("readAt one byte past end returned nil error")
	}
	// An in-bounds read still succeeds and returns exact bytes.
	got, err := v.readAt(10, 20)
	if err != nil {
		t.Fatalf("in-bounds readAt: %v", err)
	}
	if !bytes.Equal(got, content[10:30]) {
		t.Fatal("in-bounds readAt returned wrong bytes")
	}
}

// assertNotStreamable checks err is a NotStreamableError and, when substr is
// non-empty, that its reason mentions it.
func assertNotStreamable(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected NotStreamable, got nil")
	}
	if !errors.Is(err, ErrNotStreamable) {
		t.Fatalf("error %v is not NotStreamable", err)
	}
	var nse *NotStreamableError
	if !errors.As(err, &nse) {
		t.Fatalf("error %v does not unwrap to *NotStreamableError", err)
	}
	if substr != "" && !strings.Contains(nse.Reason, substr) {
		t.Fatalf("reason %q does not mention %q", nse.Reason, substr)
	}
}
