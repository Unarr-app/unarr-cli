package engine

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/fsx"
	"github.com/anacrolix/torrent/storage"
)

// PieceCompletionDBName is the bolt piece-completion DB inside the agent state
// dir. The name is the one anacrolix/torrent's own bolt backend used through
// 1.11.x, kept so those files carry over — see piece_completion_bolt.go for
// why the backend is ours.
const PieceCompletionDBName = ".torrent.bolt.db"

// PieceCompletionQuarantineName is where a corrupt DB is moved. A FIXED name on
// purpose: bolt files never shrink, and the users who hit this (Fast Startup,
// flaky power) hit it repeatedly — a timestamped name would pile up a full-size
// file per incident. One file keeps the latest for forensics, and `unarr clean`
// knows it by this name.
const PieceCompletionQuarantineName = PieceCompletionDBName + ".corrupt"

// PieceCompletionRebuiltSuffix names (with a ".<pid>" after it) the consistent
// copy the checker child writes next to a DB whose only damage is a lying
// freelist; the parent swaps it into place. A leftover means a crash mid-swap;
// `unarr clean` removes them by glob.
const PieceCompletionRebuiltSuffix = ".rebuilt"

// PieceCompletionLegacySQLiteName is the SQLite piece-completion DB the
// library's default backend wrote on cgo builds (a `go install` from source;
// every shipped release is CGO_ENABLED=0 and never had one). It is not read:
// migrating would drag the sqlite driver back in for a cache that rebuilds
// itself in one verify pass. Its presence is logged once and `unarr clean`
// reaps it with its -wal/-shm companions.
const PieceCompletionLegacySQLiteName = ".torrent.db"

// pieceCompletionRenameWindow / Step bound the wait for a Windows rename that
// an antivirus or the search indexer momentarily blocks — the errCorruptNotMoved
// case, which without the retry repeats the child check, the failed rename and a
// full re-hash on EVERY boot for as long as the indexer keeps winning the race.
const (
	pieceCompletionRenameWindow = 2 * time.Second
	pieceCompletionRenameStep   = 10 * time.Millisecond
)

// errCorruptNotMoved: the DB is proven corrupt but could not be moved aside
// even after the retry window (a read-only state dir). The caller must NOT hand
// that file to the backend — that is the crash loop.
var errCorruptNotMoved = errors.New("piece-completion db is corrupt and could not be moved aside")

// newTorrentStore picks the anacrolix storage backend for the downloader.
//
// mmap instead of the default file backend: the library author notes file
// storage has "very high system overhead"; mmap improves I/O throughput and
// piece verification speed significantly.
//
// The piece-completion DB lives in PieceCompletionDir when set — both the
// daemon and the one-shot `unarr download` pass the agent state dir, keeping
// it off NFS/SMB where file locking times out and every fsync crosses the
// network — and in DataDir only for callers that set nothing (tests). Either
// way it is integrity-checked and, if damaged, rebuilt or moved aside BEFORE
// the backend opens it — see repairPieceCompletionDB. Every failure to get a
// persistent, trustworthy DB degrades to an in-memory completion map (pieces
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
// in-memory one when the persistent DB cannot be trusted or created.
func openPieceCompletion(dir string) storage.PieceCompletion {
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		log.Printf("[torrent] piece-completion dir %q create failed (%v) - using in-memory completion, pieces re-verify each run", dir, mkErr)
		return storage.NewMapPieceCompletion()
	}
	if _, err := os.Stat(filepath.Join(dir, PieceCompletionLegacySQLiteName)); err == nil {
		log.Printf("[torrent] legacy sqlite piece-completion cache found (%s, from a cgo build) - not migrated, resumed torrents re-verify once; `unarr clean` removes it", PieceCompletionLegacySQLiteName)
	}
	repair, repErr := repairPieceCompletionDB(dir)
	switch {
	case errors.Is(repErr, errCorruptNotMoved):
		log.Printf("[torrent] %v - using in-memory completion this run, pieces re-verify from disk", repErr)
		return storage.NewMapPieceCompletion()
	case repErr != nil:
		log.Printf("[torrent] piece-completion db check skipped (%v) - opening it as-is", repErr)
	case repair.Salvaged:
		log.Printf("[torrent] piece-completion db had a damaged freelist (unclean shutdown?) - rebuilt in place, completion records kept; damaged copy at %s (%s)", repair.Quarantined, repair.Damage)
	case repair.Quarantined != "":
		log.Printf("[torrent] piece-completion db was corrupt (unclean shutdown?) - moved to %s; resumed torrents re-verify their pieces from disk (%s)", repair.Quarantined, repair.Damage)
	}
	pc, pcErr := openBoltPieceCompletion(dir)
	if pcErr != nil {
		log.Printf("[torrent] piece-completion db in %q failed (%v) - using in-memory completion, pieces re-verify each run", dir, pcErr)
		return storage.NewMapPieceCompletion()
	}
	return pc
}

// pieceCompletionRepair is what repairPieceCompletionDB did to the DB.
type pieceCompletionRepair struct {
	Quarantined string // path of the damaged original, "" when nothing was wrong
	Salvaged    bool   // a rebuilt copy with every record now sits under the DB name
	Damage      string // the checker's reason, for the log
}

// repairPieceCompletionDB integrity-checks the bolt piece-completion DB in dir
// before the backend opens it and, when it is damaged, either swaps in a
// rebuilt copy (freelist-only damage: the records are intact) or moves it aside
// so the backend starts fresh.
//
// Why this exists: through 1.11.x the DB was opened by the library with NoSync,
// so an unclean shutdown (power loss, Windows Fast Startup hibernating a live
// daemon, a hard kill) could leave the persisted freelist disagreeing with the
// B+tree. bbolt does not detect that on Open — it surfaces LATER as `panic: page
// N already freed` inside the first Update that touches the damaged page, i.e.
// the moment a resumed torrent hashes its next piece and calls MarkComplete. The
// panic fires on a library goroutine (pieceHasher), so nothing in this process
// can recover it, and the same piece re-hashes on every boot: the daemon
// crash-loops forever (crash report 2026-09-07, agent 1.11.6 / windows). Bolt's
// Check names exactly that inconsistency, which is why it runs here, before the
// backend gets the file — in a child process, see checkPieceCompletionDB.
//
// It is a one-shot migration in practice: the check only runs while the file
// still carries a persisted freelist (pieceCompletionNeedsCheck), i.e. until
// our backend's first commit; after that bbolt rebuilds the freelist from
// reachability at every open and the damage class cannot exist.
//
// Two daemons over one state dir (a dev agent next to the prod one, the
// supported UNARR_CONFIG_DIR setup) can both reach this. The rebuilt copies
// carry the pid, so they never touch each other's; the SameFile guard makes
// the loser of the quarantine rename leave the winner's fresh DB alone in all
// but a microsecond window (an exclusive lock across the rename is not
// available on Windows, where a locked file cannot be renamed).
//
// Errors: errCorruptNotMoved when the damage is proven but the rename failed;
// any other error means the check could NOT run (file locked by a live daemon,
// unreadable, checker unavailable) and the file was left alone.
func repairPieceCompletionDB(dir string) (pieceCompletionRepair, error) {
	path := filepath.Join(dir, PieceCompletionDBName)
	before, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return pieceCompletionRepair{}, nil
		}
		return pieceCompletionRepair{}, fmt.Errorf("stat piece-completion db: %w", statErr)
	}
	if needs, _ := pieceCompletionNeedsCheck(path); !needs {
		return pieceCompletionRepair{}, nil
	}

	v := checkPieceCompletionDB(path)
	switch v.kind {
	case boltHealthy:
		return pieceCompletionRepair{}, nil
	case boltSkipped:
		return pieceCompletionRepair{}, errors.New(v.reason)
	case boltCorrupt, boltSalvaged:
		// fall through to the swap below
	}

	after, statErr := os.Stat(path)
	if statErr != nil || !os.SameFile(before, after) {
		discardRebuilt(v)
		return pieceCompletionRepair{}, errors.New("piece-completion db was replaced while being checked (another daemon?) - left alone")
	}
	quarantine := filepath.Join(dir, PieceCompletionQuarantineName)
	if renameErr := fsx.RenameWithRetry(path, quarantine, pieceCompletionRenameWindow, pieceCompletionRenameStep); renameErr != nil {
		discardRebuilt(v)
		return pieceCompletionRepair{}, fmt.Errorf("%w: %v (damage: %s)", errCorruptNotMoved, renameErr, v.reason)
	}
	repair := pieceCompletionRepair{Quarantined: quarantine, Damage: v.reason}
	if v.kind != boltSalvaged {
		return repair, nil
	}
	// The damaged original is safely aside. A crash between the two renames
	// leaves no DB and an orphaned rebuilt copy: the next boot starts a fresh DB
	// (a full re-verify, never a crash) and `unarr clean` reaps the orphan.
	if renameErr := fsx.RenameWithRetry(v.rebuilt, path, pieceCompletionRenameWindow, pieceCompletionRenameStep); renameErr != nil {
		discardRebuilt(v)
		repair.Damage += fmt.Sprintf("; rebuilt copy could not be moved into place (%v), starting fresh", renameErr)
		return repair, nil
	}
	repair.Salvaged = true
	return repair, nil
}

func discardRebuilt(v boltVerdict) {
	if v.rebuilt != "" {
		_ = os.Remove(v.rebuilt)
	}
}
