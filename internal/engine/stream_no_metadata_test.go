package engine

import (
	"context"
	"strings"
	"testing"
)

// A task is registered in d.active as soon as the magnet is added, and the
// web's stream flag is re-sent on every sync, so a stream request lands
// routinely while the torrent is still waiting for metadata. Files() panics
// there (it dereferences the nil t.files), which took the whole daemon down —
// crash report from agent v1.12.1, engine/torrent.go:980.
func TestGetStreamProvider_NoMetadataYet(t *testing.T) {
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("downloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	// A magnet nobody can resolve: metadata never arrives.
	tor, err := dl.client.AddMagnet("magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&dn=no-metadata")
	if err != nil {
		t.Fatalf("add magnet: %v", err)
	}
	dl.activeMu.Lock()
	dl.active["task-no-meta"] = tor
	dl.activeMu.Unlock()

	provider, err := dl.GetStreamProvider("task-no-meta")
	if err == nil {
		t.Fatalf("GetStreamProvider = %v, nil error; want an error while metadata is missing", provider)
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error = %q, want it to name the missing metadata", err)
	}
}
