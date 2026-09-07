package engine

import (
	"encoding/binary"
	"path/filepath"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"go.etcd.io/bbolt"
)

// Own bolt piece-completion backend, replacing storage.NewBoltPieceCompletion.
//
// Same file (PieceCompletionDBName) and byte-identical layout — "completion"
// bucket → one sub-bucket per infohash → 4-byte big-endian piece index → "c"/"i"
// — so a DB written by 1.11.x opens here with every record intact. What differs
// is how the file is opened, and that is the root cause of the field crash:
//
//   - NoSync stays FALSE. The library sets NoSync=true, which drops the
//     data-fsync-then-meta-fsync ordering bbolt's crash safety rests on; an
//     unclean shutdown could then persist a meta page that points at a freelist
//     and tree that never reached the disk in that shape.
//   - NoFreelistSync=true: the freelist is never persisted; bbolt rebuilds it
//     from reachability on every open, so a stale persisted freelist — the
//     "page N already freed" panic on the next MarkComplete — is unreachable by
//     construction. (bbolt still TRUSTS a freelist it finds in an older file:
//     that is why the pre-flight in torrent_storage.go stays, once, for files
//     written by 1.11.x. After our first commit the meta carries PgidNoFreelist.)
//
// Cost: two fdatasyncs per NEWLY completed piece (Set short-circuits an
// unchanged value, so re-verification writes nothing). The state dir is local
// disk by construction (never the NFS/SMB download dir), where that is ~1 ms.
// The backend no longer depends on CGO_ENABLED: the dev box tests exercise
// exactly what ships.

const (
	boltCompletionBucket = "completion"
	boltCompleteValue    = "c"
	boltIncompleteValue  = "i"
)

var boltCompletionBucketKey = []byte(boltCompletionBucket)

// boltLockTimeout bounds how long a bolt open waits for the file lock. A live
// daemon holding it (two instances racing at boot) must make the open fail
// fast — and the pre-flight step aside — rather than hang the boot.
const boltLockTimeout = time.Second

// boltCompletionOptions is the one way this package opens a bolt file for
// writing — the backend and the salvage rebuild share it so every file we
// produce is in the same (freelist-less, synced) mode.
func boltCompletionOptions() *bbolt.Options {
	return &bbolt.Options{
		Timeout:        boltLockTimeout,
		NoFreelistSync: true,
		FreelistType:   bbolt.FreelistMapType,
		// NoSync: false — deliberately, see the file comment.
	}
}

type boltPieceCompletion struct {
	db *bbolt.DB
}

var _ storage.PieceCompletion = (*boltPieceCompletion)(nil)

func openBoltPieceCompletion(dir string) (*boltPieceCompletion, error) {
	db, err := bbolt.Open(filepath.Join(dir, PieceCompletionDBName), 0o660, boltCompletionOptions())
	if err != nil {
		return nil, err
	}
	return &boltPieceCompletion{db: db}, nil
}

// Persistent tells the library the records survive restarts, so it need not
// flush pieces on MarkComplete for correctness (same as the library backend).
func (c *boltPieceCompletion) Persistent() bool { return true }

func pieceIndexKey(index int) [4]byte {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], uint32(index))
	return key
}

func (c *boltPieceCompletion) Get(pk metainfo.PieceKey) (cn storage.Completion, err error) {
	err = c.db.View(func(tx *bbolt.Tx) error {
		root := tx.Bucket(boltCompletionBucketKey)
		if root == nil {
			return nil
		}
		ih := root.Bucket(pk.InfoHash[:])
		if ih == nil {
			return nil
		}
		key := pieceIndexKey(pk.Index)
		switch string(ih.Get(key[:])) {
		case boltCompleteValue:
			cn.Ok, cn.Complete = true, true
		case boltIncompleteValue:
			cn.Ok = true
		}
		return nil
	})
	return cn, err
}

func (c *boltPieceCompletion) Set(pk metainfo.PieceKey, complete bool) error {
	if cn, err := c.Get(pk); err == nil && cn.Ok && cn.Complete == complete {
		return nil
	}
	value := []byte(boltIncompleteValue)
	if complete {
		value = []byte(boltCompleteValue)
	}
	return c.db.Update(func(tx *bbolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists(boltCompletionBucketKey)
		if err != nil {
			return err
		}
		ih, err := root.CreateBucketIfNotExists(pk.InfoHash[:])
		if err != nil {
			return err
		}
		key := pieceIndexKey(pk.Index)
		return ih.Put(key[:], value)
	})
}

func (c *boltPieceCompletion) Close() error { return c.db.Close() }
