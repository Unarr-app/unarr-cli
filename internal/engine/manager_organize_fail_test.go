package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// validFileDownloader writes a single plausible (>1 MiB) video file so verify()
// passes; the only thing that can then fail the pipeline is organize().
type validFileDownloader struct {
	dir string
}

func (m *validFileDownloader) Method() DownloadMethod { return MethodTorrent }
func (m *validFileDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return true, nil
}
func (m *validFileDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	path := filepath.Join(m.dir, "movie.mkv")
	size := int64(minPlausibleVideoBytes + 4096)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		return nil, err
	}
	return &Result{FilePath: path, FileName: "movie.mkv", Method: MethodTorrent, Size: size}, nil
}
func (m *validFileDownloader) Pause(_ string) error             { return nil }
func (m *validFileDownloader) Cancel(_ string) error            { return nil }
func (m *validFileDownloader) Shutdown(_ context.Context) error { return nil }

// TestManagerPipeline_OrganizeError_MarksFailed (RC-7) asserts that when organize()
// returns an error the task is reported FAILED, not completed. Previously the error
// was logged as a warning and the task completed anyway — the library then showed a
// green item pointing at a file never filed into Movies/TV.
//
// organize() is forced to fail by pointing MoviesDir at a regular FILE: the
// os.MkdirAll(MoviesDir/<title>) inside moveToDir then fails with "not a directory".
func TestManagerPipeline_OrganizeError_MarksFailed(t *testing.T) {
	base := t.TempDir()
	downloadDir := filepath.Join(base, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}

	// MoviesDir is a FILE, not a directory → MkdirAll under it fails inside organize.
	moviesDir := filepath.Join(base, "movies-as-file")
	if err := os.WriteFile(moviesDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write movies file: %v", err)
	}

	pr, reporter := captureReporter()
	dl := &validFileDownloader{dir: downloadDir}

	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     downloadDir,
		Organize: OrganizeConfig{
			Enabled:   true,
			MoviesDir: moviesDir,
			OutputDir: downloadDir,
		},
	}, pr, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	const taskID = "organize-fail-12345678"
	mgr.Submit(ctx, agent.Task{
		ID:              taskID,
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Organize Fail Movie",
		ContentType:     "movie",
		ContentTitle:    "Organize Fail Movie",
		PreferredMethod: "torrent",
	})
	mgr.Wait()

	u := terminalUpdate(t, reporter, taskID)
	if u.Status != "failed" {
		t.Errorf("final status = %q (%s), want failed (organize error must not complete)", u.Status, u.ErrorMessage)
	}
}

// TestManagerPipeline_OrganizeDisabled_Completes guards the no-op path RC-7 must
// NOT break: when organize is disabled it returns (result.FilePath, nil) — no
// error — and the task must still complete.
func TestManagerPipeline_OrganizeDisabled_Completes(t *testing.T) {
	downloadDir := t.TempDir()

	pr, reporter := captureReporter()
	dl := &validFileDownloader{dir: downloadDir}

	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     downloadDir,
		Organize:      OrganizeConfig{Enabled: false},
	}, pr, dl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	const taskID = "organize-noop-12345678"
	mgr.Submit(ctx, agent.Task{
		ID:              taskID,
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Organize Disabled Movie",
		PreferredMethod: "torrent",
	})
	mgr.Wait()

	u := terminalUpdate(t, reporter, taskID)
	if u.Status != "completed" {
		t.Errorf("final status = %q (%s), want completed (disabled organize is a no-op, not an error)", u.Status, u.ErrorMessage)
	}
}
