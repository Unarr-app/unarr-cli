package metrics

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// TokenHeader is accepted next to `Authorization: Bearer`, so the control-plane
// token (which the control plane takes in this header) works unchanged.
const TokenHeader = "X-Unarr-Control-Token" //nolint:gosec // G101: header NAME, not a credential

// Server is the opt-in /metrics endpoint. A nil *Server is valid and inert, so
// a disabled feature costs a nil check, not a branch at every call site.
type Server struct {
	ln    net.Listener
	srv   *http.Server
	token string
}

// Listen starts the endpoint on addr and serves until ctx is done.
//
// Auth is mandatory. token is the shared secret a scraper sends as
// `Authorization: Bearer <token>` (what Prometheus' authorization block does)
// or in TokenHeader. A non-loopback addr is refused without a token of its own:
// the caller falls back to the control-plane token for loopback only, and that
// token changes on every daemon start, which no static scrape config can follow.
func Listen(ctx context.Context, addr, token string, hub *Hub) (*Server, error) {
	if token == "" {
		return nil, errors.New("metrics: empty token")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind metrics endpoint: %w", err)
	}
	s := &Server{ln: ln, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		if err := WriteProm(w, hub.Poll(time.Now())); err != nil {
			log.Printf("[metrics] write response: %v", err)
		}
	})
	s.srv = &http.Server{Handler: s.withAuth(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[metrics] server stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	log.Printf("[metrics] Prometheus endpoint on %s/metrics", ln.Addr())
	return s, nil
}

// Addr returns the bound address, or "" for a nil/unstarted server.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops the server.
func (s *Server) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(TokenHeader)
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			got = b
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="unarr-metrics"`)
			http.Error(w, "bad metrics token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
