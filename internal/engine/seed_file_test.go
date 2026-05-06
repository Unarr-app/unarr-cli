package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSeedFile_RejectsMissingFile — explicit error rather than crashing
// inside anacrolix when the path doesn't exist.
func TestSeedFile_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:        dir,
		ListenPort:     0,
		WebRTCEnabled:  true,
		WebRTCTrackers: []string{"wss://tracker.torrentclaw.com"},
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if _, err := SeedFile(dl.client, "/nonexistent/path", nil); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestSeedFile_RejectsDirectory — single-file torrents only for now.
func TestSeedFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:        dir,
		ListenPort:     0,
		WebRTCEnabled:  true,
		WebRTCTrackers: []string{"wss://tracker.torrentclaw.com"},
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := SeedFile(dl.client, subDir, nil); err == nil {
		t.Fatal("expected error for directory path")
	}
}

// TestSeedFile_BuildsDeterministicInfoHash — the same file should yield
// the same info_hash on every call so the web client can poll for it.
func TestSeedFile_BuildsDeterministicInfoHash(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.bin")
	payload := []byte("hello world — torrentclaw seed_file test")
	if err := os.WriteFile(file, payload, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	mkClient := func() *TorrentDownloader {
		dl, err := NewTorrentDownloader(TorrentConfig{
			DataDir:        t.TempDir(),
			ListenPort:     0,
			WebRTCEnabled:  true,
			WebRTCTrackers: []string{"wss://tracker.torrentclaw.com"},
		})
		if err != nil {
			t.Fatalf("NewTorrentDownloader: %v", err)
		}
		return dl
	}

	dl1 := mkClient()
	defer dl1.Shutdown(context.Background())
	hash1, err := SeedFile(dl1.client, file, []string{"wss://tracker.torrentclaw.com"})
	if err != nil {
		t.Fatalf("first SeedFile: %v", err)
	}

	dl2 := mkClient()
	defer dl2.Shutdown(context.Background())
	hash2, err := SeedFile(dl2.client, file, []string{"wss://tracker.torrentclaw.com"})
	if err != nil {
		t.Fatalf("second SeedFile: %v", err)
	}

	if hash1 != hash2 {
		t.Fatalf("info_hash not deterministic: %s vs %s", hash1.HexString(), hash2.HexString())
	}
	if hash1.HexString() == "" || len(hash1.HexString()) != 40 {
		t.Fatalf("info_hash is not 40 hex chars: %q", hash1.HexString())
	}
}

// TestSeedFileOnDownloader_RequiresWebRTC — silent failure mode is the
// worst UX; bail loud when the operator hasn't opted into WebRTC.
func TestSeedFileOnDownloader_RequiresWebRTC(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:       dir,
		ListenPort:    0,
		WebRTCEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	file := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := SeedFileOnDownloader(dl, file); err == nil {
		t.Fatal("expected error when WebRTC disabled")
	}
}

// TestChooseSeedPieceLength_LadderShape — sanity-check the breakpoints
// stay aligned with the libtorrent reference (16 KiB → 4 MiB).
func TestChooseSeedPieceLength_LadderShape(t *testing.T) {
	cases := []struct {
		size   int64
		expect int64
	}{
		{1, 16 * 1024},
		{4 * 1024 * 1024, 64 * 1024},
		{64 * 1024 * 1024, 256 * 1024},
		{512 * 1024 * 1024, 1024 * 1024},
		{4 * 1024 * 1024 * 1024, 4 * 1024 * 1024},
	}
	for _, c := range cases {
		if got := chooseSeedPieceLength(c.size); got != c.expect {
			t.Errorf("chooseSeedPieceLength(%d) = %d want %d", c.size, got, c.expect)
		}
	}
}

// TestMakeAnnounceList_HandlesEmpty — nil/empty in → nil out, so
// AddTorrent doesn't see a dangling tier with no URLs.
func TestMakeAnnounceList_HandlesEmpty(t *testing.T) {
	if got := makeAnnounceList(nil); got != nil {
		t.Errorf("nil input should yield nil announce list, got %+v", got)
	}
	if got := makeAnnounceList([]string{}); got != nil {
		t.Errorf("empty input should yield nil announce list, got %+v", got)
	}
	if got := makeAnnounceList([]string{"", " ", ""}); got != nil {
		// Empty strings should be filtered; if everything is empty,
		// nil is the right answer.
		// (We do NOT trim whitespace today — only literal "".)
		if len(got) != 1 || len(got[0]) != 1 {
			t.Errorf("expected 1 single-element tier, got %+v", got)
		}
	}
	got := makeAnnounceList([]string{"wss://a", "", "wss://b"})
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("expected 1 tier of 2 URLs, got %+v", got)
	}
}
