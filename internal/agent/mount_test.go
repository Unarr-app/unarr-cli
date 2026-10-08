package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountAPIContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-key" {
			t.Error("missing agent auth")
		}
		if r.URL.Query().Get("api_key") != "" {
			t.Error("credential in URL")
		}
		switch r.URL.Path {
		case "/api/internal/agent/mount/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []MountAccount{{Provider: "torbox", Revision: "rev"}}})
		case "/api/internal/agent/mount/library":
			if r.URL.Query().Get("provider") != "torbox" || r.URL.Query().Get("revision") != "rev" || r.URL.Query().Get("cursor") != "a+b" {
				t.Error(r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(MountPage{Entries: []MountEntry{{Path: "release/v", Key: "k", Size: 7, Reference: "signed"}}, Next: "next"})
		case "/api/internal/agent/mount/resolve":
			if r.Method != "POST" {
				t.Error(r.Method)
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["reference"] != "signed" {
				t.Error("bad resolve body")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://cdn.example/v"})
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "agent-key", "test")
	a, err := c.MountAccounts(context.Background())
	if err != nil || len(a) != 1 || a[0].Revision != "rev" {
		t.Fatal(a, err)
	}
	p, err := c.MountLibrary(context.Background(), "torbox", "rev", "a+b")
	if err != nil || p.Next != "next" || len(p.Entries) != 1 {
		t.Fatal(p, err)
	}
	u, err := c.MountResolve(context.Background(), "signed")
	if err != nil || u != "https://cdn.example/v" {
		t.Fatal(u, err)
	}
}
