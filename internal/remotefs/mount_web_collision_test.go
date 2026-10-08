package remotefs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"golang.org/x/text/unicode/norm"
)

// The external corpus contains actual accounts/library/resolve route output.
// Run the same expected-all-files assertion against red and green exports;
// this consumer never repairs or rewrites the supplied wire bodies.
func TestMountActualWebPortableCollisionCorpus(t *testing.T) {
	dir := os.Getenv("MOUNT_CONTRACT_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set MOUNT_CONTRACT_FIXTURE_DIR to the actual web collision corpus")
	}
	corpus, expected := readWebCollisionCorpus(t, dir)
	adapter := newWebCollisionAdapter(t, corpus)
	defer adapter.Close()
	cache := t.TempDir()
	cat, source, _ := openWebCollisionCatalog(t, cache, adapter.URL)
	defer func() { _ = cat.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cat.Refresh(ctx, source)
	adapter.assertPages(t)
	if err != nil {
		t.Fatalf("actual route corpus could not publish all %d files: %v", len(expected), err)
	}
	assertWebCollisionCatalog(t, cat, expected)
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, api := openWebCollisionCatalog(t, cache, adapter.URL)
	cat = reopened
	assertWebCollisionCatalog(t, cat, expected)
	// Submit the exact persisted opaque references to the real Go API client.
	// Resolve returns captured route URLs; no media URL is fetched.
	byReference := make(map[string]Record)
	for _, record := range cat.records["debrid"] {
		byReference[record.Link] = record
	}
	for _, resolve := range corpus.Resolves {
		record, ok := byReference[resolve.Reference]
		if !ok || record.Path != "real-debrid/"+resolve.Path || resolve.Original == "" {
			t.Fatal("original signed reference/display path lost", resolve.Path)
		}
		url, err := api.MountResolve(ctx, record.Link)
		if err != nil || url != resolve.URL {
			t.Fatal("persisted original reference no longer resolves", resolve.Original, err)
		}
	}
	adapter.assertResolves(t)
	if ctx.Err() != nil {
		t.Fatal("fixture traversal exhausted its context", ctx.Err())
	}
	t.Logf("imported and reopened %d exact records, all pages once, %d original resolves", len(expected), len(corpus.Resolves))
}

type webCollisionWire struct {
	File    string
	Status  int
	Bytes   int
	Headers map[string]string
	body    []byte
}

type webCollisionPage struct {
	webCollisionWire
	Provider, Revision, RequestCursor, Next string
	Entries                                 int
}

type webCollisionCorpus struct {
	WireLimit       int
	AccountResponse webCollisionWire
	Accounts        []struct {
		Provider, Revision string
		EntryCount         int
		Pages              []webCollisionPage
	}
	Resolves []struct {
		webCollisionWire
		Reference, Original, Path, URL string
	}
}

func readWebCollisionWire(t *testing.T, dir string, wire *webCollisionWire) {
	t.Helper()
	if filepath.Base(wire.File) != wire.File || wire.Status != http.StatusOK {
		t.Fatal("unexpected actual wire response", wire.File)
	}
	body, err := os.ReadFile(filepath.Join(dir, wire.File))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != wire.Bytes || len(body) > 1<<20 || wire.Headers["cache-control"] != "private, no-store" || wire.Headers["content-type"] != "application/json" {
		t.Fatal("invalid actual wire bytes/headers", wire.File)
	}
	wire.body = body
}

func readWebCollisionCorpus(t *testing.T, dir string) (webCollisionCorpus, map[string]Record) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus webCollisionCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.WireLimit != 1<<20 || len(corpus.Accounts) != 2 || len(corpus.Resolves) != 10 {
		t.Fatal("unexpected actual collision corpus")
	}
	readWebCollisionWire(t, dir, &corpus.AccountResponse)
	expected := make(map[string]Record)
	references := make(map[string]bool)
	for i := range corpus.Accounts {
		account := &corpus.Accounts[i]
		cursor, count := "", 0
		for j := range account.Pages {
			page := &account.Pages[j]
			readWebCollisionWire(t, dir, &page.webCollisionWire)
			var decoded agent.MountPage
			if err := json.Unmarshal(page.body, &decoded); err != nil {
				t.Fatal(err)
			}
			if page.Provider != account.Provider || page.Revision != account.Revision || page.RequestCursor != cursor || page.Next != decoded.Next || page.Entries != len(decoded.Entries) {
				t.Fatal("actual opaque page traversal mismatch", page.File)
			}
			for _, entry := range decoded.Entries {
				key := account.Revision + ":" + entry.Key
				if _, duplicate := expected[key]; duplicate || references[entry.Reference] || entry.Reference == "" {
					t.Fatal("duplicate/missing actual key/reference", page.File)
				}
				references[entry.Reference] = true
				expected[key] = Record{Entry: Entry{Path: account.Provider + "/" + entry.Path, Key: key, Size: entry.Size}, ID: account.Provider, Link: entry.Reference}
				count++
			}
			cursor = decoded.Next
		}
		if cursor != "" || count != account.EntryCount {
			t.Fatal("incomplete actual pagination", account.Provider, count)
		}
	}
	if len(expected) != 2011 {
		t.Fatal("expected 2010 collision-account files and one independent TorBox file", len(expected))
	}
	for i := range corpus.Resolves {
		readWebCollisionWire(t, dir, &corpus.Resolves[i].webCollisionWire)
	}
	return corpus, expected
}

type webCollisionAdapter struct {
	*httptest.Server
	mu       sync.Mutex
	pages    map[string]int
	resolves map[string]int
}

func newWebCollisionAdapter(t *testing.T, corpus webCollisionCorpus) *webCollisionAdapter {
	t.Helper()
	adapter := &webCollisionAdapter{pages: make(map[string]int), resolves: make(map[string]int)}
	wires := make(map[string]webCollisionWire)
	for _, account := range corpus.Accounts {
		for _, page := range account.Pages {
			key := page.Provider + "\x00" + page.Revision + "\x00" + page.RequestCursor
			wires[key] = page.webCollisionWire
			adapter.pages[key] = 0
		}
	}
	resolveWires := make(map[string]webCollisionWire)
	for _, resolve := range corpus.Resolves {
		resolveWires[resolve.Reference] = resolve.webCollisionWire
		adapter.resolves[resolve.Reference] = 0
	}
	adapter.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire webCollisionWire
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/internal/agent/mount/accounts":
			wire = corpus.AccountResponse
		case r.Method == http.MethodGet && r.URL.Path == "/api/internal/agent/mount/library":
			query := r.URL.Query()
			key := query.Get("provider") + "\x00" + query.Get("revision") + "\x00" + query.Get("cursor")
			wire = wires[key]
			adapter.mu.Lock()
			adapter.pages[key]++
			adapter.mu.Unlock()
		case r.Method == http.MethodPost && r.URL.Path == "/api/internal/agent/mount/resolve":
			var request struct{ Reference string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			wire = resolveWires[request.Reference]
			adapter.mu.Lock()
			adapter.resolves[request.Reference]++
			adapter.mu.Unlock()
		}
		if wire.body == nil {
			t.Error("unexpected actual-corpus request", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		for name, value := range wire.Headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(wire.Status)
		_, _ = w.Write(wire.body)
	}))
	return adapter
}

func (adapter *webCollisionAdapter) assertPages(t *testing.T) {
	t.Helper()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	for key, count := range adapter.pages {
		if count != 1 {
			t.Fatal("actual page lost or requested twice", key, count)
		}
	}
}

func (adapter *webCollisionAdapter) assertResolves(t *testing.T) {
	t.Helper()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	for reference, count := range adapter.resolves {
		if count != 1 {
			t.Fatal("original reference lost or resolved twice", reference, count)
		}
	}
}

func openWebCollisionCatalog(t *testing.T, dir, baseURL string) (*Catalog, *WebSource, *agent.Client) {
	t.Helper()
	api := agent.NewClient(baseURL, "synthetic", "web-collision-test")
	source := &WebSource{API: api, AccountIdentity: "actual-web-portable-collision"}
	cat, err := OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	return cat, source, api
}

func assertWebCollisionCatalog(t *testing.T, cat *Catalog, expected map[string]Record) {
	t.Helper()
	got := make(map[string]Record)
	portable := make(map[string]bool)
	for _, record := range cat.records["debrid"] {
		got[record.Key] = record
		path := strings.ToLower(norm.NFC.String(record.Path))
		if portable[path] {
			t.Fatal("portable path collision survived publication", record.Path)
		}
		portable[path] = true
		info, err := cat.FS.Stat(context.Background(), "debrid/"+record.Path)
		if err != nil || info.IsDir() || info.Size() != record.Size {
			t.Fatal("published actual path/size changed", record.Path, err)
		}
	}
	if !reflect.DeepEqual(got, expected) || len(cat.records["debrid"]) != len(expected) {
		t.Fatal("actual pagination/reference retention lost or duplicated files", len(got), len(expected))
	}
}
