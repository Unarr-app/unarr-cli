package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The edge's 404 (Traefik with no route to a backend, which is every rolling
// deploy for a few seconds) must fail over like a 502, while the API's own 404
// stays final. Field agents logged `sync failed: API error 404: 404 page not
// found` and lost that cycle.
func TestGatewayNotFoundIsTransient(t *testing.T) {
	gw := httpErrorFromBody(http.StatusNotFound, []byte("404 page not found\n"))
	if !gw.FromGateway {
		t.Errorf("the edge's body was not recognised: %+v", gw)
	}
	if !IsTransient(gw) {
		t.Errorf("IsTransient(%v) = false, want true — another mirror serves it", gw)
	}
	if strings.Contains(gw.Error(), "API error") {
		t.Errorf("error text still blames the API: %q", gw.Error())
	}

	api := httpErrorFromBody(http.StatusNotFound, []byte(`{"error":"not_found","message":"no such task"}`))
	if api.FromGateway {
		t.Errorf("an API 404 was taken for the edge's: %+v", api)
	}
	if IsTransient(api) {
		t.Error("IsTransient(API 404) = true, want false — it fails the same everywhere")
	}
	if api.Message != "not_found" || api.Detail != "no such task" {
		t.Errorf("API error parsed as %+v, want the JSON fields", api)
	}
}

// An HTML error page says nothing actionable, so it is summarized — and it is
// not the edge's not-found either.
func TestNonJSONErrorIsSummarized(t *testing.T) {
	got := httpErrorFromBody(http.StatusBadGateway, []byte("<!DOCTYPE html><html><body>nginx</body></html>"))
	if got.FromGateway {
		t.Errorf("an HTML page was taken for the edge's not-found: %+v", got)
	}
	if !strings.Contains(got.Message, "non-JSON") {
		t.Errorf("message = %q, want the summary", got.Message)
	}
}

// classifyStatus reads the body to tell the two 404s apart, so it must put it
// back: the caller that receives this very response still reads it whole.
func TestClassifyStatusLeavesTheBodyReadable(t *testing.T) {
	const body = "404 page not found\n"
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusNotFound)
	rec.WriteString(body)
	resp := rec.Result()

	if he := classifyStatus(resp); !he.FromGateway {
		t.Errorf("classifyStatus did not recognise the edge's 404: %+v", he)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if string(rest) != body {
		t.Errorf("body after classification = %q, want %q", rest, body)
	}
}
