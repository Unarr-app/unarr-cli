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

	tor, closeClient := openVerifiedTorrent(t, dataDir, &mi)
	defer closeClient()

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

// The poll loop's completion test must be on the SAME scale as sel.totalBytes.
// t.BytesCompleted() is not: it counts verified pieces of files we never
// selected, so it crosses a selection-sized total while the selection is still
// unfinished — the loop declares complete and the integrity guard then fails a
// healthy download. Same mixing of scales as the original bug, smaller N.
func TestSelectionCompletedBytesIsNotTorrentWide(t *testing.T) {
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

	// The unselected file is fully present; the SELECTED video is only half
	// there. A finished-looking byte count, an unfinished download.
	half := append([]byte(nil), video...)
	for i := 4 * pieceLen; i < len(half); i++ {
		half[i] = 0
	}
	dataDir := t.TempDir()
	packDst := filepath.Join(dataDir, "Pack")
	mustMkdir(t, packDst)
	mustWrite(t, filepath.Join(packDst, "extra.bin"), extra)
	mustWrite(t, filepath.Join(packDst, "movie.mkv"), half)

	tor, closeClient := openVerifiedTorrent(t, dataDir, &mi)
	defer closeClient()

	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, "completion-scale")

	// The old exit test would already have fired here.
	if tor.BytesCompleted() < sel.totalBytes {
		t.Fatalf("trap did not arm: BytesCompleted %d < totalBytes %d", tor.BytesCompleted(), sel.totalBytes)
	}
	// The selection-scoped one correctly says we are only half done.
	if got := sel.completedBytes(tor); got >= sel.totalBytes {
		t.Fatalf("completedBytes = %d, want < %d — half the video is missing", got, sel.totalBytes)
	}
	if got, want := sel.missingBytes(tor), int64(4*pieceLen); got != want {
		t.Fatalf("missingBytes = %d, want %d", got, want)
	}
}

// A single-file torrent takes the files==nil path, and the guard must be real
// there too — it is the commonest case of all.
//
// Honest limit: this asserts the RESULT, and for one file on disk
// t.BytesMissing() would produce the same number. The two only diverge on
// received-but-unhashed chunks, which BytesMissing credits and the piece-bitmap
// count does not, and that needs a live download with peers to reproduce. The
// reason files==nil is no longer delegated to BytesMissing is the loop, not
// this number: with the exit test now reading completedBytes = totalBytes -
// missingBytes, delegating would make the loop exit exactly when BytesMissing
// hit 0, so the guard could never see a non-zero value. See
// TestSelectionCompletedBytesIsNotTorrentWide.
func TestSelectionMissingBytesGuardsSingleFileTorrents(t *testing.T) {
	const pieceLen = 32 << 10
	movie := bytesPattern(8 * pieceLen)

	srcDir := t.TempDir()
	moviePath := filepath.Join(srcDir, "movie.mkv")
	mustWrite(t, moviePath, movie)

	var info metainfo.Info
	info.PieceLength = pieceLen
	if err := info.BuildFromFilePath(moviePath); err != nil {
		t.Fatalf("build info: %v", err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	damaged := append([]byte(nil), movie...)
	for i := 6 * pieceLen; i < len(damaged); i++ {
		damaged[i] = 0
	}
	dataDir := t.TempDir()
	mustWrite(t, filepath.Join(dataDir, "movie.mkv"), damaged)

	tor, closeClient := openVerifiedTorrent(t, dataDir, &mi)
	defer closeClient()

	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, "single-file")
	if sel.files != nil {
		t.Fatalf("a single-file torrent must take the DownloadAll path, got %d selected files", len(sel.files))
	}
	if got, want := sel.missingBytes(tor), int64(2*pieceLen); got != want {
		t.Fatalf("missingBytes = %d, want %d — the guard must not be dead here", got, want)
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

	tor, closeClient := openVerifiedTorrent(t, dataDir, &mi)
	defer closeClient()

	d := &TorrentDownloader{}
	sel := d.selectFiles(tor, "selection-test-damaged")

	if got := sel.missingBytes(tor); got != 2*pieceLen {
		t.Fatalf("selection.missingBytes = %d, want %d (the two corrupted video pieces)", got, 2*pieceLen)
	}
}

// openVerifiedTorrent adds mi against dataDir on a networkless client, re-hashes
// every piece, and waits for the completion flags to settle. Returns the torrent
// and a close func.
//
// The settle is not optional: VerifyDataContext returns once pieces are HASHED,
// but the completion write for a piece that FAILED can still be in flight, and
// reading state in that window reports it complete — without this the damaged
// cases passed 2 runs in 3. Production settles at the same point for the same
// reason (see waitPieceMarkingSettled).
//
// The client opens NO sockets at all. These tests only re-hash data already on
// disk — no peer ever connects — so a listener is dead weight, and it was the
// source of Windows CI flakes: with ListenPort 0 anacrolix binds tcp4 on a
// random port and then binds udp4 (uTP) on the SAME number (listenAllRetry in
// anacrolix/torrent socket.go). On Windows that number can sit inside a
// Hyper-V/WinNAT excluded UDP range, the bind fails with WSAEACCES ("subsequent
// listen: ... forbidden by its access permissions"), and anacrolix retries only
// on addr-in-use. With TCP and uTP disabled and NoDHT set, listenNetworks() is
// empty, listenAll returns no sockets and NewClient accepts that — nothing left
// to collide. The assertions are unaffected: storage, piece hashing and the
// completion bitmap never touch the network.
func openVerifiedTorrent(t *testing.T, dataDir string, mi *metainfo.MetaInfo) (*torrent.Torrent, func()) {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.Seed = false
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.NoDefaultPortForwarding = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if addrs := client.ListenAddrs(); len(addrs) != 0 {
		client.Close()
		t.Fatalf("networkless client still listens on %v — the Windows port-range flake is back", addrs)
	}
	tor, err := client.AddTorrent(mi)
	if err != nil {
		client.Close()
		t.Fatalf("add: %v", err)
	}
	<-tor.GotInfo()
	if err := tor.VerifyDataContext(context.Background()); err != nil {
		client.Close()
		t.Fatalf("verify: %v", err)
	}
	waitPieceMarkingSettled(context.Background(), tor)
	return tor, func() { client.Close() }
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
