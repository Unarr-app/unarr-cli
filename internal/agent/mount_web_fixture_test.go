package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The web worker exports actual serialized pages (including Unicode names and
// signed within-release cursors). Run with MOUNT_WEB_FIXTURE_DIR set to that
// export; keeping multi-megabyte generated data out of the source repository.
func TestMountActualWebPages(t *testing.T) {
	dir := os.Getenv("MOUNT_WEB_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set MOUNT_WEB_FIXTURE_DIR to the web contract fixture export")
	}
	for _, name := range []string{"1000-3", "1-4000"} {
		t.Run(name, func(t *testing.T) { verifyWebMountPages(t, filepath.Join(dir, name)) })
	}
}

type webMountManifest struct {
	WireLimit, EntryCount int
	Pages                 []struct {
		File, RequestCursor, Next string
		Bytes, Entries            int
	}
}

func verifyWebMountPages(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest webMountManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.WireLimit != 1048576 {
		t.Fatal("unexpected contract cap", manifest.WireLimit)
	}
	bodies := make(map[string][]byte)
	expected := make(map[string]MountPage)
	for _, item := range manifest.Pages {
		body, err := os.ReadFile(filepath.Join(dir, item.File))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > manifest.WireLimit || len(body) != item.Bytes {
			t.Fatal("invalid exported size", item.File, len(body))
		}
		var page MountPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) != item.Entries || page.Next != item.Next {
			t.Fatal("manifest mismatch", item.File)
		}
		bodies[item.RequestCursor], expected[item.RequestCursor] = body, page
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Query().Get("cursor")]
		if !ok {
			http.Error(w, "unexpected opaque cursor", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "synthetic", "test")
	seen := make(map[string]bool)
	cursor := ""
	for range manifest.Pages {
		page, err := client.MountLibrary(context.Background(), "torbox", "fixture-revision", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(page, expected[cursor]) {
			t.Fatal("wire fields or opaque cursor changed")
		}
		for _, entry := range page.Entries {
			if entry.Path == "" || entry.Key == "" || entry.Size <= 0 || entry.Reference == "" || seen[entry.Key] {
				t.Fatal("missing or duplicate entry", entry.Path)
			}
			seen[entry.Key] = true
		}
		cursor = page.Next
	}
	if cursor != "" || len(seen) != manifest.EntryCount {
		t.Fatal("truncated export", len(seen), manifest.EntryCount)
	}
}
