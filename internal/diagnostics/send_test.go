package diagnostics

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendExactPayloadAndNoIdentityHeaders(t *testing.T) {
	payload := []byte("{\n  \"schemaVersion\": 1\n}")
	var posts int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-For", "X-Real-IP"} {
			if r.Header.Get(h) != "" {
				t.Errorf("unexpected %s", h)
			}
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"schemaVersion":1,"acceptsAnonymousReports":true,"maxBytes":1048576}`)
			return
		}
		posts++
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, payload) {
			t.Error("payload changed")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer s.Close()
	if err := send(context.Background(), s.Client(), s.URL, payload); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts=%d", posts)
	}
}

func TestSendFailsClosedBeforeUpload(t *testing.T) {
	for _, capability := range []string{`{}`, `{"schemaVersion":2,"acceptsAnonymousReports":true,"maxBytes":1048576}`, `{"schemaVersion":1,"acceptsAnonymousReports":false,"maxBytes":1048576}`, `{"schemaVersion":1,"acceptsAnonymousReports":true,"maxBytes":1}`, string(bytes.Repeat([]byte("x"), 4097))} {
		t.Run(capability[:min(len(capability), 60)], func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					t.Error("uploaded without capability")
				}
				_, _ = io.WriteString(w, capability)
			}))
			defer s.Close()
			if err := send(context.Background(), s.Client(), s.URL, []byte(`{}`)); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestSendBoundedAndCanceled(t *testing.T) {
	var requests int
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer s.Close()
	if send(context.Background(), s.Client(), s.URL, make([]byte, MaxReportBytes+1)) == nil {
		t.Fatal("oversize accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if send(ctx, s.Client(), s.URL, []byte(`{}`)) == nil {
		t.Fatal("cancellation ignored")
	}
	if requests != 0 {
		t.Fatal("unexpected network request")
	}
}
