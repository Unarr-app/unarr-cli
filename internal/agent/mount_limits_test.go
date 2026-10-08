package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMountLibraryResponseSizeBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(map[int]string{0: "exact limit", 1: "limit plus one"}[extra], func(t *testing.T) {
			body := `{"entries":[],"next":""}`
			body += strings.Repeat(" ", (1<<20)+extra-len(body))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "synthetic", "test").MountLibrary(context.Background(), "torbox", "rev", "")
			if extra == 0 && err != nil {
				t.Fatal("exact limit rejected", err)
			}
			if extra == 1 && (err == nil || !strings.Contains(err.Error(), "1048576")) {
				t.Fatalf("oversized response needs an explicit bounded-size error: %v", err)
			}
		})
	}
}

func TestMountDenialsNeverFailOver(t *testing.T) {
	for _, status := range []int{403, 410} {
		for _, oversized := range []bool{false, true} {
			t.Run(http.StatusText(status)+map[bool]string{false: " bounded", true: " oversized"}[oversized], func(t *testing.T) {
				var mirrorCalls atomic.Int32
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					body := `{"error":"paid_plan_required"}`
					if oversized {
						body = strings.Repeat("sensitive-body-canary", 60000)
					}
					_, _ = io.WriteString(w, body)
				}))
				defer primary.Close()
				mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mirrorCalls.Add(1)
					_, _ = io.WriteString(w, `{"allowed":true}`)
				}))
				defer mirror.Close()
				err := NewClientWithMirrors(primary.URL, []string{mirror.URL}, "synthetic", "test").MountAccess(context.Background())
				var denial *HTTPError
				if !errors.As(err, &denial) || denial.StatusCode != status || mirrorCalls.Load() != 0 {
					t.Fatal("denial bypassed", err, mirrorCalls.Load())
				}
				if strings.Contains(err.Error(), "sensitive-body-canary") {
					t.Fatal("oversized error leaked response body")
				}
			})
		}
	}
}

func TestMountAccessHungPrimaryRetainsMirrorBudget(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer primary.Close()
	var calls atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("failover lost authentication")
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	}))
	defer healthy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := NewClientWithMirrors(primary.URL, []string{healthy.URL}, "synthetic", "test").MountAccess(ctx)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("hung primary consumed the watchdog budget: mirror calls=%d, error=%v", calls.Load(), err)
	}
}
