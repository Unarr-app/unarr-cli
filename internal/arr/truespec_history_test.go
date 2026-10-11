package arr

// `unarr arr sync` against the failure modes of a real *arr history: orphan
// file ids (500 on the batch GET), >1000 history rows, grabs outside the
// window, User-Agent, partial writes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type historyArr struct {
	t          *testing.T
	live       map[int]int // existing file id -> indexerFlags
	imports    []map[string]any
	grabs      []map[string]any
	batchCalls atomic.Int32
	oneCalls   atomic.Int32
	putBodies  [][]FlagChange
	uas        []string
	histCalls  atomic.Int32
}

func (a *historyArr) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/history", func(w http.ResponseWriter, r *http.Request) {
		a.histCalls.Add(1)
		a.uas = append(a.uas, r.UserAgent())
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
		var src []map[string]any
		switch r.URL.Query().Get("eventType") {
		case "3":
			src = a.imports
		case "1":
			src = a.grabs
		}
		lo := (page - 1) * size
		hi := min(lo+size, len(src))
		recs := []map[string]any{}
		if lo < len(src) {
			recs = src[lo:hi]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": recs, "totalRecords": len(src), "page": page, "pageSize": size})
	})
	// Batch GET: Sonarr/Radarr answer 500 if ANY id does not exist.
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		a.batchCalls.Add(1)
		var out []map[string]any
		for _, s := range r.URL.Query()["episodeFileIds"] {
			id, _ := strconv.Atoi(s)
			fl, ok := a.live[id]
			if !ok {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"message":"Sequence contains no matching element","description":"System.InvalidOperationException..."}`))
				return
			}
			out = append(out, map[string]any{"id": id, "indexerFlags": fl})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/v3/episodefile/bulk", func(w http.ResponseWriter, r *http.Request) {
		var body []FlagChange
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.putBodies = append(a.putBodies, body)
		_, _ = w.Write([]byte(`[]`))
	})
	// Per-id GET: 404 for an orphan.
	mux.HandleFunc("/api/v3/episodefile/", func(w http.ResponseWriter, r *http.Request) {
		a.oneCalls.Add(1)
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"))
		fl, ok := a.live[id]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"NotFound"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "indexerFlags": fl})
	})
	srv := httptest.NewServer(mux)
	a.t.Cleanup(srv.Close)
	return srv
}

// Distinct 5-hex prefix per n (a real library spreads over 16^5 buckets).
func hashN(n int) string { return fmt.Sprintf("%05x%035x", (n*7919)%(1<<20), n+0x1000) }

func imp(fileID int, hash string, at time.Time) map[string]any {
	return map[string]any{"eventType": "downloadFolderImported", "downloadId": strings.ToUpper(hash),
		"date": at.UTC().Format(time.RFC3339Nano), "data": map[string]any{"fileId": strconv.Itoa(fileID)}}
}
func grab(hash, indexer string, at time.Time) map[string]any {
	return map[string]any{"eventType": "grabbed", "downloadId": strings.ToUpper(hash),
		"date": at.UTC().Format(time.RFC3339Nano), "data": map[string]any{"indexer": indexer}}
}

func allVerified(prefixesSeen *[][]string) LookupFunc {
	return func(_ context.Context, prefixes []string) (map[string]TrueSpecState, error) {
		*prefixesSeen = append(*prefixesSeen, prefixes)
		out := map[string]TrueSpecState{}
		for n := 0; n < 3000; n++ {
			h := hashN(n)
			for _, p := range prefixes {
				if strings.HasPrefix(h, p) {
					out[h] = TrueSpecState{Verified: true}
				}
			}
		}
		return out, nil
	}
}

// [BUG b] One orphan id in the history makes the batch GET 500 and the whole
// sync abort — exactly the user's failure.
func TestSyncSkipsOrphanFileIDs(t *testing.T) {
	now := time.Now()
	a := &historyArr{t: t, live: map[int]int{1: 0, 2: 0}}
	a.imports = []map[string]any{
		imp(1, hashN(1), now.Add(-time.Hour)),
		imp(999, hashN(999), now.Add(-2*time.Hour)), // replaced by an upgrade: id no longer exists
		imp(2, hashN(2), now.Add(-3*time.Hour)),
	}
	a.grabs = []map[string]any{grab(hashN(1), "TorrentClaw (Prowlarr)", now.Add(-time.Hour))}
	srv := a.server()
	var seen [][]string
	rep, err := SyncTrueSpecFlags(context.Background(), NewClient(srv.URL, "k"), "sonarr", allVerified(&seen), SyncOptions{TCMatch: "torrentclaw"})
	// The orphan is skipped, the two live files are flagged.
	if err != nil {
		t.Fatalf("MediaFiles should survive an orphan id: %v", err)
	}
	if rep.Checked != 2 || len(rep.Changes) != 2 || len(a.putBodies) != 1 || len(a.putBodies[0]) != 2 {
		t.Fatalf("rep=%+v puts=%+v", rep, a.putBodies)
	}
	if a.oneCalls.Load() != 1 {
		t.Fatalf("expected exactly one per-id GET (the orphan), got %d", a.oneCalls.Load())
	}
}

// A Sonarr behind Cloudflare/Authelia must not see Go's "Go-http-client/1.1".
func TestArrClientSendsOwnUserAgent(t *testing.T) {
	prev := userAgent
	t.Cleanup(func() { userAgent = prev })
	SetUserAgent("unarr/9.9.9")
	a := &historyArr{t: t, live: map[int]int{}}
	srv := a.server()
	_, _ = SyncTrueSpecFlags(context.Background(), NewClient(srv.URL, "k"), "sonarr", nil, SyncOptions{})
	if len(a.uas) == 0 || a.uas[0] != "unarr/9.9.9" {
		t.Fatalf("client must send its own UA, got %q", a.uas)
	}
}

// >1000 history rows: pagination, window cutoff and full grab walk.
func TestSyncPaginationWindowAndGrabsOutsideWindow(t *testing.T) {
	now := time.Now()
	a := &historyArr{t: t, live: map[int]int{}}
	// 1500 imports, all live, newest first; 1200 within last 30 days, 300 older.
	for i := 0; i < 1500; i++ {
		age := time.Duration(i) * 40 * time.Minute // 1500*40min ≈ 41 days
		a.imports = append(a.imports, imp(i, hashN(i), now.Add(-age)))
		a.live[i] = 0
		// the grab for every file happened 60 days before its import (outside window)
		a.grabs = append(a.grabs, grab(hashN(i), "TorrentClaw", now.Add(-age-60*24*time.Hour)))
	}
	srv := a.server()
	var seen [][]string
	rep, err := SyncTrueSpecFlags(context.Background(), NewClient(srv.URL, "k"), "sonarr", allVerified(&seen),
		SyncOptions{TCMatch: "torrentclaw", Since: now.AddDate(0, 0, -30), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	inWindow := 0
	for i := 0; i < 1500; i++ {
		if !now.Add(-time.Duration(i) * 40 * time.Minute).Before(now.AddDate(0, 0, -30)) {
			inWindow++
		}
	}
	if rep.Checked != inWindow {
		t.Fatalf("checked %d, want %d (window)", rep.Checked, inWindow)
	}
	if len(rep.Changes) != inWindow {
		t.Fatalf("all in-window files should get Internal: %d/%d", len(rep.Changes), inWindow)
	}
	for _, c := range rep.Changes {
		if c.Flags != 8 || !c.FromTC {
			t.Fatalf("flags=%d fromTC=%v (grab outside window must still be found)", c.Flags, c.FromTC)
		}
	}
	for _, b := range seen {
		if len(b) > 100 {
			t.Fatalf("lookup batch > 100: %d", len(b))
		}
		for _, p := range b {
			if len(p) != 5 {
				t.Fatalf("prefix leak: %q", p)
			}
		}
	}
}

// Lookup failure on the SECOND batch must leave *arr untouched.
func TestSyncLookupFailureMidwayWritesNothing(t *testing.T) {
	now := time.Now()
	a := &historyArr{t: t, live: map[int]int{}}
	for i := 0; i < 250; i++ {
		a.imports = append(a.imports, imp(i, hashN(i), now.Add(-time.Hour)))
		a.live[i] = 0
	}
	srv := a.server()
	calls := 0
	lookup := func(ctx context.Context, prefixes []string) (map[string]TrueSpecState, error) {
		calls++
		if calls == 2 {
			return nil, fmt.Errorf("HTTP 429: rate limited")
		}
		return map[string]TrueSpecState{}, nil
	}
	_, err := SyncTrueSpecFlags(context.Background(), NewClient(srv.URL, "k"), "sonarr", lookup, SyncOptions{})
	if err == nil || len(a.putBodies) != 0 {
		t.Fatalf("expected abort without writes, err=%v puts=%d", err, len(a.putBodies))
	}
}

// Bulk PUT payload must carry only id + indexerFlags.
func TestBulkPutPayloadShape(t *testing.T) {
	b, _ := json.Marshal([]FlagChange{{ID: 7, Flags: 8, Old: 0, Hash: hashN(1), Verified: true, FromTC: true}})
	if string(b) != `[{"id":7,"indexerFlags":8}]` {
		t.Fatalf("payload = %s", b)
	}
}

// Bisection must isolate several orphans scattered through a batch.
func TestMediaFilesBisectsScatteredOrphans(t *testing.T) {
	a := &historyArr{t: t, live: map[int]int{}}
	var ids []int
	for i := 1; i <= 40; i++ {
		ids = append(ids, i)
		if i%7 != 0 { // 7, 14, 21, 28, 35 are orphans
			a.live[i] = 0
		}
	}
	srv := a.server()
	files, err := NewClient(srv.URL, "k").MediaFiles("sonarr", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 35 {
		t.Fatalf("got %d files, want 35", len(files))
	}
}

// Only a 404 on a single id means "orphan"; anything else is a real failure.
func TestMediaFilesSingleIDNon404Aborts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	mux.HandleFunc("/api/v3/episodefile/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "k").MediaFiles("sonarr", []int{1, 2}); err == nil {
		t.Fatal("a 503 on a single id must abort, not be treated as an orphan")
	}
}

// A 401 on the batch is not a missing id: no bisection, straight error.
func TestMediaFilesAuthErrorDoesNotBisect(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "k").MediaFiles("radarr", []int{1, 2, 3, 4}); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestWithTimeout(t *testing.T) {
	c := NewClient("http://x", "k").WithTimeout(60 * time.Second)
	if c.httpClient.Timeout != 60*time.Second {
		t.Fatalf("timeout = %v", c.httpClient.Timeout)
	}
}
