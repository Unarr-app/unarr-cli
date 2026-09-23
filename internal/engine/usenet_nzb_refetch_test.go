package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

const minimalNZB = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="p@example.com" date="1700000000" subject="&quot;Film.2018.1080p.mkv&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments><segment bytes="1000" number="1">abc@news.example.com</segment></segments>
  </file>
</nzb>`

// A cached NZB the id sidecar vouches for but that no longer parses (neither
// file is fsynced; a crash can leave it empty) must be fetched again, not fail
// the task on every retry.
func TestDownloadRefetchesAnUnreadableCachedNzb(t *testing.T) {
	// DataDir per OS: XDG_DATA_HOME (Linux), LOCALAPPDATA (Windows), the
	// config dir (macOS) — all three, or CI writes into the runner's profile.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("UNARR_CONFIG_DIR", t.TempDir())
	resumeDir := filepath.Join(config.DataDir(), "resume")
	if err := os.MkdirAll(resumeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskID := "refetch-0000-0000-0000-000000000000"
	cache := filepath.Join(resumeDir, taskID+".nzb")
	if err := os.WriteFile(cache, nil, 0o644); err != nil { // truncated by a crash
		t.Fatal(err)
	}
	if err := os.WriteFile(cache+".id", []byte("nzb-1"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/nzb-download"):
			fetches.Add(1)
			_, _ = w.Write([]byte(minimalNZB))
		default: // credentials: stop the download right after the parse
			http.Error(w, "nope", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	u := NewUsenetDownloader(agent.NewClient(srv.URL, "k", "test"))
	task := NewTaskFromAgent(agent.Task{ID: taskID, Title: "Film 2018 1080p", NzbID: "nzb-1"})
	progress := make(chan Progress, 64)
	_, err := u.Download(context.Background(), task, t.TempDir(), progress)

	if err == nil || strings.Contains(err.Error(), "parse NZB") {
		t.Fatalf("err = %v, want it past the parse (a credentials failure)", err)
	}
	if fetches.Load() != 1 {
		t.Errorf("NZB fetched %d times, want 1 refetch", fetches.Load())
	}
	if data, _ := os.ReadFile(cache); !strings.Contains(string(data), "<nzb") {
		t.Error("the refetched NZB should replace the broken cache")
	}
}
