package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestMountAccessIncompleteDenialsNeverFailOver(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 409, 410, 422, 429, 500} {
		for _, mode := range []string{"stalled", "truncated", "oversized"} {
			t.Run(fmt.Sprintf("%d/%s", status, mode), func(t *testing.T) {
				// Even a valid JSON prefix must not be trusted when its HTTP body
				// never completes. In particular, it must not revoke an ordinary 403.
				const body = `{"error":"agent_key_mismatch","message":"sensitive-denial-canary"}`
				var primaryCalls, mirrorCalls atomic.Int32
				release := make(chan struct{})
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					if r.URL.Path != "/api/internal/agent/mount/access" || r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("public mount access request lost its route or credential")
					}
					if mode == "oversized" {
						w.WriteHeader(status)
						_, _ = io.WriteString(w, body+strings.Repeat(" ", (1<<20)+1-len(body)))
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
					w.(http.Flusher).Flush()
					if mode == "stalled" {
						select {
						case <-r.Context().Done():
						case <-release:
						}
					}
				}))
				defer primary.Close()
				defer close(release)
				mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mirrorCalls.Add(1)
					_, _ = io.WriteString(w, `{"allowed":true}`)
				}))
				defer mirror.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				start := time.Now()
				err := agent.NewClientWithMirrors(primary.URL, []string{mirror.URL}, "synthetic", "test").MountAccess(ctx)
				t.Logf("status=%d mode=%s elapsed=%s primary=%d mirror=%d error=%v", status, mode, time.Since(start), primaryCalls.Load(), mirrorCalls.Load(), err)
				var denial *agent.HTTPError
				if !errors.As(err, &denial) || denial.StatusCode != status {
					t.Fatalf("received denial was lost: %v", err)
				}
				if primaryCalls.Load() != 1 || mirrorCalls.Load() != 0 || ctx.Err() != nil {
					t.Fatalf("terminal denial retried or exhausted watchdog: primary=%d mirror=%d context=%v", primaryCalls.Load(), mirrorCalls.Load(), ctx.Err())
				}
				if denial.Detail != "" || strings.Contains(err.Error(), "sensitive-denial-canary") || strings.Contains(denial.Message, "agent_key_mismatch") {
					t.Fatal("incomplete denial body was exposed or interpreted", err)
				}
				if agent.IsRevoked(err) != (status == http.StatusGone) {
					t.Fatal("incomplete body changed status-based revocation semantics", err)
				}
			})
		}
	}
}

func TestMountAccessCompleteDenialSemantics(t *testing.T) {
	for _, tc := range []struct {
		status  int
		code    string
		revoked bool
	}{
		{403, "paid_plan_required", false},
		{403, "agent_key_mismatch", true},
		{410, "agent_revoked", true},
	} {
		for _, exactLimit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exactLimit=%t", tc.code, exactLimit), func(t *testing.T) {
				const detail = "complete denial detail"
				body := fmt.Sprintf(`{"error":%q,"message":%q}`, tc.code, detail)
				if exactLimit {
					body += strings.Repeat(" ", (1<<20)-len(body))
				}
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, body)
				}))
				defer primary.Close()
				var mirrorCalls atomic.Int32
				mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mirrorCalls.Add(1)
					_, _ = io.WriteString(w, `{"allowed":true}`)
				}))
				defer mirror.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				err := agent.NewClientWithMirrors(primary.URL, []string{mirror.URL}, "synthetic", "test").MountAccess(ctx)
				var denial *agent.HTTPError
				if !errors.As(err, &denial) || denial.StatusCode != tc.status || denial.Message != tc.code || denial.Detail != detail {
					t.Fatal("complete JSON denial changed", err)
				}
				if agent.IsRevoked(err) != tc.revoked || mirrorCalls.Load() != 0 {
					t.Fatal("complete denial changed revocation or failover", err, mirrorCalls.Load())
				}
			})
		}
	}
}

func TestMountAccessStalledNonterminalResponseFailsOver(t *testing.T) {
	for _, status := range []int{200, 408, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var primaryCalls, mirrorCalls atomic.Int32
			release := make(chan struct{})
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer primary.Close()
			defer close(release)
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mirrorCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("failover lost credential")
				}
				_, _ = io.WriteString(w, `{"allowed":true}`)
			}))
			defer mirror.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			start := time.Now()
			err := agent.NewClientWithMirrors(primary.URL, []string{mirror.URL}, "synthetic", "test").MountAccess(ctx)
			t.Logf("status=%d elapsed=%s primary=%d mirror=%d error=%v", status, time.Since(start), primaryCalls.Load(), mirrorCalls.Load(), err)
			if err != nil || primaryCalls.Load() != 1 || mirrorCalls.Load() != 1 || ctx.Err() != nil {
				t.Fatal("transient response exhausted mirror budget", err)
			}
		})
	}
}

func TestMountAccessCallerCancelsBodyRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var primaryCalls, mirrorCalls atomic.Int32
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer primary.Close()
	defer close(release)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mirrorCalls.Add(1)
		_, _ = io.WriteString(w, `{"allowed":true}`)
	}))
	defer mirror.Close()
	err := agent.NewClientWithMirrors(primary.URL, []string{mirror.URL}, "synthetic", "test").MountAccess(ctx)
	if !errors.Is(err, context.Canceled) || primaryCalls.Load() != 1 || mirrorCalls.Load() != 0 {
		t.Fatalf("caller cancellation changed: primary=%d mirror=%d error=%v", primaryCalls.Load(), mirrorCalls.Load(), err)
	}
}
