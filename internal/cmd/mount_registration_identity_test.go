package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestMountHeldRegistrationCannotOverwriteNewSignIn(t *testing.T) {
	isolatedMountConfig(t)
	creds, file := storeAt(t, "K1", "ID1")
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agent.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer K1" || req.AgentID != "ID1" {
			t.Error("registration identity pair changed")
		}
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"agentKey":"late-K1-minted","user":{"name":"synthetic"}}`))
	}))
	defer srv.Close()
	d := agent.NewDaemon(agent.DaemonConfig{AgentID: "ID1", DownloadDir: t.TempDir()}, agent.NewClient(srv.URL, "K1", "test"))
	wireDaemonCredentials(d, creds)
	done := make(chan error, 1)
	go func() { done <- d.RegisterBestEffort(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("registration did not start")
	}
	next := config.Default()
	next.Auth.APIKey, next.Agent.ID = "K2", "ID2"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload() // actual background callback: never mutates daemon config
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registration did not settle")
	}
	after, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if after.Auth.APIKey != "K2" || after.Agent.ID != "ID2" {
		t.Fatal("held K1 reply overwrote K2 sign-in")
	}
}

func TestMountStaleMintAndRejectionBeforePollPreserveNewSignIn(t *testing.T) {
	for _, event := range []string{"mint", "revoke"} {
		t.Run(event, func(t *testing.T) {
			isolatedMountConfig(t)
			creds, file := storeAt(t, "K1", "ID1")
			d := agent.NewDaemon(agent.DaemonConfig{AgentID: "ID1"}, agent.NewClient("http://127.0.0.1:1", "K1", "test"))
			wireDaemonCredentials(d, creds)
			next := config.Default()
			next.Auth.APIKey, next.Agent.ID = "K2", "ID2"
			if err := config.Save(next, file); err != nil {
				t.Fatal(err)
			}
			if event == "mint" {
				if d.OnAgentKeyMintedForIdentity("K1", "ID1", "late-K1-minted") {
					t.Fatal("stale mint accepted")
				}
			} else {
				d.SyncClient().OnRevokedForIdentity(&agent.HTTPError{StatusCode: 410}, "K1", "ID1")
				if d.OnCredentialRejectedForIdentity("K1", "ID1") {
					t.Fatal("stale register revocation accepted")
				}
			}
			after, err := config.Load(file)
			if err != nil || after.Auth.APIKey != "K2" || after.Agent.ID != "ID2" {
				t.Fatal("stale reply changed newer login", err)
			}
		})
	}
}

func TestMountDaemonOwnerHandoffKeepsRegisterAndSyncIdentityPaired(t *testing.T) {
	for _, polled := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked reply before poll", true: "cancel held sync after poll"}[polled], func(t *testing.T) { verifyMountDaemonHandoff(t, polled) })
	}
}

func verifyMountDaemonHandoff(t *testing.T, polled bool) {
	t.Helper()
	isolatedMountConfig(t)
	creds, file := storeAt(t, "K1", "ID1")
	oldSync, release, newSync := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var oldOnce, newOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/agent/wake" {
			<-r.Context().Done()
			return
		}
		var req struct {
			AgentID string `json:"agentId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		key := r.Header.Get("Authorization")
		if (key == "Bearer K1" && req.AgentID != "ID1") || (key == "Bearer K2" && req.AgentID != "ID2") {
			t.Error("key/ID mismatch", key, req.AgentID)
		}
		if r.URL.Path == "/api/internal/agent/sync" && key == "Bearer K1" {
			oldOnce.Do(func() { close(oldSync) })
			<-release
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":"agent_revoked"}`))
			return
		}
		if r.URL.Path == "/api/internal/agent/sync" && key == "Bearer K2" {
			newOnce.Do(func() { close(newSync) })
		}
		_, _ = w.Write([]byte(`{"success":true,"user":{"name":"synthetic"},"tasks":[]}`))
	}))
	defer srv.Close()
	d := agent.NewDaemon(agent.DaemonConfig{AgentID: "ID1", DownloadDir: t.TempDir(), Downlink: "poll"}, agent.NewClient(srv.URL, "K1", "test"))
	wireDaemonCredentials(d, creds)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-oldSync:
	case <-time.After(2 * time.Second):
		t.Fatal("old identity did not sync")
	}
	next := config.Default()
	next.Auth.APIKey, next.Agent.ID = "K2", "ID2"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	if polled {
		creds.reload()
	}
	close(release)
	select {
	case <-newSync:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon owner did not hand off to K2")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old/new agent cycles were not joined")
	}
	after, err := config.Load(file)
	if err != nil || after.Auth.APIKey != "K2" || after.Agent.ID != "ID2" {
		t.Fatal("old sync revocation wiped K2", err)
	}
}

func TestMountDaemonRemovalAndBootstrapRecoverWithoutProcessRestart(t *testing.T) {
	isolatedMountConfig(t)
	creds, file := storeAt(t, "K1", "ID1")
	entered, release, empty, newSync := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var oldOnce, emptyOnce, newOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/agent/wake" {
			<-r.Context().Done()
			return
		}
		key := r.Header.Get("Authorization")
		if r.URL.Path == "/api/internal/agent/register" && key == "Bearer K1" {
			oldOnce.Do(func() { close(entered) })
			<-release
			_, _ = w.Write([]byte(`{"agentKey":"stale-K1-minted"}`))
			return
		}
		if r.URL.Path == "/api/internal/agent/register" && key == "Bearer" {
			emptyOnce.Do(func() { close(empty) })
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if r.URL.Path == "/api/internal/agent/register" && key == "Bearer K2" {
			_, _ = w.Write([]byte(`{"agentKey":"minted-K2","user":{"name":"synthetic"}}`))
			return
		}
		if r.URL.Path == "/api/internal/agent/sync" {
			var req agent.SyncRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if key != "Bearer minted-K2" || req.AgentID != "ID2" {
				t.Error("bootstrap sync identity mismatch", key, req.AgentID)
			}
			newOnce.Do(func() { close(newSync) })
		}
		_, _ = w.Write([]byte(`{"success":true,"user":{"name":"synthetic"},"tasks":[]}`))
	}))
	defer srv.Close()
	d := agent.NewDaemon(agent.DaemonConfig{AgentID: "ID1", DownloadDir: t.TempDir(), Downlink: "poll"}, agent.NewClient(srv.URL, "K1", "test"))
	wireDaemonCredentials(d, creds)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial registration not held")
	}
	next := config.Default()
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload()
	close(release)
	select {
	case <-empty:
	case <-time.After(time.Second):
		t.Fatal("removal did not cancel old registration")
	}
	next.Auth.APIKey, next.Agent.ID = "K2", "ID2"
	if err := config.Save(next, file); err != nil {
		t.Fatal(err)
	}
	creds.reload()
	select {
	case <-newSync:
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrap did not recover the same daemon process")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not join bootstrap cycle")
	}
	after, err := config.Load(file)
	if err != nil || after.Auth.APIKey != "minted-K2" || after.Agent.ID != "ID2" {
		t.Fatal("bootstrap identity was not persisted", err)
	}
}
