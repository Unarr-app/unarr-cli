package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// sessionsWeb is a fake web that records POST /api/internal/agent/sessions.
type sessionsWeb struct {
	mu     sync.Mutex
	bodies []map[string]json.RawMessage
	got    chan struct{}
}

func newSessionsWeb(t *testing.T) (*sessionsWeb, *agent.Client) {
	t.Helper()
	w := &sessionsWeb{got: make(chan struct{}, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/sessions" {
			http.NotFound(rw, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		w.mu.Lock()
		w.bodies = append(w.bodies, body)
		w.mu.Unlock()
		_, _ = rw.Write([]byte(`{"ok":true}`))
		select {
		case w.got <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	return w, agent.NewClient(srv.URL, "k", "unarr-test")
}

func (w *sessionsWeb) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.bodies)
}

// Publishing does not depend on the Prometheus switch: with [metrics] enabled
// = false the daemon still sends the snapshot, wrapped as the contract says.
func TestSessionPublisherRunsWithPrometheusOff(t *testing.T) {
	web, client := newSessionsWeb(t)
	poster := webSessionPoster{client: client, agentID: func() string { return "agent-1" }}
	stop := startSessionPublisher(context.Background(), metrics.NewHub("1.2.3", nil),
		config.MetricsConfig{Enabled: false, Publish: true}, poster)
	defer stop()

	select {
	case <-web.got:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was published with publish = true and Prometheus off")
	}
	web.mu.Lock()
	body := web.bodies[0]
	web.mu.Unlock()
	var id string
	var snap metrics.Snapshot
	if err := json.Unmarshal(body["agentId"], &id); err != nil || id != "agent-1" {
		t.Fatalf("agentId = %s (%v)", body["agentId"], err)
	}
	if err := json.Unmarshal(body["snapshot"], &snap); err != nil || snap.SchemaVersion != metrics.SchemaVersion || snap.Agent.Version != "1.2.3" {
		t.Fatalf("snapshot = %s (%v)", body["snapshot"], err)
	}
}

// publish = false: no goroutine, no request, ever.
func TestSessionPublisherOffSendsNothing(t *testing.T) {
	web, client := newSessionsWeb(t)
	poster := webSessionPoster{client: client, agentID: func() string { return "agent-1" }}
	stop := startSessionPublisher(context.Background(), metrics.NewHub("v", nil),
		config.MetricsConfig{Enabled: true, Publish: false}, poster)
	time.Sleep(100 * time.Millisecond)
	stop()
	if n := web.count(); n != 0 {
		t.Fatalf("%d snapshots sent with publish = false", n)
	}
}

// Without an identity there is nobody to publish for, and nothing goes out.
func TestWebSessionPosterNeedsAnAgentID(t *testing.T) {
	web, client := newSessionsWeb(t)
	err := webSessionPoster{client: client, agentID: func() string { return "" }}.PostSessions(context.Background(), []byte(`{}`))
	if err == nil || web.count() != 0 {
		t.Fatalf("err=%v posts=%d, want an error and no request", err, web.count())
	}
}

// Only the APP's own 404/405 means "old web": the edge's 404 during a deploy is
// a blip, not a reason to go quiet for 10 minutes.
func TestClassifyPublishError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"app 404", &agent.HTTPError{StatusCode: 404}, metrics.ErrPublishUnsupported},
		{"app 405", &agent.HTTPError{StatusCode: 405}, metrics.ErrPublishUnsupported},
		{"edge 404", &agent.HTTPError{StatusCode: 404, FromGateway: true}, nil},
		{"400", &agent.HTTPError{StatusCode: 400}, metrics.ErrPublishRejected},
		{"413", &agent.HTTPError{StatusCode: 413}, metrics.ErrPublishRejected},
		{"401", &agent.HTTPError{StatusCode: 401}, nil},
		{"403", &agent.HTTPError{StatusCode: 403}, nil},
		{"503", &agent.HTTPError{StatusCode: 503}, nil},
		{"network", errors.New("connection refused"), nil},
	}
	for _, c := range cases {
		got := classifyPublishError(c.err)
		for _, sentinel := range []error{metrics.ErrPublishUnsupported, metrics.ErrPublishRejected} {
			if errors.Is(got, sentinel) != (c.want == sentinel) {
				t.Errorf("%s: %v classified wrong (want %v)", c.name, got, c.want)
			}
		}
		var he *agent.HTTPError
		if _, isHTTP := c.err.(*agent.HTTPError); isHTTP && !errors.As(got, &he) {
			t.Errorf("%s: the HTTPError must stay reachable for the auth paths", c.name)
		}
	}
	if classifyPublishError(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// The publisher starts once with the daemon; flipping publish on reload needs
// a restart, and the reload says so.
func TestReportMetricsReloadPublish(t *testing.T) {
	var sink bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(prev) })

	reportMetricsReload(config.MetricsConfig{Publish: true}, config.MetricsConfig{Publish: false})
	if !strings.Contains(sink.String(), "RESTART REQUIRED") {
		t.Fatalf("publish change logged %q", sink.String())
	}
}
