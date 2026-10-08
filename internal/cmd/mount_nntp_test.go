package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestMountNNTPUsesWebCredentialsLazily(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/mount/usenet-credentials" || r.Header.Get("Authorization") != "Bearer device-key" {
			t.Error("wrong web credential request")
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(agent.UsenetCredentials{Host: "news.example", Port: 563, SSL: true, Username: "web-user", Password: "web-secret", MaxConnections: 8})
	}))
	defer srv.Close()
	n := &mountNNTP{api: agent.NewClient(srv.URL, "device-key", "test")}
	if n.MaxConcurrency() != 1 || calls.Load() != 0 {
		t.Fatal("eager credential fetch")
	}
	first, err := n.connection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n.MaxConcurrency() != 8 || calls.Load() != 1 {
		t.Fatal("pool size or fetch count")
	}
	second, err := n.connection(context.Background())
	if err != nil || second != first || calls.Load() != 1 {
		t.Fatal("did not reuse warm credentials")
	}
	n.refreshed = time.Now().Add(-6 * time.Minute)
	second, err = n.connection(context.Background())
	if err != nil || second != first || calls.Load() != 2 {
		t.Fatal("did not refresh credentials")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if n.credentials.Password != "" {
		t.Fatal("retained credentials on close")
	}
	if _, err := n.connection(context.Background()); err == nil {
		t.Fatal("reopened closed source")
	}
}
