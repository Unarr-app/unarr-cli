package stream

import (
	"errors"
	"testing"
)

// storeChunk is a convenience builder for a stored, unencrypted file chunk.
func storeChunk(name string, vol int, dataOff, pack, unp int64) rarChunk {
	return rarChunk{
		name: name, volIndex: vol, dataOffset: dataOff,
		packSize: pack, unpSize: unp, stored: true,
	}
}

func TestSelectVideoHappySingleFileMultiVolume(t *testing.T) {
	chunks := []rarChunk{
		storeChunk("movie.mkv", 0, 100, 3000, 5000),
		storeChunk("movie.mkv", 1, 80, 2000, 5000),
	}
	group, err := selectVideo(chunks)
	if err != nil {
		t.Fatalf("selectVideo: %v", err)
	}
	if len(group) != 2 || group[0].volIndex != 0 || group[1].volIndex != 1 {
		t.Fatalf("group not ordered by volume: %+v", group)
	}
}

func TestSelectVideoOrdersByVolume(t *testing.T) {
	// Chunks supplied out of order must come back volume-ascending.
	chunks := []rarChunk{
		storeChunk("movie.mkv", 2, 10, 1000, 3000),
		storeChunk("movie.mkv", 0, 10, 1000, 3000),
		storeChunk("movie.mkv", 1, 10, 1000, 3000),
	}
	group, err := selectVideo(chunks)
	if err != nil {
		t.Fatalf("selectVideo: %v", err)
	}
	for i, c := range group {
		if c.volIndex != i {
			t.Fatalf("chunk %d has volIndex %d", i, c.volIndex)
		}
	}
}

func TestSelectVideoRejectsEncrypted(t *testing.T) {
	c := storeChunk("movie.mkv", 0, 10, 100, 100)
	c.encrypted = true
	assertSelectReject(t, []rarChunk{c}, "encrypted")
}

func TestSelectVideoRejectsCompressed(t *testing.T) {
	c := storeChunk("movie.mkv", 0, 10, 100, 100)
	c.stored = false
	assertSelectReject(t, []rarChunk{c}, "compressed")
}

func TestSelectVideoRejectsNoVideo(t *testing.T) {
	chunks := []rarChunk{storeChunk("readme.txt", 0, 10, 100, 100)}
	assertSelectReject(t, chunks, "no video")
}

func TestSelectVideoRejectsAmbiguous(t *testing.T) {
	chunks := []rarChunk{
		storeChunk("movie.mkv", 0, 10, 100, 100),
		storeChunk("extra.mp4", 0, 10, 100, 100),
	}
	assertSelectReject(t, chunks, "ambiguous")
}

func TestSelectVideoIgnoresNonVideoSiblings(t *testing.T) {
	// A single video plus a non-video sibling (e.g. an nfo stored in the archive)
	// is unambiguous: the video is picked, the sibling ignored.
	chunks := []rarChunk{
		storeChunk("movie.mkv", 0, 10, 500, 500),
		storeChunk("movie.nfo", 0, 600, 20, 20),
	}
	group, err := selectVideo(chunks)
	if err != nil {
		t.Fatalf("selectVideo: %v", err)
	}
	if len(group) != 1 || group[0].name != "movie.mkv" {
		t.Fatalf("expected the mkv, got %+v", group)
	}
}

func TestBuildExtentsLayoutAndTotal(t *testing.T) {
	group := []rarChunk{
		storeChunk("movie.mkv", 0, 52, 3000, 5000),
		storeChunk("movie.mkv", 1, 48, 2000, 5000),
	}
	extents, total, err := buildExtents(group)
	if err != nil {
		t.Fatalf("buildExtents: %v", err)
	}
	if total != 5000 {
		t.Fatalf("total = %d, want 5000", total)
	}
	want := []extent{
		{videoStart: 0, length: 3000, volIndex: 0, dataOffset: 52},
		{videoStart: 3000, length: 2000, volIndex: 1, dataOffset: 48},
	}
	for i, e := range want {
		if extents[i] != e {
			t.Fatalf("extent %d = %+v, want %+v", i, extents[i], e)
		}
	}
}

func TestBuildExtentsRejectsSizeMismatch(t *testing.T) {
	group := []rarChunk{
		storeChunk("movie.mkv", 0, 52, 3000, 9999), // unp 9999 != summed 5000
		storeChunk("movie.mkv", 1, 48, 2000, 9999),
	}
	_, _, err := buildExtents(group)
	if !errors.Is(err, ErrNotStreamable) {
		t.Fatalf("expected NotStreamable for size mismatch, got %v", err)
	}
}

func TestVolumeOrder(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"release.rar", 0},
		{"release.r00", 1},
		{"release.r01", 2},
		{"release.part01.rar", 1},
		{"release.part02.rar", 2},
		{"release.002", 2},
	}
	for _, c := range cases {
		if got := volumeOrder(c.name); got != c.want {
			t.Errorf("volumeOrder(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func assertSelectReject(t *testing.T, chunks []rarChunk, substr string) {
	t.Helper()
	_, err := selectVideo(chunks)
	if !errors.Is(err, ErrNotStreamable) {
		t.Fatalf("expected NotStreamable, got %v", err)
	}
	var nse *NotStreamableError
	if errors.As(err, &nse) && substr != "" && !contains(nse.Reason, substr) {
		t.Fatalf("reason %q does not mention %q", nse.Reason, substr)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
