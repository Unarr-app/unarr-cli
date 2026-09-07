package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"go.etcd.io/bbolt"
)

// boltCheckTestCrashEnv makes the checker child panic instead of checking, to
// prove the parent survives a checker the file takes down.
const boltCheckTestCrashEnv = "UNARR_BOLT_CHECK_TEST_CRASH"

// boltCheckTestExitEnv makes the checker child exit with that status and no
// output — an external kill (antivirus, OOM killer) or a binary that never
// ran a checker at all.
const boltCheckTestExitEnv = "UNARR_BOLT_CHECK_TEST_EXIT"

// pgidNoFreelist mirrors bbolt's internal/common.PgidNoFreelist: the meta
// value that says "no persisted freelist, rebuild from reachability on open".
const pgidNoFreelist = 0xffffffffffffffff

const fixtureRecords = 4000

// TestMain doubles as the checker child, the way cmd/unarr/main.go does: the
// quarantine re-execs os.Executable(), which under `go test` is this binary.
func TestMain(m *testing.M) {
	if path := os.Getenv(BoltCheckChildEnv); path != "" {
		if os.Getenv(boltCheckTestCrashEnv) != "" {
			panic("simulated torn page: checker died")
		}
		if code := os.Getenv(boltCheckTestExitEnv); code != "" {
			n, _ := strconv.Atoi(code)
			os.Exit(n)
		}
		os.Exit(BoltCheckMain(path))
	}
	os.Exit(m.Run())
}

func mustNoRebuiltLeftover(t *testing.T, dir string) {
	t.Helper()
	leftovers, _ := filepath.Glob(filepath.Join(dir, PieceCompletionDBName+PieceCompletionRebuiltSuffix+".*"))
	if len(leftovers) != 0 {
		t.Fatalf("rebuilt copies left behind: %v", leftovers)
	}
}

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

// injectReachableFreed reproduces the damage class behind the field crash
// (`panic: page N already freed` on the first MarkComplete after an unclean
// shutdown): a page that is still part of the B+tree also listed in the
// persisted freelist. bbolt.Open does NOT notice — the meta pages are intact —
// so only the consistency check can. It appends the root bucket page id to the
// freelist page of the winning meta.
//
// Layouts (bbolt internal/common, little-endian on every target we ship):
//
//	page header: id u64 @0 · flags u16 @8 · count u16 @10 · overflow u32 @12 · data @16
//	meta (@16 in pages 0 and 1): magic u32 · version u32 · pageSize u32 · flags u32 ·
//	  root.pgid u64 @16 · root.seq u64 @24 · freelist pgid u64 @32 · pgid u64 @40 · txid u64 @48
//	freelist page data: ids u64[count]
func injectReachableFreed(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	le := binary.LittleEndian
	pageSize := int(le.Uint32(raw[16+8:]))
	m := winningMeta(raw, pageSize)
	rootPg := le.Uint64(m[16:])
	freelistPg := int(le.Uint64(m[32:]))
	if uint64(freelistPg) == pgidNoFreelist {
		t.Fatal("fixture has no persisted freelist; the legacy backend must have written it")
	}

	fl := raw[freelistPg*pageSize:]
	count := int(le.Uint16(fl[10:]))
	if count >= 0xFFFF {
		t.Fatal("freelist uses the overflow-count layout; test assumes the compact one")
	}
	le.PutUint64(fl[16+8*count:], rootPg)
	le.PutUint16(fl[10:], uint16(count+1))

	if err := os.WriteFile(path, raw, 0o660); err != nil {
		t.Fatalf("write db: %v", err)
	}
}

// winningMeta returns the meta bbolt will use: the one with the higher txid.
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

// writeGarbageDB: a file both meta pages of which are junk. Large enough that
// bbolt finds BOTH metas inside the file whatever the OS page size (it falls
// back to the OS page size when meta0 is unreadable — 16 KiB on Apple Silicon,
// up to 64 KiB elsewhere): a shorter file makes the meta1 read fail with an I/O
// error, which is an environment failure, not damage.
func writeGarbageDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, PieceCompletionDBName)
	garbage := make([]byte, 128<<10)
	for i := range garbage {
		garbage[i] = 0xAB
	}
	if err := os.WriteFile(path, garbage, 0o660); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should be gone, stat err=%v", path, err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// assertFixtureRecords opens dir with OUR backend and checks every record the
// legacy fixture wrote is still there, then writes one more (the MarkComplete
// that used to panic) and closes.
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
		t.Fatalf("MarkComplete after repair: %v", err)
	}
}

// --- own backend ---------------------------------------------------------------

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

// --- pre-flight: repair / quarantine ---------------------------------------------

func TestRepairPieceCompletionDB_AbsentIsNoop(t *testing.T) {
	dir := t.TempDir()
	r, err := repairPieceCompletionDB(dir)
	if err != nil || r.Quarantined != "" || r.Salvaged {
		t.Fatalf("absent db: %+v err=%v, want nothing", r, err)
	}
}

func TestRepairPieceCompletionDB_HealthyIsKept(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)

	r, err := repairPieceCompletionDB(dir)
	if err != nil || r.Quarantined != "" || r.Salvaged {
		t.Fatalf("healthy db: %+v err=%v, want nothing", r, err)
	}
	mustExist(t, path)
	mustNotExist(t, filepath.Join(dir, PieceCompletionQuarantineName))
}

func TestRepairPieceCompletionDB_UnopenableIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := writeGarbageDB(t, dir)

	r, err := repairPieceCompletionDB(dir)
	if err != nil {
		t.Fatalf("garbage db: %v", err)
	}
	if r.Quarantined != filepath.Join(dir, PieceCompletionQuarantineName) || r.Salvaged {
		t.Fatalf("garbage db: %+v", r)
	}
	mustNotExist(t, path)
	mustExist(t, r.Quarantined)
	// The backend must now be able to start from scratch in that dir.
	pc, err := openBoltPieceCompletion(dir)
	if err != nil {
		t.Fatalf("fresh db after quarantine: %v", err)
	}
	pc.Close()
}

// The field case: metas fine, tree fine, freelist lies. Open succeeds, Check
// does not — and because the records are intact, the file is REBUILT, not
// discarded: every record survives and the MarkComplete that used to panic
// succeeds. The damaged original is kept aside.
func TestRepairPieceCompletionDB_ReachableFreedPageIsSalvaged(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	injectReachableFreed(t, path)

	// Sanity: this damage is invisible to a plain open, which is the whole point.
	db, err := bbolt.Open(path, 0o660, &bbolt.Options{Timeout: time.Second, ReadOnly: true})
	if err != nil {
		t.Fatalf("damaged db must still open (bbolt only validates metas): %v", err)
	}
	db.Close()

	r, err := repairPieceCompletionDB(dir)
	if err != nil {
		t.Fatalf("damaged db: %v", err)
	}
	if !r.Salvaged || r.Quarantined == "" {
		t.Fatalf("freelist-only damage must be salvaged: %+v", r)
	}
	mustExist(t, r.Quarantined)
	mustExist(t, path)
	mustNoRebuiltLeftover(t, dir)
	if v, _ := inspectBoltFile(path); v.kind != boltHealthy {
		t.Fatalf("rebuilt db is not healthy: %s", v.reason)
	}
	if got := persistedFreelistPgid(t, path); got != pgidNoFreelist {
		t.Fatalf("rebuilt db still persists a freelist (%#x)", got)
	}
	assertFixtureRecords(t, dir)
	t.Logf("damage: %s", r.Damage)
}

// A second incident overwrites the previous quarantine file instead of adding
// one: the fixed name is what keeps the state dir from filling up.
func TestRepairPieceCompletionDB_SecondIncidentReplacesTheFirst(t *testing.T) {
	dir := t.TempDir()
	writeGarbageDB(t, dir)
	if _, err := repairPieceCompletionDB(dir); err != nil {
		t.Fatal(err)
	}
	path := writeLegacyPieceCompletionDB(t, dir)
	injectReachableFreed(t, path)
	r, err := repairPieceCompletionDB(dir)
	if err != nil || !r.Salvaged {
		t.Fatalf("second incident: %+v err=%v", r, err)
	}
	got := dirNames(t, dir)
	if len(got) != 2 {
		t.Fatalf("want the live db + one quarantine file, got %v", got)
	}
}

// Proves the injected damage IS the field crash and not a bogus fixture: the
// LIBRARY's MarkComplete path (Set on a new piece, as 1.11.x does) dies inside
// bbolt with a panic, not an error. Which panic depends on where the
// doubly-owned page lands in the next write: `page N already freed`
// (freelist.Free while spilling the old root, the report's message) or
// `misplaced bucket header` (the "free" root page got reused and overwritten).
// Both are the process dying on pieceHasher.
func TestInjectedDamageReproducesFieldPanic(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	injectReachableFreed(t, path)

	pc, err := storage.NewBoltPieceCompletion(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pc.Close()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = pc.Set(metainfo.PieceKey{InfoHash: fixtureInfoHash(), Index: fixtureRecords}, true)
	}()
	msg, _ := recovered.(string)
	if !strings.Contains(msg, "already freed") && !strings.Contains(msg, "misplaced bucket header") {
		t.Fatalf("want a bbolt integrity panic on MarkComplete, got %v", recovered)
	}
	t.Logf("MarkComplete on the damaged db panics with: %s", msg)
}

// The reason the check runs out of process: a file that takes the checker down
// (torn page ⇒ runtime fault or assert panic on bbolt's own goroutine) must read
// as "corrupt" in the daemon, not kill it. The child is told to panic.
func TestCheckPieceCompletionDB_CheckerCrashCountsAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	t.Setenv(boltCheckTestCrashEnv, "1")

	v := checkPieceCompletionDB(path)
	if v.kind != boltCorrupt {
		t.Fatalf("crashing checker: kind=%v reason=%q, want corrupt", v.kind, v.reason)
	}
	if !strings.Contains(v.reason, "checker died") || !strings.Contains(v.reason, "simulated torn page") {
		t.Fatalf("reason should carry the child's first stderr lines, got %q", v.reason)
	}

	r, err := repairPieceCompletionDB(dir)
	if err != nil || r.Quarantined == "" || r.Salvaged {
		t.Fatalf("crashing checker must quarantine, never salvage: %+v err=%v", r, err)
	}
}

// The salvage decision reads the WHOLE Check stream: a tree finding after more
// freelist findings than the text cap keeps must still condemn the file (a
// legacy DB after a NoSync power loss routinely carries dozens of stale
// freelist ids, and Compact would copy a damaged tree into a clean-looking
// file). Only the text is capped.
func TestClassifyCheckFindings_TreeDamageAfterTheTextCapStillCounts(t *testing.T) {
	feed := func(msgs ...string) <-chan error {
		ch := make(chan error)
		go func() {
			defer close(ch)
			for _, m := range msgs {
				ch <- errors.New(m)
			}
		}()
		return ch
	}
	var many []string
	for i := 0; i < boltCheckMaxFindings+8; i++ {
		many = append(many, fmt.Sprintf("page %d: already freed", 100+i))
	}

	findings, tree := classifyCheckFindings(feed(many...))
	if tree || len(findings) != boltCheckMaxFindings {
		t.Fatalf("freelist-only stream: tree=%v kept=%d, want false/%d", tree, len(findings), boltCheckMaxFindings)
	}
	findings, tree = classifyCheckFindings(feed(append(many, "page 9: multiple references (stack: [3 9])")...))
	if !tree || len(findings) != boltCheckMaxFindings {
		t.Fatalf("tree finding past the cap: tree=%v kept=%d, want true/%d", tree, len(findings), boltCheckMaxFindings)
	}
	if _, tree = classifyCheckFindings(feed("page 3: reachable freed", "page 7: unreachable unfreed")); tree {
		t.Fatal("reachable freed / unreachable unfreed are freelist findings")
	}
	if _, tree = classifyCheckFindings(feed("page 4: invalid type: unknown<00>")); !tree {
		t.Fatal("invalid type is tree damage")
	}
	if findings, tree = classifyCheckFindings(feed()); tree || len(findings) != 0 {
		t.Fatal("empty stream is healthy")
	}
}

// The check is a one-shot migration: a file our backend has committed carries
// no persisted freelist and is NOT handed to the child — proven by arming the
// child to crash and seeing nothing happen. A legacy file and a garbage file
// are.
func TestPieceCompletionNeedsCheck_OnlyLegacyOrUnparseableFiles(t *testing.T) {
	legacy := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, legacy)
	if needs, why := pieceCompletionNeedsCheck(path); !needs {
		t.Fatalf("legacy file must be checked (%s)", why)
	}

	ours := t.TempDir()
	pc, err := openBoltPieceCompletion(ours)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // >1 commit so BOTH metas carry the sentinel
		if err := pc.Set(metainfo.PieceKey{InfoHash: fixtureInfoHash(), Index: i}, true); err != nil {
			t.Fatal(err)
		}
	}
	pc.Close()
	ourPath := filepath.Join(ours, PieceCompletionDBName)
	if needs, why := pieceCompletionNeedsCheck(ourPath); needs {
		t.Fatalf("our own file must not be re-checked every boot (%s)", why)
	}
	t.Setenv(boltCheckTestCrashEnv, "1")
	if r, err := repairPieceCompletionDB(ours); err != nil || r.Quarantined != "" {
		t.Fatalf("no child must run for our own file: %+v err=%v", r, err)
	}
	mustExist(t, ourPath)

	garbage := t.TempDir()
	if needs, _ := pieceCompletionNeedsCheck(writeGarbageDB(t, garbage)); !needs {
		t.Fatal("garbage must be checked")
	}
	if needs, _ := boltHeaderNeedsCheck(nil); !needs {
		t.Fatal("empty header must be checked")
	}
}

// Only a Go panic (exit 2) means the file broke the checker. An external kill
// or a binary that never ran a checker says nothing about the file and must
// leave it alone — quarantining a healthy DB over an antivirus kill forces a
// full re-hash and blames a shutdown that never happened.
func TestCheckPieceCompletionDB_ExternalKillOrNoCheckerIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	for _, code := range []string{"1", "0"} {
		t.Setenv(boltCheckTestExitEnv, code)
		if v := checkPieceCompletionDB(path); v.kind != boltSkipped {
			t.Fatalf("child exit %s: kind=%v reason=%q, want skipped", code, v.kind, v.reason)
		}
		r, err := repairPieceCompletionDB(dir)
		if err == nil || r.Quarantined != "" {
			t.Fatalf("child exit %s: %+v err=%v, want left alone with an error", code, r, err)
		}
		mustExist(t, path)
	}
}

// Two daemons racing at boot: the check must step aside, never move a DB the
// other process is writing to.
func TestRepairPieceCompletionDB_LockedIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	holder, err := bbolt.Open(path, 0o660, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("holder open: %v", err)
	}
	defer holder.Close()

	r, err := repairPieceCompletionDB(dir)
	if err == nil {
		t.Fatalf("locked db: want an error, got %+v", r)
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("want a 'locked' reason, got %v", err)
	}
	if r.Quarantined != "" {
		t.Fatalf("locked db must not be moved, got %+v", r)
	}
	mustExist(t, path)
}

// Environment failures are not damage: an unreadable file must be left alone,
// not quarantined (that would force a full re-hash and blame a shutdown that
// never happened). Root and Windows can read anything, so skip there.
func TestRepairPieceCompletionDB_UnreadableIsSkippedNotQuarantined(t *testing.T) {
	if os.Getuid() == 0 || filepath.Separator == '\\' {
		t.Skip("needs a non-root POSIX user for a 0000-mode file to be unreadable")
	}
	dir := t.TempDir()
	path := writeLegacyPieceCompletionDB(t, dir)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o660) })

	r, err := repairPieceCompletionDB(dir)
	if err == nil || r.Quarantined != "" {
		t.Fatalf("unreadable db: %+v err=%v, want skipped with an error", r, err)
	}
	mustExist(t, path)
	mustNotExist(t, filepath.Join(dir, PieceCompletionQuarantineName))
}

// --- end to end through the constructor the daemon uses ---------------------------

// A damaged DB must not stop NewTorrentDownloader; freelist-only damage is
// repaired with the records kept, and the downloader comes up on OUR backend
// (no cgo dependence any more: the same file on every build) and closes cleanly.
func TestNewTorrentDownloader_RepairsDamagedPieceCompletion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		separateDB bool // PieceCompletionDir set (daemon) vs. DB in DataDir (`unarr download`)
	}{
		{"state dir", true},
		{"download dir", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			cfg := TorrentConfig{DataDir: dataDir, ListenPort: 0}
			dbDir := dataDir
			if tc.separateDB {
				dbDir = t.TempDir()
				cfg.PieceCompletionDir = dbDir
			}
			path := writeLegacyPieceCompletionDB(t, dbDir)
			injectReachableFreed(t, path)

			dl, err := NewTorrentDownloader(cfg)
			if err != nil {
				t.Fatalf("downloader over damaged db: %v", err)
			}
			if err := dl.Shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			mustExist(t, filepath.Join(dbDir, PieceCompletionQuarantineName))
			mustNoRebuiltLeftover(t, dbDir)
			// Shutdown closed the store, so the live file can be inspected here.
			if v, _ := inspectBoltFile(path); v.kind != boltHealthy {
				t.Fatalf("live db after repair is not healthy: %s", v.reason)
			}
			assertFixtureRecords(t, dbDir)
		})
	}
}
