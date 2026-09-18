package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultBuildDoesNotContactUnverifiedEndpoint(t *testing.T) {
	if privateDeliveryEnabled != "false" {
		t.Skip("operator-enabled release build")
	}
	// An invalid transport would panic if Send attempted to construct its
	// client. A disabled build must return before even preparing a request.
	original := http.DefaultTransport
	http.DefaultTransport = nil
	t.Cleanup(func() { http.DefaultTransport = original })
	if Send(context.Background(), []byte(`{}`)) != ErrUnavailable {
		t.Fatal("unverified endpoint was enabled")
	}
}

func TestProductionTransportRejectsRedirectsAndEnvironmentProxy(t *testing.T) {
	var forwarded int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv("HTTPS_PROXY", target.URL)
	t.Setenv("HTTP_PROXY", target.URL)
	client, transport := reportHTTPClient()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || client.Jar != nil || client.Timeout == 0 {
		t.Fatal("unsafe transport defaults")
	}
	if send(context.Background(), client, origin.URL, []byte(`{}`)) == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded != 0 {
		t.Fatal("request forwarded")
	}
}
