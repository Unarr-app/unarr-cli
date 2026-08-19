//go:build smoke

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// TestStalePieceCompletionSmoke reproduces the corruption trap behind repeated
// "damaged download" reports: the piece-completion DB says a torrent is done,
// but the data file was deleted (Cancel, a verify-failure cleanup, a user wipe).
// mmap storage recreates the file as zero-filled sparse and the client trusts
// the DB — BytesCompleted() reports complete instantly, and without the guard a
// zero-filled "movie" of the right size gets filed into the library.
//
// The test asserts (1) the trap is real — a fresh client over the same state
// dir claims the bytes are complete with no data on disk — and (2)
// verifyInheritedPieces catches it: after the forced re-hash no piece survives,
// and a re-download against the seeder produces the correct bytes.
// Run with: go test -tags smoke -run TestStalePieceCompletionSmoke ./internal/engine/
func TestStalePieceCompletionSmoke(t *testing.T) {
	// --- a real 4 MiB torrent + loopback seeder ---
	seedDir := t.TempDir()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "movie.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	var info metainfo.Info
	info.PieceLength = 256 << 10
	if err := info.BuildFromFilePath(filepath.Join(seedDir, "movie.bin")); err != nil {
		t.Fatalf("build info: %v", err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	scfg := torrent.NewDefaultClientConfig()
	scfg.DataDir = seedDir
	scfg.Seed = true
	scfg.NoDHT = true
	scfg.DisableTrackers = true
	scfg.ListenPort = 0
	seeder, err := torrent.NewClient(scfg)
	if err != nil {
		t.Fatalf("seeder client: %v", err)
	}
	defer seeder.Close()
	st, err := seeder.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("seeder add: %v", err)
	}
	<-st.GotInfo()
	st.DownloadAll()

	leechDir := t.TempDir()
	stateDir := t.TempDir() // piece-completion DB lives here, SEPARATE from the data

	newLeecher := func() *TorrentDownloader {
		dl, err := NewTorrentDownloader(TorrentConfig{
			DataDir:            leechDir,
			PieceCompletionDir: stateDir,
			ListenPort:         0,
		})
		if err != nil {
			t.Fatalf("downloader: %v", err)
		}
		return dl
	}

	// --- 1st session: download the torrent for real ---
	dl := newLeecher()
	lt, err := dl.client.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("leecher add: %v", err)
	}
	<-lt.GotInfo()
	lt.AddClientPeer(seeder)
	lt.DownloadAll()
	deadline := time.After(60 * time.Second)
	for lt.BytesMissing() > 0 {
		select {
		case <-deadline:
			t.Fatalf("first download did not complete: %s missing", formatBytes(lt.BytesMissing()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := dl.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// --- poison: the data disappears, the completion DB survives ---
	if err := os.Remove(filepath.Join(leechDir, "movie.bin")); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	// --- 2nd session (fresh client, same state): the DB claims completion ---
	dl2 := newLeecher()
	defer dl2.Shutdown(context.Background())
	lt2, err := dl2.client.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	<-lt2.GotInfo()
	if lt2.BytesCompleted() == 0 {
		// Not a failure of the guard — but the trap this test documents did not
		// arm, so the assertions below would prove nothing.
		t.Fatal("expected the piece-completion DB to (wrongly) claim completed bytes with no data on disk")
	}

	// --- the guard: re-hash inherited pieces instead of trusting the DB ---
	task := &Task{ID: "stale-completion-smoke"}
	if err := dl2.verifyInheritedPieces(context.Background(), lt2, task); err != nil {
		t.Fatalf("verifyInheritedPieces: %v", err)
	}
	if got := lt2.BytesCompleted(); got != 0 {
		for i := 0; i < lt2.NumPieces(); i++ {
			ps := lt2.PieceState(i)
			if ps.Complete {
				t.Logf("piece %d still complete: %+v", i, ps)
			}
		}
		t.Fatalf("after re-hash of a zeroed file, BytesCompleted = %s, want 0", formatBytes(got))
	}

	// --- and the re-download now produces the CORRECT bytes ---
	lt2.AddClientPeer(seeder)
	lt2.DownloadAll()
	deadline = time.After(60 * time.Second)
	for lt2.BytesMissing() > 0 {
		select {
		case <-deadline:
			t.Fatalf("re-download did not complete: %s missing", formatBytes(lt2.BytesMissing()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	got, err := os.ReadFile(filepath.Join(leechDir, "movie.bin"))
	if err != nil {
		t.Fatalf("read re-downloaded payload: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("re-downloaded payload differs from the original")
	}
}
