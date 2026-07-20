package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReachableAcceptsAnyReply(t *testing.T) {
	// The probe asks whether a server is there at all, not whether it likes the
	// request — a 404 from a reachable host must not block sign-in.
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		if err := reachable(srv.URL); err != nil {
			t.Errorf("reachable() on a server returning %d = %v, want nil", status, err)
		}
		srv.Close()
	}
}

func TestReachableFailsFastOnADeadServer(t *testing.T) {
	// The failure this exists for: an api_url pointing at a dev server that is
	// not running. Opening a browser at it shows an error page and then makes
	// the user wait out the whole authorization timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening now

	err := reachable(dead)

	if err == nil {
		t.Fatal("reachable() = nil for a server that is not listening")
	}
	if !strings.Contains(err.Error(), dead) {
		t.Errorf("error = %q, want it to name the URL that could not be reached", err)
	}
	if !strings.Contains(err.Error(), "api_url") {
		t.Errorf("error = %q, want it to point at the setting the user can fix", err)
	}
}

func TestReachableTolerAtesATrailingSlash(t *testing.T) {
	// api_url is user-edited config, so it may or may not end in a slash; a
	// doubled slash must not turn a healthy server into an unreachable one.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "//") {
			http.Error(nil, "doubled slash", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	if err := reachable(srv.URL + "/"); err != nil {
		t.Errorf("reachable() with a trailing slash = %v, want nil", err)
	}
}
