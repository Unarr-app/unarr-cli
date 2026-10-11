package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// lookupServer answers `fail` times with status (and Retry-After when set),
// then with an empty, successful lookup.
func lookupServer(t *testing.T, status int, retryAfter string, fail int32, calls *atomic.Int32) *agent.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= fail {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(srv.Close)
	return agent.NewClient(srv.URL, "key", "unarr/test")
}

func captureSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	prev := lookupSleep
	lookupSleep = func(d time.Duration) { got = append(got, d) }
	t.Cleanup(func() { lookupSleep = prev })
	return &got
}

func TestTrueSpecLookupRetries429HonouringRetryAfter(t *testing.T) {
	sleeps := captureSleeps(t)
	var n atomic.Int32
	ac := lookupServer(t, 429, "7", 2, &n)
	if _, err := trueSpecLookup(ac)(context.Background(), []string{"abcde"}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 || len(*sleeps) != 2 || (*sleeps)[0] != 7*time.Second {
		t.Fatalf("attempts=%d sleeps=%v", n.Load(), *sleeps)
	}
}

func TestTrueSpecLookupCapsRetryAfter(t *testing.T) {
	sleeps := captureSleeps(t)
	var n atomic.Int32
	ac := lookupServer(t, 429, "9999", 1, &n)
	if _, err := trueSpecLookup(ac)(context.Background(), []string{"abcde"}); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != maxRetryPause {
		t.Fatalf("sleeps=%v, want one pause of %v", *sleeps, maxRetryPause)
	}
}

func TestTrueSpecLookupRetries5xxWithGrowingPauseThenGivesUp(t *testing.T) {
	sleeps := captureSleeps(t)
	var n atomic.Int32
	ac := lookupServer(t, 503, "", 100, &n)
	_, err := trueSpecLookup(ac)(context.Background(), []string{"abcde"})
	if err == nil {
		t.Fatal("expected failure after retries")
	}
	if n.Load() != lookupRetries+1 || len(*sleeps) != lookupRetries {
		t.Fatalf("attempts=%d sleeps=%v", n.Load(), *sleeps)
	}
	if (*sleeps)[0] >= (*sleeps)[1] {
		t.Fatalf("pause should grow: %v", *sleeps)
	}
}

func TestTrueSpecLookupDoesNotRetryAuthErrors(t *testing.T) {
	sleeps := captureSleeps(t)
	for _, status := range []int{401, 403} {
		var n atomic.Int32
		ac := lookupServer(t, status, "", 100, &n)
		if _, err := trueSpecLookup(ac)(context.Background(), []string{"abcde"}); err == nil {
			t.Fatalf("%d: expected error", status)
		}
		if n.Load() != 1 {
			t.Fatalf("%d: attempts=%d, want 1", status, n.Load())
		}
	}
	if len(*sleeps) != 0 {
		t.Fatalf("slept on a non-retryable error: %v", *sleeps)
	}
}

func TestDescribeSyncErrorDistinguishesStatuses(t *testing.T) {
	seen := map[string]bool{}
	for _, code := range []int{401, 403, 429, 503} {
		msg := describeSyncError(&agent.HTTPError{StatusCode: code})
		if !strings.Contains(msg, "TorrentClaw") || seen[msg] {
			t.Fatalf("%d: message %q not specific", code, msg)
		}
		seen[msg] = true
	}
}
