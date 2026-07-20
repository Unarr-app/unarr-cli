package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

const (
	testDAVUser = "unarr"
	testDAVPass = "s3cr3t-webdav-pass"
	testDAVBody = "hello-media-payload"

	// testDAVLANAddr / testDAVWANAddr stand in for the two sides of the
	// local-network gate on /dav/.
	testDAVLANAddr = "192.168.1.20:5000"
	testDAVWANAddr = "8.8.8.8:5000"
)

// newWebDAVServer returns a StreamServer with a read-only WebDAV export over a
// temp dir holding one file, plus the guarded handler to drive via httptest.
func newWebDAVServer(t *testing.T) (*StreamServer, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte(testDAVBody), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	ss := NewStreamServer(0, 1)
	// webdav.Dir is writable, but the guard rejects every mutating verb before it
	// reaches the FileSystem — so this exercises the guard's read-only contract.
	ss.EnableWebDAV(webdav.Dir(dir), testDAVUser, testDAVPass)
	if !ss.WebDAVEnabled() {
		t.Fatal("WebDAVEnabled() = false after EnableWebDAV")
	}
	return ss, ss.webdavGuard(ss.webdavHandler)
}

// davRequest drives the guard as a LAN client — the case every test here that
// isn't about the network gate cares about. httptest.NewRequest's default
// RemoteAddr is 192.0.2.1 (TEST-NET-1), which is a PUBLIC address, so leaving it
// alone would make each of these assert the WAN 404 path by accident. Tests that
// do care about the source network use davRequestFromAddr.
func davRequest(t *testing.T, h http.Handler, method, target string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	return davRequestFromAddr(t, h, method, target, testDAVLANAddr, auth)
}

// davRequestFromAddr is davRequest with an explicit source address, for the
// local-network gate on /dav/ (see remoteIsLocalNetwork).
func davRequestFromAddr(t *testing.T, h http.Handler, method, target, remoteAddr string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remoteAddr
	if auth {
		req.SetBasicAuth(testDAVUser, testDAVPass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebDAVGuardRejectsMutations(t *testing.T) {
	_, h := newWebDAVServer(t)
	// Even WITH valid credentials, every mutating verb must be 405 (read-only).
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "UNLOCK", "POST"} {
		rec := davRequest(t, h, method, "/dav/movie.mkv", true)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s → %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "PROPFIND") {
			t.Errorf("%s Allow header = %q, want it to advertise read verbs", method, allow)
		}
	}
}

func TestWebDAVGuardRequiresAuth(t *testing.T) {
	_, h := newWebDAVServer(t)

	rec := davRequest(t, h, http.MethodGet, "/dav/movie.mkv", false)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-auth GET → %d, want 401", rec.Code)
	}
	if ch := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", ch)
	}

	// Wrong password → 401.
	req := httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil)
	req.RemoteAddr = testDAVLANAddr
	req.SetBasicAuth(testDAVUser, "wrong-password")
	wrong := httptest.NewRecorder()
	h.ServeHTTP(wrong, req)
	if wrong.Code != http.StatusUnauthorized {
		t.Errorf("wrong-pass GET → %d, want 401", wrong.Code)
	}

	// Wrong username → 401.
	req2 := httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil)
	req2.RemoteAddr = testDAVLANAddr
	req2.SetBasicAuth("intruder", testDAVPass)
	wrongUser := httptest.NewRecorder()
	h.ServeHTTP(wrongUser, req2)
	if wrongUser.Code != http.StatusUnauthorized {
		t.Errorf("wrong-user GET → %d, want 401", wrongUser.Code)
	}
}

func TestWebDAVReadWithAuth(t *testing.T) {
	_, h := newWebDAVServer(t)

	// GET a file → 200 with the real body + no-store caching.
	get := davRequest(t, h, http.MethodGet, "/dav/movie.mkv", true)
	if get.Code != http.StatusOK {
		t.Fatalf("authed GET → %d, want 200", get.Code)
	}
	if got := get.Body.String(); got != testDAVBody {
		t.Errorf("GET body = %q, want %q", got, testDAVBody)
	}
	if cc := get.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	// PROPFIND Depth:1 on the collection → 207 Multi-Status listing the file.
	req := httptest.NewRequest("PROPFIND", "/dav/", nil)
	req.RemoteAddr = testDAVLANAddr
	req.Header.Set("Depth", "1")
	req.SetBasicAuth(testDAVUser, testDAVPass)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND → %d, want 207", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "movie.mkv") {
		t.Errorf("PROPFIND body missing movie.mkv: %s", rec.Body.String())
	}

	// OPTIONS is allowed and advertises DAV capability.
	opt := davRequest(t, h, http.MethodOptions, "/dav/", true)
	if opt.Code >= 400 {
		t.Errorf("OPTIONS → %d, want < 400", opt.Code)
	}
	if dav := opt.Header().Get("DAV"); dav == "" {
		t.Error("OPTIONS missing DAV header")
	}
}

// TestWebDAVHeadVerb: HEAD is a permitted read verb — Infuse/Kodi/VLC probe a
// file's size and Range support with it before streaming. Authed HEAD → 200 with
// the real Content-Length but NO body; unauth HEAD → 401 like GET.
func TestWebDAVHeadVerb(t *testing.T) {
	_, h := newWebDAVServer(t)

	// Unauthenticated HEAD is rejected exactly like GET.
	if rec := davRequest(t, h, http.MethodHead, "/dav/movie.mkv", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-auth HEAD → %d, want 401", rec.Code)
	}

	// Authenticated HEAD → 200, empty body, but the size is discoverable.
	rec := davRequest(t, h, http.MethodHead, "/dav/movie.mkv", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed HEAD → %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("HEAD body = %q, want empty (HEAD carries headers only)", body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(testDAVBody)) {
		t.Errorf("HEAD Content-Length = %q, want %d (size probing must work)", cl, len(testDAVBody))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("HEAD Cache-Control = %q, want no-store", cc)
	}
}

// TestWebDAVServesOverListener proves the /dav/ subtree is actually mounted on
// the StreamServer mux and served over a real TCP socket (the one path the
// httptest-level tests can't cover): read-only + Basic auth end to end.
func TestWebDAVServesOverListener(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte(testDAVBody), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	ss := NewStreamServer(0, 1)
	ss.EnableWebDAV(webdav.Dir(dir), testDAVUser, testDAVPass)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ss.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ss.Shutdown(context.Background())

	base := fmt.Sprintf("http://127.0.0.1:%d/dav/", ss.Port())
	client := &http.Client{Timeout: 5 * time.Second}

	// do issues one request and returns (status, body); the body is closed in
	// this scope so bodyclose is satisfied.
	do := func(method, target string, auth bool) (int, string) {
		t.Helper()
		r, err := http.NewRequest(method, target, nil)
		if err != nil {
			t.Fatalf("new %s request: %v", method, err)
		}
		if auth {
			r.SetBasicAuth(testDAVUser, testDAVPass)
		}
		if method == "PROPFIND" {
			r.Header.Set("Depth", "1")
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := do(http.MethodGet, base+"movie.mkv", false); code != http.StatusUnauthorized {
		t.Errorf("no-auth GET over socket → %d, want 401", code)
	}
	if code, body := do(http.MethodGet, base+"movie.mkv", true); code != http.StatusOK || body != testDAVBody {
		t.Errorf("authed GET over socket → %d body=%q, want 200 + payload", code, body)
	}
	if code, body := do("PROPFIND", base, true); code != http.StatusMultiStatus || !strings.Contains(body, "movie.mkv") {
		t.Errorf("PROPFIND over socket → %d, want 207 listing movie.mkv", code)
	}
	if code, _ := do(http.MethodPut, base+"x.mkv", true); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT over socket → %d, want 405 (read-only)", code)
	}
}

// TestWebDAVOptionsReadOnly: OPTIONS is answered by the guard itself (not the
// stock handler, which would advertise PUT/DELETE/COPY/MOVE/LOCK) — read verbs
// only, DAV class 1, and no auth required for capability discovery.
func TestWebDAVOptionsReadOnly(t *testing.T) {
	_, h := newWebDAVServer(t)

	// No auth: OPTIONS is discovery, must still answer 200.
	rec := davRequest(t, h, http.MethodOptions, "/dav/", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS (no auth) → %d, want 200", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	for _, verb := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "PROPPATCH"} {
		if strings.Contains(allow, verb) {
			t.Errorf("OPTIONS Allow = %q, must NOT advertise mutating verb %s", allow, verb)
		}
	}
	for _, verb := range []string{"GET", "HEAD", "PROPFIND", "OPTIONS"} {
		if !strings.Contains(allow, verb) {
			t.Errorf("OPTIONS Allow = %q, must advertise read verb %s", allow, verb)
		}
	}
	if dav := rec.Header().Get("DAV"); dav != "1" {
		t.Errorf("DAV header = %q, want \"1\" (class 1 only — no LOCK)", dav)
	}
}

func TestResolveWebDAVCreds(t *testing.T) {
	// Explicit user + password → used verbatim, active.
	if u, p, active := ResolveWebDAVCreds("bob", "pw", "tc_key"); u != "bob" || p != "pw" || !active {
		t.Errorf("explicit creds = (%q,%q,%v), want (bob,pw,true)", u, p, active)
	}
	// Defaults: empty user → "unarr"; empty password → derived from the API key.
	if u, p, active := ResolveWebDAVCreds("", "", "tc_key"); u != "unarr" || p != DeriveWebDAVPassword("tc_key") || !active {
		t.Errorf("derived creds = (%q,%q,%v), want (unarr, <derived>, true)", u, p, active)
	}
	// No password and no API key → not active (daemon won't arm, status won't print).
	if _, p, active := ResolveWebDAVCreds("", "", ""); p != "" || active {
		t.Errorf("no-key creds = (%q, active=%v), want empty + inactive", p, active)
	}
}

func TestDeriveWebDAVPassword(t *testing.T) {
	const key = "tc_03b958e0f6ad1917639a7f1ab34d1c8e"
	p1 := DeriveWebDAVPassword(key)
	p2 := DeriveWebDAVPassword(key)
	if p1 == "" || p1 != p2 {
		t.Fatalf("derivation not stable: p1=%q p2=%q", p1, p2)
	}
	if len(p1) != webdavPassLen {
		t.Errorf("password len = %d, want %d", len(p1), webdavPassLen)
	}
	if DeriveWebDAVPassword("other-key") == p1 {
		t.Error("different API keys produced the same password")
	}
	if DeriveWebDAVPassword("") != "" {
		t.Error("empty API key should derive an empty password")
	}
	// Must NOT be derived from the per-restart stream secret: two servers (with
	// distinct stream secrets) yield the SAME password for the same API key.
	ss1 := NewStreamServer(0, 1)
	ss2 := NewStreamServer(0, 1)
	if ss1.StreamSecretHex() == ss2.StreamSecretHex() {
		t.Fatal("stream secrets unexpectedly equal — cannot prove independence")
	}
	if DeriveWebDAVPassword(key) != p1 {
		t.Error("password derivation depends on daemon state; must depend only on the API key")
	}
}
