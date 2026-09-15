package engine

import (
	"context"
	"io/fs"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// closeGuard makes a storage backend answer "closed" instead of PANICKING when a
// write lands after the torrent's storage was closed.
//
// This is a workaround for a live crash in anacrolix/torrent v1.61.0, and the
// crash is not hypothetical: a field report (linux/amd64, agent v1.10.4) killed
// the whole daemon with
//
//	panic: runtime error: index out of range [1] with length 0
//	  mmap-span.(*MMapSpan).locateCopy      mmap-span/mmap-span.go:80
//	  mmap-span.(*MMapSpan).WriteAt         mmap-span/mmap-span.go:94
//	  torrent.(*Torrent).writeChunk         torrent.go:1185
//	  torrent.(*Peer).receiveChunk          peer.go:421
//
// one line after we returned an integrity error and dropped the torrent.
//
// The upstream bug is an asymmetry in mmap-span: Close() sets `mMaps = nil` and
// `closed = true`; ReadAt checks `closed` and returns fs.ErrClosed, but WriteAt
// does NOT check it — it goes straight to locateCopy, which indexes `ms.mMaps[i]`
// on the now-empty slice and panics. The lock does not save us either: both take
// only an RLock, so a chunk write already in flight runs concurrently with the
// Close.
//
// The race is reachable on EVERY torrent error path. t.Drop() closes the storage
// while the peer goroutines are still alive, and a peer that delivers one more
// chunk in that window writes into closed storage. Our integrity retry
// ("torrent reported complete but N of verified pieces are still missing" ->
// cleanup -> re-download) is simply the path that hits it most, because it drops
// a torrent that has many active peers mid-flight.
//
// Returning an error here is CORRECT, not merely safe: writeChunk propagates it
// and receiveChunk re-pends the request (peer.go:425), which is exactly what
// should happen to a chunk whose destination went away.
//
// Fix here rather than in a fork of the dependency: it is a handful of lines at
// the one place we already inject storage, and it keeps working if a future
// upstream release fixes mmap-span (the guard just stops ever firing).
type closeGuard struct {
	storage.ClientImplCloser
}

// newCloseGuard wraps a storage backend so no write can reach it after close.
func newCloseGuard(inner storage.ClientImplCloser) storage.ClientImplCloser {
	return closeGuard{ClientImplCloser: inner}
}

func (g closeGuard) OpenTorrent(
	ctx context.Context,
	info *metainfo.Info,
	infoHash metainfo.Hash,
) (storage.TorrentImpl, error) {
	impl, err := g.ClientImplCloser.OpenTorrent(ctx, info, infoHash)
	if err != nil {
		return impl, err
	}
	return guardTorrentImpl(impl), nil
}

// guardTorrentImpl routes every piece of a torrent through one shared gate,
// which the torrent's own Close shuts before the inner storage unmaps.
func guardTorrentImpl(impl storage.TorrentImpl) storage.TorrentImpl {
	gate := &storageGate{}

	if inner := impl.Piece; inner != nil {
		impl.Piece = func(p metainfo.Piece) storage.PieceImpl {
			return guardedPiece{PieceImpl: inner(p), gate: gate}
		}
	}
	if inner := impl.PieceWithHash; inner != nil {
		impl.PieceWithHash = func(p metainfo.Piece, hash g.Option[[]byte]) storage.PieceImpl {
			return guardedPiece{PieceImpl: inner(p, hash), gate: gate}
		}
	}

	inner := impl.Close
	impl.Close = func() error {
		gate.shut()
		if inner == nil {
			return nil
		}
		return inner()
	}
	return impl
}

// storageGate lets reads and writes into a torrent's storage run concurrently
// and keeps them all out of its Close. A plain closed flag is not enough: a write
// that checked the flag just before Close would still reach the inner storage
// while it unmaps — the inner storage's own RLock does not stop that — and panic
// exactly as the unguarded write did. Holding the gate's read lock for the whole
// inner call closes that window: shut waits for every call in flight.
type storageGate struct {
	mu     sync.RWMutex
	closed bool
}

// enter admits a read or write, or reports the storage closed. An admitted call
// must leave.
func (g *storageGate) enter() bool {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return false
	}
	return true
}

func (g *storageGate) leave() { g.mu.RUnlock() }

// shut refuses every later read and write once the ones in flight have returned.
func (g *storageGate) shut() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// guardedPiece refuses reads and writes once its torrent's storage is closed.
type guardedPiece struct {
	storage.PieceImpl
	gate *storageGate
}

func (p guardedPiece) WriteAt(b []byte, off int64) (int, error) {
	if !p.gate.enter() {
		return 0, fs.ErrClosed
	}
	defer p.gate.leave()
	return p.PieceImpl.WriteAt(b, off)
}

func (p guardedPiece) ReadAt(b []byte, off int64) (int, error) {
	if !p.gate.enter() {
		return 0, fs.ErrClosed
	}
	defer p.gate.leave()
	return p.PieceImpl.ReadAt(b, off)
}
