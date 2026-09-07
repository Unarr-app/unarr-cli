package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// The bug: "corrupt download: torrent reported complete but N of verified
// pieces are still missing" fired on downloads that were perfectly fine.
// 32 failures over 12 distinct users in 14 days — 15% of every torrent failure,
// with N up to 96.1 GB — because the guard asked t.BytesMissing() (whole
// torrent) about a download selectFiles had deliberately made partial.

func TestMissingSelectedBytes_IgnoresUnselectedFiles(t *testing.T) {
	const piece = int64(1000)
	// Two files: the selected video [0,3000) and a skipped extra [3000,9000).
	// Only the video's pieces are verified — exactly the shape of every prod
	// failure, where the "missing" bytes were the pack we never asked for.
	selected := []fileExtent{{offset: 0, length: 3000}}
	verified := func(i int) bool { return i < 3 } // pieces 0..2 = the video

	if got := missingSelectedBytes(piece, selected, verified); got != 0 {
		t.Fatalf("missingSelectedBytes = %d, want 0 — the skipped 6000 bytes are not damage", got)
	}
}

func TestMissingSelectedBytes_StillCatchesRealShortfall(t *testing.T) {
	const piece = int64(1000)
	selected := []fileExtent{{offset: 0, length: 3000}}
	// Piece 1 of the video failed its hash: that IS damage and must be reported.
	verified := func(i int) bool { return i != 1 }

	if got := missingSelectedBytes(piece, selected, verified); got != 1000 {
		t.Fatalf("missingSelectedBytes = %d, want 1000 (the unverified video piece)", got)
	}
}

func TestMissingSelectedBytes_CountsOnlyTheSelectedShareOfASharedPiece(t *testing.T) {
	const piece = int64(1000)
	// The video ends mid-piece-2 (at 2400) and an unselected file continues in
	// the same piece. With piece 2 unverified, only the video's 400 bytes count.
	selected := []fileExtent{{offset: 0, length: 2400}}
	verified := func(i int) bool { return i != 2 }

	if got := missingSelectedBytes(piece, selected, verified); got != 400 {
		t.Fatalf("missingSelectedBytes = %d, want 400 (the video's share of piece 2)", got)
	}
}

func TestMissingSelectedBytes_NoDoubleCountAcrossSelectedFiles(t *testing.T) {
	const piece = int64(1000)
	// A video and its .srt sitting in the same unverified piece: each contributes
	// its own bytes, and the total can never exceed the piece.
	selected := []fileExtent{{offset: 0, length: 600}, {offset: 600, length: 400}}
	verified := func(int) bool { return false }

	if got := missingSelectedBytes(piece, selected, verified); got != 1000 {
		t.Fatalf("missingSelectedBytes = %d, want 1000 (600+400, counted once)", got)
	}
}

func TestMissingSelectedBytes_Degenerate(t *testing.T) {
	verified := func(int) bool { return false }
	if got := missingSelectedBytes(0, []fileExtent{{0, 100}}, verified); got != 0 {
		t.Errorf("pieceLength 0: got %d, want 0", got)
	}
	if got := missingSelectedBytes(1000, nil, verified); got != 0 {
		t.Errorf("no files: got %d, want 0", got)
	}
	if got := missingSelectedBytes(1000, []fileExtent{{0, 0}}, verified); got != 0 {
		t.Errorf("empty file: got %d, want 0", got)
	}
}

func TestOverlapBytes(t *testing.T) {
	cases := []struct{ aS, aE, bS, bE, want int64 }{
		{0, 100, 0, 100, 100},   // identical
		{0, 100, 50, 150, 50},   // partial
		{0, 100, 100, 200, 0},   // touching, no overlap
		{0, 100, 200, 300, 0},   // disjoint
		{50, 150, 0, 1000, 100}, // fully contained
		{0, 1000, 50, 150, 100}, // fully containing
	}
	for _, c := range cases {
		if got := overlapBytes(c.aS, c.aE, c.bS, c.bE); got != c.want {
			t.Errorf("overlapBytes(%d,%d,%d,%d) = %d, want %d", c.aS, c.aE, c.bS, c.bE, got, c.want)
		}
	}
}

// TestSelectionMissingBytesAgainstARealTorrent reproduces the prod false
// positive end-to-end against a real anacrolix client — no network, no seeder.
//
// A two-file torrent is laid on disk with ONLY the video present, which is the
// on-disk state a finished selective download leaves behind. After a real
// re-hash, the whole-torrent accounting screams (that was the bug) while the
// selection-aware accounting correctly says nothing is missing.
func TestSelectionMissingBytesAgainstARealTorrent(t *testing.T) {
	const pieceLen = 32 << 10
	// Both files are whole multiples of the piece length so neither shares a
	// piece with the other — the piece-boundary case is covered by the pure
	// tests above, and here it would just muddy the assertion.
	video := bytesPattern(8 * pieceLen)
	extra := bytesPattern(8 * pieceLen)

	// Build the metainfo from a complete copy of the pack.
	srcDir := t.TempDir()
	packSrc := filepath.Join(srcDir, "Pack")
	mustMkdir(t, packSrc)
	mustWrite(t, filepath.Join(packSrc, "extra.bin"), extra) // sorts first → offset 0
	mustWrite(t, filepath.Join(packSrc, "movie.mkv"), video)

	var info metainfo.Info
	info.PieceLength = pieceLen
	if err := info.BuildFromFilePath(packSrc); err != nil {
		t.Fatalf("build info: %v", err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	// The agent's data dir holds ONLY the selected video.
	dataDir := t.TempDir()
	packDst := filepath.Join(dataDir, "Pack")
	mustMkdir(t, packDst)
	mustWrite(t, filepath.Join(packDst, "movie.mkv"), video)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.Seed = false
	cfg.ListenPort = 0
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	tor, err := client.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	<-tor.GotInfo()
	if err := tor.VerifyDataContext(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// VerifyData returns once every piece has been HASHED, but the completion
	// flag for a failed piece can still be in flight — without this the damaged
	// case intermittently sees one of its two bad pieces as complete. Same
	// settling the download loop does; see waitPieceMarkingSettled.
	waitPieceMarkingSettled(context.Background(), tor)

	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, "selection-test")

	if len(sel.files) == 0 {
		t.Fatal("selectFiles picked nothing — the rest of this test would prove nothing")
	}
	if sel.totalBytes != int64(len(video)) {
		t.Fatalf("selected totalBytes = %d, want the video's %d", sel.totalBytes, len(video))
	}

	// The old guard's input: the skipped file reads as missing bytes.
	if tor.BytesMissing() == 0 {
		t.Fatal("expected the whole-torrent accounting to report the skipped file as missing; the trap did not arm")
	}
	// The fix: nothing we asked for is missing, so this download is complete.
	if got := sel.missingBytes(tor); got != 0 {
		t.Fatalf("selection.missingBytes = %d, want 0 — the video is fully verified on disk", got)
	}
}

// TestSelectionMissingBytesCatchesATruncatedSelectedFile is the other half: the
// guard must still fire when the file we DID ask for is short.
func TestSelectionMissingBytesCatchesATruncatedSelectedFile(t *testing.T) {
	const pieceLen = 32 << 10
	video := bytesPattern(8 * pieceLen)
	extra := bytesPattern(8 * pieceLen)

	srcDir := t.TempDir()
	packSrc := filepath.Join(srcDir, "Pack")
	mustMkdir(t, packSrc)
	mustWrite(t, filepath.Join(packSrc, "extra.bin"), extra)
	mustWrite(t, filepath.Join(packSrc, "movie.mkv"), video)

	var info metainfo.Info
	info.PieceLength = pieceLen
	if err := info.BuildFromFilePath(packSrc); err != nil {
		t.Fatalf("build info: %v", err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	// Only the video is present, and its last two pieces are zeroed — a real
	// short/damaged file rather than a deselected one.
	damaged := append([]byte(nil), video...)
	for i := 6 * pieceLen; i < len(damaged); i++ {
		damaged[i] = 0
	}
	dataDir := t.TempDir()
	packDst := filepath.Join(dataDir, "Pack")
	mustMkdir(t, packDst)
	mustWrite(t, filepath.Join(packDst, "movie.mkv"), damaged)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.Seed = false
	cfg.ListenPort = 0
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	tor, err := client.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	<-tor.GotInfo()
	if err := tor.VerifyDataContext(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// VerifyData returns once every piece has been HASHED, but the completion
	// flag for a failed piece can still be in flight — without this the damaged
	// case intermittently sees one of its two bad pieces as complete. Same
	// settling the download loop does; see waitPieceMarkingSettled.
	waitPieceMarkingSettled(context.Background(), tor)

	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, "selection-test-damaged")

	if got := sel.missingBytes(tor); got != 2*pieceLen {
		t.Fatalf("selection.missingBytes = %d, want %d (the two corrupted video pieces)", got, 2*pieceLen)
	}
}

func bytesPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 1) // never all-zero, so a zeroed region really differs
	}
	return b
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
