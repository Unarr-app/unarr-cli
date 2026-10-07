package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"
)

// Start the web's committed mount-paced-route.test.ts live harness separately,
// then set MOUNT_PACED_LIVE_FILE to its descriptor. Auth/config/provider transport
// are synthetic; actual route/service/cache/pacing/clock are never replaced.
// The fixture owner closes the lease after red/green runs; each subtest gets a
// distinct cold owner/revision so a failed old request cannot warm the next run.
func TestMountActualPacedWebCalls(t *testing.T) {
	file := os.Getenv("MOUNT_PACED_LIVE_FILE")
	if file == "" {
		t.Skip("set MOUNT_PACED_LIVE_FILE to the actual web paced-route lease")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor struct{ NewURL string }
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal(err)
	}
	requirePacedLoopback(t, descriptor.NewURL)
	t.Run("cold-and-warm", func(t *testing.T) {
		scope := newPacedWebScope(t, descriptor.NewURL)
		client := NewClient(scope.BaseURL, "synthetic-paced-agent", "test")
		accounts, err := client.MountAccounts(t.Context())
		if err != nil || !reflect.DeepEqual(accounts, scope.Accounts) {
			t.Fatalf("actual accounts changed: %v %v", accounts, err)
		}
		account := accounts[0]
		start := time.Now()
		page, err := client.MountLibrary(t.Context(), account.Provider, account.Revision, "")
		elapsed := time.Since(start)
		t.Logf("scope=%s cold=%s entries=%d error=%v", scope.BaseURL, elapsed, len(page.Entries), err)
		if err != nil {
			t.Fatal("healthy actual cold page was aborted", err)
		}
		if elapsed < 5700*time.Millisecond || len(page.Entries) != scope.ExpectedEntries || page.Next == "" {
			t.Fatal("fixture did not exercise the real cold ten-release page")
		}
		start = time.Now()
		warm, err := client.MountLibrary(t.Context(), account.Provider, account.Revision, "")
		elapsed = time.Since(start)
		t.Logf("warm=%s entries=%d error=%v", elapsed, len(warm.Entries), err)
		if err != nil || !reflect.DeepEqual(page, warm) || elapsed >= 3*time.Second {
			t.Fatal("warm route changed its metadata or failed to reuse the actual cache", err)
		}
	})
	t.Run("queued-resolve", func(t *testing.T) {
		scope := newPacedWebScope(t, descriptor.NewURL)
		// Seed signed references through the actual route with an independent
		// bounded control transport, allowing resolve to run even when old Go
		// MountLibrary fails its separate cold-page regression above.
		account := scope.Accounts[0]
		query := url.Values{"provider": {account.Provider}, "revision": {account.Revision}}
		var page MountPage
		pacedFixtureJSON(t, scope.BaseURL+"/api/internal/agent/mount/library?"+query.Encode(), &page)
		if len(page.Entries) != scope.ExpectedEntries {
			t.Fatal("actual reference seed is incomplete")
		}
		verifyPacedResolveQueue(t, scope, page.Entries[0].Reference)
	})
	t.Run("cancel-cold-page", func(t *testing.T) {
		scope := newPacedWebScope(t, descriptor.NewURL)
		account := scope.Accounts[0]
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := NewClient(scope.BaseURL, "synthetic-paced-agent", "test").MountLibrary(ctx, account.Provider, account.Revision, "")
		t.Logf("cancel=%s error=%v", time.Since(start), err)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) >= time.Second {
			t.Fatal("cold provider work ignored caller cancellation", err)
		}
	})
}

type pacedWebScope struct {
	BaseURL         string
	Accounts        []MountAccount
	ExpectedEntries int
	PacingMs        int
}

func requirePacedLoopback(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		t.Fatal("paced fixture must use synthetic IPv4 loopback", raw, err)
	}
}

func pacedFixtureJSON(t *testing.T, raw string, out any) {
	t.Helper()
	requirePacedLoopback(t, raw)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := transport.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("actual fixture route failed", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func newPacedWebScope(t *testing.T, newURL string) pacedWebScope {
	t.Helper()
	var scope pacedWebScope
	pacedFixtureJSON(t, newURL, &scope)
	requirePacedLoopback(t, scope.BaseURL)
	if len(scope.Accounts) != 1 || scope.Accounts[0].Provider != "real-debrid" || scope.Accounts[0].Revision == "" || scope.ExpectedEntries != 10 || scope.PacingMs != 600 {
		t.Fatal("unexpected actual paced scope", scope)
	}
	return scope
}

func verifyPacedResolveQueue(t *testing.T, scope pacedWebScope, reference string) {
	t.Helper()
	client := NewClient(scope.BaseURL, "synthetic-paced-agent", "test")
	start := make(chan struct{})
	type result struct {
		url     string
		err     error
		elapsed time.Duration
	}
	results := make(chan result, 8)
	for range 8 {
		go func() {
			<-start
			began := time.Now()
			u, err := client.MountResolve(t.Context(), reference)
			results <- result{u, err, time.Since(began)}
		}()
	}
	close(start)
	var slowest time.Duration
	failed := false
	for range 8 {
		got := <-results
		if got.elapsed > slowest {
			slowest = got.elapsed
		}
		if got.err != nil || got.url != "https://media.invalid/0" {
			failed = true
			t.Errorf("queued actual resolve aborted after %s: url=%q error=%v", got.elapsed, got.url, got.err)
		}
	}
	t.Logf("scope=%s resolves=8 slowest=%s", scope.BaseURL, slowest)
	if !failed && slowest < 4*time.Second {
		t.Error("fixture did not exercise the real five-second resolution queue")
	}
}
