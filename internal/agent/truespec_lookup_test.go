package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrueSpecLookupRefusesAnythingButFiveHex(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "key", "test")
	for _, bad := range []string{
		strings.Repeat("a", 40), // a full infohash must never leave the machine
		"abcd", "abcdef", "abcdg", "",
	} {
		if _, err := c.TrueSpecLookup(context.Background(), []string{bad}); err == nil ||
			!strings.Contains(err.Error(), "5-hex") {
			t.Errorf("%q: err = %v, want 5-hex refusal", bad, err)
		}
	}
}

func TestTrueSpecLookupRoundTrip(t *testing.T) {
	var got trueSpecLookupRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/truespec/lookup" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[{"infohash":"25ceb898` + strings.Repeat("a", 32) + `","verified":true,"mismatch":false}]}`))
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "key", "test").TrueSpecLookup(context.Background(), []string{"25ceb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Prefixes) != 1 || got.Prefixes[0] != "25ceb" || len(res) != 1 || !res[0].Verified {
		t.Fatalf("sent %v, got %+v", got.Prefixes, res)
	}
}
