package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// `unarr clean` must know the piece-completion pre-flight's artefacts — the
// quarantined DB and an orphaned rebuilt copy — and must never touch the live
// DB. Each artefact is a full-size bolt file, so a clean that ignores them
// leaves tens of MB behind while reporting "Nothing to clean".
func TestCleanTargetsIncludePieceCompletionArtefacts(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, engine.PieceCompletionDBName)
	quarantined := filepath.Join(dir, engine.PieceCompletionQuarantineName)
	rebuilt := live + engine.PieceCompletionRebuiltSuffix
	for _, p := range []string{live, quarantined, rebuilt} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	found, files, bytes := scanCleanTargets(cleanTargetsFor(cleanOpts{}, dir))
	got := map[string]bool{}
	for _, f := range found {
		got[f.path] = true
	}
	if !got[quarantined] || !got[rebuilt] {
		t.Fatalf("quarantine artefacts missing from clean targets: %v", found)
	}
	if got[live] {
		t.Fatalf("clean must never target the live piece-completion db: %v", found)
	}
	if files < 2 || bytes < 2 {
		t.Fatalf("files=%d bytes=%d, want the two artefacts counted", files, bytes)
	}
}
