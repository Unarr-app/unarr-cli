package engine

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// pgidNoFreelist mirrors bbolt's internal/common.PgidNoFreelist: the meta
// value that says "no persisted freelist, rebuild from reachability on open".
const pgidNoFreelist = 0xffffffffffffffff

const fixtureRecords = 4000

func fixtureInfoHash() metainfo.Hash {
	var ih metainfo.Hash
	copy(ih[:], "0123456789abcdef0123")
	return ih
}

// writeLegacyPieceCompletionDB creates the DB through the LIBRARY's bolt backend
// — exactly what every 1.11.x install has on disk (NoSync, persisted freelist) —
// with enough rows to span several pages, so the tree is more than a single leaf.
func writeLegacyPieceCompletionDB(t *testing.T, dir string) string {
	t.Helper()
	pc, err := storage.NewBoltPieceCompletion(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ih := fixtureInfoHash()
	for i := 0; i < fixtureRecords; i++ {
		if err := pc.Set(metainfo.PieceKey{InfoHash: ih, Index: i}, true); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return filepath.Join(dir, PieceCompletionDBName)
}

// winningMeta returns the meta bbolt will use: the one with the higher txid.
//
// Layouts (bbolt internal/common, little-endian on every target we ship):
//
//	page header: id u64 @0 · flags u16 @8 · count u16 @10 · overflow u32 @12 · data @16
//	meta (@16 in pages 0 and 1): magic u32 · version u32 · pageSize u32 · flags u32 ·
//	  root.pgid u64 @16 · root.seq u64 @24 · freelist pgid u64 @32 · pgid u64 @40 · txid u64 @48
func winningMeta(raw []byte, pageSize int) []byte {
	le := binary.LittleEndian
	m0, m1 := raw[16:], raw[pageSize+16:]
	if le.Uint64(m1[48:]) > le.Uint64(m0[48:]) {
		return m1
	}
	return m0
}

// persistedFreelistPgid reads meta.freelist of the winning meta.
func persistedFreelistPgid(t *testing.T, path string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int(binary.LittleEndian.Uint32(raw[16+8:]))
	return binary.LittleEndian.Uint64(winningMeta(raw, pageSize)[32:])
}

// assertFixtureRecords opens dir with OUR backend and checks every record the
// legacy fixture wrote is still there, then writes one more and closes.
func assertFixtureRecords(t *testing.T, dir string) {
	t.Helper()
	pc, err := openBoltPieceCompletion(dir)
	if err != nil {
		t.Fatalf("open with own backend: %v", err)
	}
	defer pc.Close()
	ih := fixtureInfoHash()
	for i := 0; i < fixtureRecords; i++ {
		cn, err := pc.Get(metainfo.PieceKey{InfoHash: ih, Index: i})
		if err != nil || !cn.Ok || !cn.Complete {
			t.Fatalf("record %d lost: ok=%v complete=%v err=%v", i, cn.Ok, cn.Complete, err)
		}
	}
	if err := pc.Set(metainfo.PieceKey{InfoHash: ih, Index: fixtureRecords}, true); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
}

// The root-cause fix: a file our backend has written carries NO persisted
// freelist (meta.freelist == PgidNoFreelist), so bbolt rebuilds it from
// reachability on every open and the "page N already freed" class cannot arise.
func TestBoltPieceCompletion_NeverPersistsAFreelist(t *testing.T) {
	dir := t.TempDir()
	pc, err := openBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	ih := fixtureInfoHash()
	for i := 0; i < 100; i++ {
		if err := pc.Set(metainfo.PieceKey{InfoHash: ih, Index: i}, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	if got := persistedFreelistPgid(t, filepath.Join(dir, PieceCompletionDBName)); got != pgidNoFreelist {
		t.Fatalf("meta.freelist = %#x, want PgidNoFreelist", got)
	}
	// And the file we wrote is what a fresh open reads back.
	pc, err = openBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for i := 0; i < 100; i++ {
		cn, err := pc.Get(metainfo.PieceKey{InfoHash: ih, Index: i})
		if err != nil || !cn.Ok || cn.Complete != (i%2 == 0) {
			t.Fatalf("piece %d: ok=%v complete=%v err=%v", i, cn.Ok, cn.Complete, err)
		}
	}
	if cn, _ := pc.Get(metainfo.PieceKey{InfoHash: ih, Index: 100}); cn.Ok {
		t.Fatal("unknown piece must report !Ok")
	}
}

// Upgrade path: a healthy 1.11.x file (library layout, persisted freelist) opens
// with our backend with every record, and its FIRST commit through us drops the
// persisted freelist for good.
func TestBoltPieceCompletion_ReadsLegacyFileAndDropsItsFreelist(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	if got := persistedFreelistPgid(t, path); got == pgidNoFreelist {
		t.Fatal("legacy fixture should carry a persisted freelist")
	}
	assertFixtureRecords(t, dir) // reads all + one Set + close
	if got := persistedFreelistPgid(t, path); got != pgidNoFreelist {
		t.Fatalf("after our first commit meta.freelist = %#x, want PgidNoFreelist", got)
	}
}

// And the other direction, so a downgrade to 1.11.x does not lose the cache.
func TestBoltPieceCompletion_LibraryBackendReadsOurFile(t *testing.T) {
	dir := t.TempDir()
	pc, err := openBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	ih := fixtureInfoHash()
	if err := pc.Set(metainfo.PieceKey{InfoHash: ih, Index: 7}, true); err != nil {
		t.Fatal(err)
	}
	pc.Close()

	lib, err := storage.NewBoltPieceCompletion(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	cn, err := lib.Get(metainfo.PieceKey{InfoHash: ih, Index: 7})
	if err != nil || !cn.Ok || !cn.Complete {
		t.Fatalf("library backend: ok=%v complete=%v err=%v", cn.Ok, cn.Complete, err)
	}
}
