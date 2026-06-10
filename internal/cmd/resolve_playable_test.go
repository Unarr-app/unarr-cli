package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePlayableFile(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "Acme Show", "Season 01", "ep.mkv"))
	roots := []string{root}

	t.Run("allowed path resolves to itself", func(t *testing.T) {
		want := filepath.Join(root, "Acme Show", "Season 01", "ep.mkv")
		got, code, err := resolvePlayableFile(want, roots, "test")
		if err != nil {
			t.Fatalf("unexpected error (%s): %v", code, err)
		}
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("old base path relocates onto current root", func(t *testing.T) {
		got, code, err := resolvePlayableFile("/old/base/Acme Show/Season 01/ep.mkv", roots, "test")
		if err != nil {
			t.Fatalf("unexpected error (%s): %v", code, err)
		}
		want := filepath.Join(root, "Acme Show", "Season 01", "ep.mkv")
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("deleted file under old base is file_missing, never path_rejected", func(t *testing.T) {
		// The incident shape (2026-06-10): web hands a stale host path
		// (/mnt/nas/…) whose file was deleted — the docker agent can't see the
		// original path AND no tail relocates. file_missing tells the web to
		// prune the stale row; path_rejected would block that self-heal.
		_, code, err := resolvePlayableFile("/old/base/Acme Show/Season 01/gone.mkv", roots, "test")
		if err == nil {
			t.Fatal("expected error for deleted file")
		}
		if code != pathErrMissing {
			t.Errorf("code = %q, want %q", code, pathErrMissing)
		}
	})

	t.Run("existing file outside roots is path_rejected", func(t *testing.T) {
		outside := t.TempDir()
		// 1-segment-deep on purpose: a ≥3-segment tail could legitimately
		// relocate INTO the root if a same-named file existed there.
		mkfile(t, filepath.Join(outside, "leak.mkv"))
		_, code, err := resolvePlayableFile(filepath.Join(outside, "leak.mkv"), roots, "test")
		if err == nil {
			t.Fatal("expected error for out-of-root file")
		}
		if code != pathErrRejected {
			t.Errorf("code = %q, want %q", code, pathErrRejected)
		}
	})

	t.Run("missing file inside an allowed root is file_missing", func(t *testing.T) {
		_, code, err := resolvePlayableFile(filepath.Join(root, "Acme Show", "Season 01", "gone.mkv"), roots, "test")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if code != pathErrMissing {
			t.Errorf("code = %q, want %q", code, pathErrMissing)
		}
	})

	t.Run("directory resolves to its video file", func(t *testing.T) {
		got, code, err := resolvePlayableFile(filepath.Join(root, "Acme Show", "Season 01"), roots, "test")
		if err != nil {
			t.Fatalf("unexpected error (%s): %v", code, err)
		}
		want := filepath.Join(root, "Acme Show", "Season 01", "ep.mkv")
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("directory without video is no_video_file", func(t *testing.T) {
		empty := filepath.Join(root, "Empty Show")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			t.Fatal(err)
		}
		_, code, err := resolvePlayableFile(empty, roots, "test")
		if err == nil {
			t.Fatal("expected error for empty directory")
		}
		if code != pathErrNoVideo {
			t.Errorf("code = %q, want %q", code, pathErrNoVideo)
		}
	})
}
