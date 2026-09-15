package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// TestUsenetUnregisterFreesArticleCache: the decoded articles a source's requests
// shared are released when the source is unregistered (handle.Close), not left
// resident until the process-wide LRU happens to evict them.
func TestUsenetUnregisterFreesArticleCache(t *testing.T) {
	f := newReuseFixture(t, 16, 0)
	f.rangeGet(t, 3*reusePartSize, 6*reusePartSize-1)
	f.cf.settle(t)
	if f.handle.plan.CachedBytes() <= 0 {
		t.Fatal("no articles cached after serving a range")
	}
	f.handle.Close()
	if got := f.handle.plan.CachedBytes(); got != 0 {
		t.Fatalf("source still holds %d cached bytes after unregister", got)
	}
}

// TestUsenetRegisterReplaceReleasesDisplacedSource: last-writer-wins replacement
// of an id must free the displaced source's cache too.
func TestUsenetRegisterReplaceReleasesDisplacedSource(t *testing.T) {
	released := 0
	ss := NewStreamServer(0, 1)
	nopOpener := func(context.Context) io.ReadSeekCloser { return nil }
	first := newReleasableUsenetProvider("a.mkv", 1, nopOpener, func() { released++ })
	ss.RegisterUsenetSource("id", first)
	ss.RegisterUsenetSource("id", first) // same provider again: not a displacement
	if released != 0 {
		t.Fatalf("re-registering the same provider released it (%d)", released)
	}
	ss.RegisterUsenetSource("id", newReleasableUsenetProvider("b.mkv", 1, nopOpener, nil))
	if released != 1 {
		t.Fatalf("displaced provider released %d times, want 1", released)
	}
	ss.UnregisterUsenetSource("id")
	ss.UnregisterUsenetSource("id")
	if released != 1 {
		t.Fatalf("release ran %d times, want exactly 1", released)
	}
}

// TestGetOrCreateNNTPReplacesDeadPool: a cached client whose every connection has
// died is replaced (and the cached credentials dropped), instead of parking every
// later Body in acquire() until the daemon restarts.
func TestGetOrCreateNNTPReplacesDeadPool(t *testing.T) {
	fake := nntptest.NewFakeServer(t)
	host, port := fake.Addr()
	creds := &agent.UsenetCredentials{Host: host, Port: port, Username: "user", Password: "pass", MaxConnections: 2}

	// The credentials were rotated server-side: the API now hands out the right
	// password, while the caller (and the cache) still hold the old one.
	fake.RequireCorrectAuth()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/usenet-credentials" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(creds)
	}))
	t.Cleanup(api.Close)
	stale := *creds
	stale.Password = "rotated-away"

	u := NewUsenetDownloader(agent.NewClient(api.URL, "", "test"))
	dead := nntp.NewClient(nntp.Config{Host: host, Port: port}) // never connected: 0 live connections
	u.nntpClient = dead
	u.credentials = &stale
	u.credExpiry = time.Now().Add(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := u.getOrCreateNNTP(ctx, &stale)
	if err != nil {
		t.Fatalf("getOrCreateNNTP with rotated credentials: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client == dead || client.ActiveConnections() != 2 {
		t.Fatalf("got client %p with %d connections; want a fresh connected pool", client, client.ActiveConnections())
	}
	if u.credentials == nil || u.credentials.Password != creds.Password {
		t.Fatalf("cached credentials = %+v, want the refetched ones", u.credentials)
	}
}

// TestGetOrCreateNNTPConnectFailureForgetsCredentials: bad cached credentials must
// not be replayed for their whole TTL after they failed to connect.
func TestGetOrCreateNNTPConnectFailureForgetsCredentials(t *testing.T) {
	fake := nntptest.NewFakeServer(t)
	fake.RequireCorrectAuth()
	host, port := fake.Addr()
	bad := &agent.UsenetCredentials{Host: host, Port: port, Username: "user", Password: "wrong", MaxConnections: 1}

	u := NewUsenetDownloader(agent.NewClient("http://localhost", "", "test"))
	u.credentials = bad
	u.credExpiry = time.Now().Add(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := u.getOrCreateNNTP(ctx, bad); err == nil {
		t.Fatal("connected with a wrong password")
	}
	if u.credentials != nil {
		t.Fatal("credentials that failed to connect are still cached")
	}
}
