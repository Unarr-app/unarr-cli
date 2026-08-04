package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeController records what the server asked it to do.
type fakeController struct {
	mu      sync.Mutex
	tasks   []TaskInfo
	calls   []string
	purged  int
	lastDel bool
}

func (f *fakeController) List() []TaskInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TaskInfo(nil), f.tasks...)
}

func (f *fakeController) record(action, id string) ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action+":"+id)
	return ActionResult{TaskID: id, Applied: true, Message: action + "ed"}
}

func (f *fakeController) Pause(id string) ActionResult  { return f.record("paus", id) }
func (f *fakeController) Resume(id string) ActionResult { return f.record("resum", id) }
func (f *fakeController) Retry(id string) ActionResult  { return f.record("retri", id) }

func (f *fakeController) Cancel(id string, deleteFiles bool) ActionResult {
	f.mu.Lock()
	f.lastDel = deleteFiles
	f.mu.Unlock()
	return f.record("cancel", id)
}

func (f *fakeController) Purge() []ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged++
	return []ActionResult{{TaskID: "gone", Applied: true, Message: "dropped"}}
}

func (f *fakeController) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func startTestServer(t *testing.T, ctrl Controller) (*Server, *Client) {
	t.Helper()
	srv, err := NewServer(ctrl)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, NewClient(srv.Port(), srv.Token())
}

func TestServer_ListAndAction(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{
		{ID: "11111111-aaaa", Title: "Movie", State: "downloading", Running: true},
	}}
	_, client := startTestServer(t, ctrl)

	tasks, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "Movie" {
		t.Fatalf("List returned %+v", tasks)
	}

	if _, err := client.Do(context.Background(), ActionPause, ActionRequest{TaskID: "11111111-aaaa"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := ctrl.recorded(); len(got) != 1 || got[0] != "paus:11111111-aaaa" {
		t.Fatalf("controller calls = %v", got)
	}
}

// The ids users see are 8-char prefixes; asking them to retype a UUID to stop a
// runaway download is hostile.
func TestServer_ResolvesShortPrefix(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{
		{ID: "31ec4169-1111-2222-3333-444455556666", Title: "Big"},
	}}
	_, client := startTestServer(t, ctrl)

	if _, err := client.Do(context.Background(), ActionCancel, ActionRequest{TaskID: "31ec4169"}); err != nil {
		t.Fatalf("cancel by prefix: %v", err)
	}
	got := ctrl.recorded()
	if len(got) != 1 || !strings.HasSuffix(got[0], "31ec4169-1111-2222-3333-444455556666") {
		t.Fatalf("prefix did not resolve to the full id: %v", got)
	}
}

// Two matches must be an error, never a guess: picking one of two downloads to
// kill is the worst possible resolution.
func TestServer_AmbiguousPrefixRefuses(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{
		{ID: "31ec4169-aaaa"},
		{ID: "31ec4169-bbbb"},
	}}
	_, client := startTestServer(t, ctrl)

	_, err := client.Do(context.Background(), ActionCancel, ActionRequest{TaskID: "31ec4169"})
	if err == nil {
		t.Fatal("an ambiguous prefix was accepted")
	}
	if !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if got := ctrl.recorded(); len(got) != 0 {
		t.Fatalf("controller was called despite the ambiguity: %v", got)
	}
}

// An exact id must win even when it is also a prefix of another id.
func TestServer_ExactIDBeatsPrefix(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{
		{ID: "31ec4169"},
		{ID: "31ec4169-longer"},
	}}
	_, client := startTestServer(t, ctrl)

	if _, err := client.Do(context.Background(), ActionCancel, ActionRequest{TaskID: "31ec4169"}); err != nil {
		t.Fatalf("exact id rejected: %v", err)
	}
	if got := ctrl.recorded(); len(got) != 1 || got[0] != "cancel:31ec4169" {
		t.Fatalf("exact match did not win: %v", got)
	}
}

func TestServer_AllTargetsEveryTask(t *testing.T) {
	ctrl := &fakeController{tasks: []TaskInfo{{ID: "aaaaaaaa"}, {ID: "bbbbbbbb"}}}
	_, client := startTestServer(t, ctrl)

	results, err := client.Do(context.Background(), ActionCancel, ActionRequest{All: true, DeleteFiles: true})
	if err != nil {
		t.Fatalf("cancel --all: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if !ctrl.lastDel {
		t.Error("deleteFiles was not propagated")
	}
}

func TestServer_UnknownTaskIsNotFound(t *testing.T) {
	_, client := startTestServer(t, &fakeController{})
	_, err := client.Do(context.Background(), ActionCancel, ActionRequest{TaskID: "nope"})
	if err == nil || !strings.Contains(err.Error(), "no download matches") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestServer_PurgeNeedsNoTarget(t *testing.T) {
	ctrl := &fakeController{}
	_, client := startTestServer(t, ctrl)

	results, err := client.Do(context.Background(), ActionPurge, ActionRequest{})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(results) != 1 || ctrl.purged != 1 {
		t.Fatalf("purge did not reach the controller (results=%d purged=%d)", len(results), ctrl.purged)
	}
}

// The control plane can stop downloads and delete files. A caller without the
// token must get nowhere — this is the whole reason it is not part of the
// LAN-facing stream server.
func TestServer_RejectsBadToken(t *testing.T) {
	srv, _ := startTestServer(t, &fakeController{tasks: []TaskInfo{{ID: "aaaaaaaa"}}})

	bad := NewClient(srv.Port(), "not-the-token")
	if _, err := bad.List(context.Background()); err == nil {
		t.Fatal("List succeeded without the right token")
	}
	if _, err := bad.Do(context.Background(), ActionCancel, ActionRequest{All: true}); err == nil {
		t.Fatal("Cancel succeeded without the right token")
	}
}

func TestServer_RejectsUnknownAction(t *testing.T) {
	_, client := startTestServer(t, &fakeController{tasks: []TaskInfo{{ID: "aaaaaaaa"}}})
	_, err := client.Do(context.Background(), "detonate", ActionRequest{TaskID: "aaaaaaaa"})
	if err == nil || !strings.Contains(err.Error(), "unsupported action") {
		t.Fatalf("expected an unsupported-action error, got %v", err)
	}
}

func TestServer_TokenIsRandomPerServer(t *testing.T) {
	a, err := NewServer(&fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServer(&fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Token() == b.Token() {
		t.Fatal("two servers minted the same control token")
	}
	if len(a.Token()) < 32 {
		t.Fatalf("control token is too short to be a secret: %d chars", len(a.Token()))
	}
}

func TestServer_BindsLoopbackOnly(t *testing.T) {
	srv, _ := startTestServer(t, &fakeController{})
	addr := srv.ln.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("control plane bound %s — it must never leave loopback", addr)
	}
}

// A GET on the action path (or a POST on the list path) must not be silently
// treated as the other.
func TestServer_MethodGuards(t *testing.T) {
	srv, _ := startTestServer(t, &fakeController{})
	base := "http://127.0.0.1:" + itoa(srv.Port())

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/tasks/cancel", nil)
	req.Header.Set(TokenHeader, srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /tasks/cancel = %d, want 405", resp.StatusCode)
	}
}

func TestServer_RejectsInvalidJSON(t *testing.T) {
	srv, _ := startTestServer(t, &fakeController{})
	base := "http://127.0.0.1:" + itoa(srv.Port())

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/tasks/cancel",
		strings.NewReader("{not json"))
	req.Header.Set(TokenHeader, srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", resp.StatusCode)
	}
	var body ActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	if body.Error == "" {
		t.Fatal("error response carried no message")
	}
}

// itoa avoids pulling strconv into the test file for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
