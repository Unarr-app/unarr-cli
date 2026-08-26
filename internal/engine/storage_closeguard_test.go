package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// testInfo builds a minimal single-file info so a storage backend can open a
// real torrent (one piece is enough to get a real mmap).
func testInfo(t *testing.T, dir string, size int64) *metainfo.Info {
	t.Helper()
	const pieceLen = 1 << 14
	info := &metainfo.Info{
		Name:        "guard-test.bin",
		Length:      size,
		PieceLength: pieceLen,
	}
	n := int(size / pieceLen)
	if size%pieceLen != 0 {
		n++
	}
	info.Pieces = make([]byte, n*20)
	_ = dir
	return info
}

// TestCloseGuardRefusesWriteAfterClose is the regression test for the field
// crash: anacrolix/torrent v1.61.0's MMapSpan.WriteAt does not check its closed
// flag, so a chunk write racing t.Drop() indexes a nil slice and takes the whole
// daemon down with "index out of range [1] with length 0". With the guard the
// write must come back as an error the client can handle.
func TestCloseGuardRefusesWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	// Two pieces: the panic needs a multi-segment locate (index [1]).
	info := testInfo(t, dir, 1<<15)

	guarded := newCloseGuard(storage.NewMMap(dir))
	t.Cleanup(func() { _ = guarded.Close() })

	impl, err := guarded.OpenTorrent(context.Background(), info, metainfo.Hash{1})
	if err != nil {
		t.Fatalf("open torrent: %v", err)
	}

	piece := impl.Piece(info.Piece(0))
	if _, err := piece.WriteAt(make([]byte, 1<<14), 0); err != nil {
		t.Fatalf("write before close should succeed: %v", err)
	}

	if err := impl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Upstream panics here. The guard must return an error instead.
	n, err := piece.WriteAt(make([]byte, 1<<14), 0)
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("write after close: got (%d, %v), want fs.ErrClosed", n, err)
	}
	if n != 0 {
		t.Fatalf("write after close reported %d bytes written, want 0", n)
	}

	if _, err := piece.ReadAt(make([]byte, 16), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after close: want fs.ErrClosed, got %v", err)
	}
}

// TestCloseGuardWriteRacingClose runs the actual shape of the crash: peer
// goroutines writing chunks while the torrent is dropped underneath them. Run
// with -race. Without the guard this panics rather than failing.
func TestCloseGuardWriteRacingClose(t *testing.T) {
	dir := t.TempDir()
	info := testInfo(t, dir, 1<<16)

	guarded := newCloseGuard(storage.NewMMap(dir))
	t.Cleanup(func() { _ = guarded.Close() })

	impl, err := guarded.OpenTorrent(context.Background(), info, metainfo.Hash{2})
	if err != nil {
		t.Fatalf("open torrent: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			piece := impl.Piece(info.Piece(idx % info.NumPieces()))
			buf := make([]byte, 1<<14)
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Only fs.ErrClosed is acceptable once Close has run; any other
				// error (or a panic) is the bug.
				if _, err := piece.WriteAt(buf, 0); err != nil && !errors.Is(err, fs.ErrClosed) {
					t.Errorf("unexpected write error: %v", err)
					return
				}
			}
		}(i)
	}

	if err := impl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(stop)
	wg.Wait()

	if _, err := os.Stat(filepath.Join(dir, info.Name)); err != nil {
		t.Fatalf("stat data file: %v", err)
	}
}
