package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

type fakeSessions struct{ snap metrics.Snapshot }

func (f fakeSessions) Poll(time.Time) metrics.Snapshot { return f.snap }

func TestServer_Sessions(t *testing.T) {
	speed := 1.2
	src := fakeSessions{snap: metrics.Snapshot{
		SchemaVersion: metrics.SchemaVersion,
		Sessions: []metrics.Session{
			{ID: "hls-1", Kind: metrics.KindHLS, Title: "Movie", State: "active", Health: metrics.HealthOK, SpeedX: &speed},
		},
		Summary: metrics.Summary{Stalled: 0},
	}}
	srv, client := startTestServer(t, &fakeController{})
	srv.SetSessions(src)

	snap, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if snap.SchemaVersion != metrics.SchemaVersion || len(snap.Sessions) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if s := snap.Sessions[0]; s.ID != "hls-1" || s.SpeedX == nil || *s.SpeedX != 1.2 {
		t.Fatalf("session = %+v", s)
	}
}

// Without a wired source the endpoint says "not supported", which the client
// reports as an old daemon — not as an empty list.
func TestServer_SessionsUnwired(t *testing.T) {
	_, client := startTestServer(t, &fakeController{})
	_, err := client.Sessions(context.Background())
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v, want ErrNoDaemon", err)
	}
}

func TestServer_SessionsAuthAndMethod(t *testing.T) {
	srv, _ := startTestServer(t, &fakeController{})
	srv.SetSessions(fakeSessions{})
	url := "http://127.0.0.1:" + strconv.Itoa(srv.Port()) + "/sessions"

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set(TokenHeader, srv.Token())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", resp.StatusCode)
	}
}

// /tasks keeps its contract when sessions are wired.
func TestServer_TasksUnchangedWithSessions(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{{ID: "t1", Title: "X", State: "downloading", Running: true}}}
	srv, client := startTestServer(t, ctrl)
	srv.SetSessions(fakeSessions{})
	tasks, err := client.List(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("List = %+v, %v", tasks, err)
	}
}
