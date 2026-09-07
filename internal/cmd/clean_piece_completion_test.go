package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// `unarr clean` must know the piece-completion pre-flight's artefacts — the
// quarantined DB, rebuilt copies orphaned mid-swap (one per pid), the SQLite
// cache of cgo builds with its journal — and must never touch the live DB.
// Each is a full-size DB file, so a clean that ignores them leaves tens of MB
// behind while reporting "Nothing to clean".
func TestCleanTargetsIncludePieceCompletionArtefacts(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, engine.PieceCompletionDBName)
	artefacts := []string{
		filepath.Join(dir, engine.PieceCompletionQuarantineName),
		live + engine.PieceCompletionRebuiltSuffix + ".4242",
		live + engine.PieceCompletionRebuiltSuffix + ".9",
		filepath.Join(dir, engine.PieceCompletionLegacySQLiteName),
		filepath.Join(dir, engine.PieceCompletionLegacySQLiteName+"-wal"),
		filepath.Join(dir, engine.PieceCompletionLegacySQLiteName+"-shm"),
	}
	for _, p := range append([]string{live}, artefacts...) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	found, files, bytes := scanCleanTargets(cleanTargetsFor(cleanOpts{}, dir))
	got := map[string]bool{}
	for _, f := range found {
		got[f.path] = true
	}
	for _, p := range artefacts {
		if !got[p] {
			t.Errorf("artefact missing from clean targets: %s", p)
		}
	}
	if got[live] {
		t.Fatalf("clean must never target the live piece-completion db: %v", found)
	}
	if files < len(artefacts) || bytes < int64(len(artefacts)) {
		t.Fatalf("files=%d bytes=%d, want the %d artefacts counted", files, bytes, len(artefacts))
	}
}
