package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func startTestServer(t *testing.T, token string) (*Server, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Listen(ctx, "127.0.0.1:0", token, NewHub("1.0.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + srv.Addr() + "/metrics"
}

func get(t *testing.T, url string, hdr map[string]string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestMetricsEndpointAuthAndFormat(t *testing.T) {
	_, url := startTestServer(t, "s3cret")

	if code, _, _ := get(t, url, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _, _ := get(t, url, map[string]string{"Authorization": "Bearer nope"}); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", code)
	}

	for name, hdr := range map[string]map[string]string{
		"bearer": {"Authorization": "Bearer s3cret"},
		"header": {TokenHeader: "s3cret"},
	} {
		code, ctype, body := get(t, url, hdr)
		if code != http.StatusOK {
			t.Fatalf("%s: %d, want 200", name, code)
		}
		if ctype != ContentType {
			t.Fatalf("%s: content-type %q", name, ctype)
		}
		samples := parseProm(t, body)
		if v, ok := find(samples, "unarr_info"); !ok || v.labels["version"] != "1.0.0" {
			t.Fatalf("%s: unarr_info missing/wrong in %q", name, body)
		}
	}
}

func TestMetricsRejectsOtherPathsAndMethods(t *testing.T) {
	_, url := startTestServer(t, "tok")
	if code, _, _ := get(t, strings.TrimSuffix(url, "/metrics")+"/sessions", map[string]string{TokenHeader: "tok"}); code != http.StatusNotFound {
		t.Fatalf("/sessions on the metrics port: %d, want 404", code)
	}
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set(TokenHeader, "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", resp.StatusCode)
	}
}

func TestListenRequiresToken(t *testing.T) {
	if _, err := Listen(context.Background(), "127.0.0.1:0", "", NewHub("v", nil)); err == nil {
		t.Fatal("an unauthenticated endpoint must not start")
	}
}

func TestNilServerIsInert(t *testing.T) {
	var s *Server
	if s.Addr() != "" || s.Close() != nil {
		t.Fatal("nil server must be a no-op")
	}
}
