package remotefs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// Export the actual library route with the web repository's committed
// mount-alldebrid-completeness.test.ts generator. Ordinary suites explicitly
// skip; acceptance requires MOUNT_FAILURE_FIXTURE_DIR pointing to that corpus.
func TestMountActualWebFailureRetention(t *testing.T) {
	dir := os.Getenv("MOUNT_FAILURE_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set MOUNT_FAILURE_FIXTURE_DIR to the actual web failure corpus")
	}
	corpus := readWebRetentionCorpus(t, dir)
	healthy := webRetentionRecords(t, corpus, 0, 1)
	recovered := webRetentionRecords(t, corpus, len(corpus.Responses)-1, 2)
	for path, record := range healthy {
		if !reflect.DeepEqual(recovered[path], record) {
			t.Fatal("recovery fixture changed the original reference/path/size")
		}
	}
	adapter := newWebRetentionAdapter(t, corpus)
	defer adapter.Close()
	cache := t.TempDir()
	cat, source := openWebRetentionCatalog(t, cache, adapter.URL)
	defer func() { _ = cat.Close() }()
	for i, response := range corpus.Responses {
		t.Logf("actual web phase %s: HTTP %d", response.Phase, response.Status)
		adapter.phase.Store(int32(i))
		if response.Phase == "rate" {
			assertWebRetentionRateWire(t, adapter, corpus, response)
		}
		before := adapter.calls.Load()
		err := refreshWebRetentionCatalog(t, cat, source)
		if adapter.calls.Load()-before != 1 {
			t.Fatal("refresh did not consume exactly one exported library response")
		}
		want := healthy
		switch response.Phase {
		case "healthy":
			if err != nil {
				t.Fatal(err)
			}
		case "recovered":
			if err != nil {
				t.Fatal(err)
			}
			want = recovered
		default:
			if err == nil {
				t.Fatalf("%s published an authoritative successful scan", response.Phase)
			}
		}
		assertWebRetentionCatalog(t, cat, want)
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
		// Fresh WebSource and agent.Client model a restart, without another
		// refresh that could hide a persistence error by importing again.
		cat, source = openWebRetentionCatalog(t, cache, adapter.URL)
		assertWebRetentionCatalog(t, cat, want)
	}
}

type webRetentionResponse struct {
	Phase, File   string
	Status, Bytes int
	FileRequests  int
	Headers       map[string]string
	body          []byte
}

type webRetentionCorpus struct {
	Account   agent.MountAccount
	Accounts  []agent.MountAccount
	WireLimit int
	Responses []webRetentionResponse
}

func readWebRetentionCorpus(t *testing.T, dir string) webRetentionCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus webRetentionCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	phases := []string{"healthy", "http500", "invalid-json", "missing-files", "bad-node", "wrong-id", "throw", "rate", "recovered"}
	if corpus.WireLimit != 1<<20 || len(corpus.Responses) != len(phases) || corpus.Account.Provider != "alldebrid" || corpus.Account.Revision == "" {
		t.Fatal("unexpected actual-web corpus contract")
	}
	if !reflect.DeepEqual(corpus.Accounts, []agent.MountAccount{corpus.Account}) {
		t.Fatal("inconsistent actual account metadata")
	}
	statuses := []int{http.StatusOK, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusOK}
	for i := range corpus.Responses {
		response := &corpus.Responses[i]
		if response.Phase != phases[i] || response.Status != statuses[i] || response.FileRequests != 1 || filepath.Base(response.File) != response.File {
			t.Fatal("unexpected actual response phase", response.Phase)
		}
		response.body, err = os.ReadFile(filepath.Join(dir, response.File))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.body) != response.Bytes || len(response.body) > corpus.WireLimit {
			t.Fatal("exported wire size mismatch", response.File)
		}
		if response.Headers["cache-control"] != "private, no-store" || response.Headers["content-type"] != "application/json" {
			t.Fatal("missing actual sensitive-response headers", response.Phase)
		}
	}
	return corpus
}

func webRetentionRecords(t *testing.T, corpus webRetentionCorpus, phase, count int) map[string]Record {
	t.Helper()
	var page agent.MountPage
	if err := json.Unmarshal(corpus.Responses[phase].body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != count || page.Next != "" {
		t.Fatal("unexpected complete exported page", corpus.Responses[phase].Phase)
	}
	records := make(map[string]Record)
	for _, entry := range page.Entries {
		if entry.Path == "" || entry.Key == "" || entry.Reference == "" || entry.Size <= 0 {
			t.Fatal("incomplete actual entry")
		}
		path := corpus.Account.Provider + "/" + entry.Path
		records[path] = Record{Entry: Entry{Path: path, Key: corpus.Account.Revision + ":" + entry.Key, Size: entry.Size}, ID: corpus.Account.Provider, Link: entry.Reference}
	}
	if len(records) != count {
		t.Fatal("duplicate exported paths")
	}
	return records
}

type webRetentionAdapter struct {
	*httptest.Server
	phase atomic.Int32
	calls atomic.Int32
}

func newWebRetentionAdapter(t *testing.T, corpus webRetentionCorpus) *webRetentionAdapter {
	t.Helper()
	adapter := &webRetentionAdapter{}
	// The accounts envelope wraps actual account metadata from the manifest;
	// library bodies and headers are replayed untouched from route output.
	accounts, err := json.Marshal(map[string]any{"accounts": corpus.Accounts})
	if err != nil {
		t.Fatal(err)
	}
	adapter.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("unexpected request method", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/internal/agent/mount/accounts":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(accounts)
		case "/api/internal/agent/mount/library":
			query := r.URL.Query()
			if query.Get("provider") != corpus.Account.Provider || query.Get("revision") != corpus.Account.Revision || query.Get("cursor") != "" {
				t.Error("unexpected library account/cursor")
				http.Error(w, "unexpected account/cursor", http.StatusBadRequest)
				return
			}
			response := corpus.Responses[adapter.phase.Load()]
			adapter.calls.Add(1)
			for name, value := range response.Headers {
				w.Header().Set(name, value)
			}
			w.WriteHeader(response.Status)
			_, _ = w.Write(response.body)
		default:
			t.Error("unexpected route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return adapter
}

func openWebRetentionCatalog(t *testing.T, dir, baseURL string) (*Catalog, *WebSource) {
	t.Helper()
	source := &WebSource{API: agent.NewClient(baseURL, "synthetic", "web-retention-test"), AccountIdentity: "actual-web-wa03-fixture"}
	cat, err := OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	return cat, source
}

func refreshWebRetentionCatalog(t *testing.T, cat *Catalog, source *WebSource) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := cat.Refresh(ctx, source)
	if ctx.Err() != nil {
		t.Fatal("refresh exhausted its context instead of consuming the fixture", ctx.Err())
	}
	return err
}

func assertWebRetentionCatalog(t *testing.T, cat *Catalog, want map[string]Record) {
	t.Helper()
	got := make(map[string]Record)
	for _, record := range cat.records["debrid"] {
		got[record.Path] = record
		info, err := cat.FS.Stat(context.Background(), "debrid/"+record.Path)
		if err != nil || info.IsDir() || info.Size() != record.Size {
			t.Fatal("published path/size changed", record.Path, err)
		}
	}
	if !reflect.DeepEqual(got, want) || len(cat.records["debrid"]) != len(want) {
		t.Fatalf("durable reference/path/size changed: got %d records, want %d", len(got), len(want))
	}
}

func assertWebRetentionRateWire(t *testing.T, adapter *webRetentionAdapter, corpus webRetentionCorpus, response webRetentionResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	query := url.Values{"provider": {corpus.Account.Provider}, "revision": {corpus.Account.Revision}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.URL+"/api/internal/agent/mount/library?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := adapter.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(body, response.body) || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "17" {
		t.Fatal("actual rate-limit wire contract changed", resp.StatusCode, resp.Header, err)
	}
}
