//go:build smoke

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// TestSeedLifecycleSmoke spins up a real loopback BitTorrent swarm: a seeder
// client serving a small file, and our TorrentDownloader's client leeching it.
// Once the leecher completes, the torrent is handed to seedAndDrop with a short
// SeedTime; the test asserts the lifecycle fires and the handle is dropped
// (removed from d.active). Exercises the real anacrolix Stats/Drop/ticker path,
// not mocks. Run with: go test -tags smoke -run TestSeedLifecycleSmoke ./internal/engine/
func TestSeedLifecycleSmoke(t *testing.T) {
	// --- seeder: a real client serving a 4 MiB file over loopback ---
	seedDir := t.TempDir()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i)
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
	scfg.ListenPort = 0 // random — never collides with the leecher's 42069
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
	st.DownloadAll() // verifies the existing pieces so the seeder is "complete"

	// --- leecher: our downloader, seeding enabled, very short seed time ---
	leechDir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:     leechDir,
		SeedEnabled: true,
		SeedTime:    1 * time.Second, // time target fires fast (no peers pull from us, so ratio stays 0)
	})
	if err != nil {
		t.Fatalf("downloader: %v", err)
	}
	dl.seedCheckInterval = 200 * time.Millisecond // poll fast so the 1s target is noticed promptly
	defer dl.Shutdown(context.Background())

	lt, err := dl.client.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("leecher add: %v", err)
	}
	<-lt.GotInfo()
	lt.AddClientPeer(seeder) // loopback peer — no DHT/tracker needed
	lt.DownloadAll()

	deadline := time.After(30 * time.Second)
	for lt.BytesMissing() > 0 {
		select {
		case <-deadline:
			t.Fatalf("download did not complete (missing %d bytes)", lt.BytesMissing())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Logf("leecher completed %d bytes", lt.BytesCompleted())

	// Track it as the daemon would for a seeding torrent, then run the lifecycle.
	const taskID = "smoke-seed-task-0001"
	dl.activeMu.Lock()
	dl.active[taskID] = lt
	dl.activeMu.Unlock()

	done := make(chan struct{})
	go func() {
		dl.seedAndDrop(taskID, lt, info.Length)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("seedAndDrop did not return within 10s")
	}

	dl.activeMu.Lock()
	_, stillTracked := dl.active[taskID]
	dl.activeMu.Unlock()
	if stillTracked {
		t.Error("torrent still tracked after seedAndDrop — lifecycle did not drop it")
	}
}
