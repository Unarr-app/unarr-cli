package engine

import (
	"log"
	"os"

	"github.com/anacrolix/torrent/storage"
)

// PieceCompletionDBName is the bolt piece-completion DB inside the agent state
// dir (or the download dir for the one-shot `unarr download`). The name is the
// one anacrolix/torrent's own bolt backend used through 1.11.x, kept so those
// files carry over — see piece_completion_bolt.go for why the backend is ours.
const PieceCompletionDBName = ".torrent.bolt.db"

// newTorrentStore picks the anacrolix storage backend for the downloader.
//
// mmap instead of the default file backend: the library author notes file
// storage has "very high system overhead"; mmap improves I/O throughput and
// piece verification speed significantly.
//
// The piece-completion DB lives in PieceCompletionDir when set (the daemon
// passes the agent state dir, keeping it off NFS/SMB where file locking times
// out) and in DataDir otherwise (the one-shot `unarr download`). Every failure
// to get a persistent DB degrades to an in-memory completion map (pieces
// re-verify from disk each run) rather than to an unchecked file somewhere else.
//
// The caller keeps the returned impl so Shutdown can close it: torrent.Client.Close()
// closes torrents and peers but NOT DefaultStorage, so the piece-completion DB
// handle would leak for the process's life — on Windows that keeps the DB file
// locked and the directory undeletable.
func newTorrentStore(cfg TorrentConfig) storage.ClientImplCloser {
	dir := cfg.PieceCompletionDir
	if dir == "" {
		dir = cfg.DataDir
	}
	return storage.NewMMapWithCompletion(cfg.DataDir, openPieceCompletion(dir))
}

// openPieceCompletion returns the persistent piece-completion for dir, or an
// in-memory one when the persistent DB cannot be created.
func openPieceCompletion(dir string) storage.PieceCompletion {
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		log.Printf("[torrent] piece-completion dir %q create failed (%v) - using in-memory completion, pieces re-verify each run", dir, mkErr)
		return storage.NewMapPieceCompletion()
	}
	pc, pcErr := openBoltPieceCompletion(dir)
	if pcErr != nil {
		log.Printf("[torrent] piece-completion db in %q failed (%v) - using in-memory completion, pieces re-verify each run", dir, pcErr)
		return storage.NewMapPieceCompletion()
	}
	return pc
}
