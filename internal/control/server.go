package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// TokenHeader carries the shared secret on every control request.
const TokenHeader = "X-Unarr-Control-Token" //nolint:gosec // G101: header NAME, not a credential

// Server is the daemon's loopback control endpoint.
type Server struct {
	ctrl  Controller
	token string
	ln    net.Listener
	srv   *http.Server
}

// NewServer creates a control server for the given controller. The token is
// generated here and published (by the daemon) into the state file, so a client
// that can read that file — the same user, and only the same user — can drive
// the plane.
func NewServer(ctrl Controller) (*Server, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	return &Server{ctrl: ctrl, token: tok}, nil
}

func newToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Token returns the secret clients must present.
func (s *Server) Token() string { return s.token }

// Port returns the bound TCP port, or 0 before Listen succeeds.
func (s *Server) Port() int {
	if s.ln == nil {
		return 0
	}
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// Listen binds 127.0.0.1 on an ephemeral port and serves until ctx is done.
//
// Loopback only, deliberately: the control plane can stop downloads and delete
// partial files, so it must not be reachable from the LAN the way the stream
// server is. Binding "127.0.0.1:0" also sidesteps the port-already-taken dance
// entirely — there is no fixed port to collide with a second agent (a dev agent
// and the production one share a host on purpose).
func (s *Server) Listen(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind control server: %w", err)
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", s.handleList)
	mux.HandleFunc("/tasks/", s.handleAction)

	s.srv = &http.Server{
		Handler:           s.withAuth(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[control] server stopped: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// Derived from ctx so it keeps its values, but WITHOUT its cancellation:
		// ctx is already done by definition here, and a dead context would make
		// Shutdown close connections instantly instead of draining them.
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	log.Printf("[control] local control plane on 127.0.0.1:%d", s.Port())
	return nil
}

// Close stops the server (used by tests and by a shutdown that cannot wait for
// the context goroutine).
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// withAuth rejects anything without the token, in constant time. Also refuses
// non-loopback peers: the listener already binds loopback, but a future change
// (or a proxy in front) must not silently widen the surface.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopback(host) {
			http.Error(w, "control plane is loopback-only", http.StatusForbidden)
			return
		}
		got := r.Header.Get(TokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "bad control token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, ListResponse{Tasks: s.ctrl.List()})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/tasks/")
	var req ActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ActionResponse{Error: "invalid JSON body"})
		return
	}

	if action == ActionPurge {
		writeJSON(w, http.StatusOK, ActionResponse{Results: s.ctrl.Purge()})
		return
	}

	targets, err := s.resolveTargets(req)
	if err != nil {
		writeJSON(w, http.StatusNotFound, ActionResponse{Error: err.Error()})
		return
	}

	results := make([]ActionResult, 0, len(targets))
	for _, id := range targets {
		res, aerr := s.apply(action, id, req.DeleteFiles)
		if aerr != nil {
			writeJSON(w, http.StatusBadRequest, ActionResponse{Error: aerr.Error()})
			return
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, ActionResponse{Results: results})
}

func (s *Server) apply(action, taskID string, deleteFiles bool) (ActionResult, error) {
	switch action {
	case ActionPause:
		return s.ctrl.Pause(taskID), nil
	case ActionResume:
		return s.ctrl.Resume(taskID), nil
	case ActionCancel:
		return s.ctrl.Cancel(taskID, deleteFiles), nil
	case ActionRetry:
		return s.ctrl.Retry(taskID), nil
	default:
		return ActionResult{}, fmt.Errorf("unsupported action %q", action)
	}
}

// resolveTargets expands the request into concrete task ids, accepting a unique
// short prefix. An ambiguous prefix is an error rather than a guess — picking
// one of two downloads to kill would be the worst possible resolution.
func (s *Server) resolveTargets(req ActionRequest) ([]string, error) {
	tasks := s.ctrl.List()
	if req.All {
		ids := make([]string, 0, len(tasks))
		for _, t := range tasks {
			ids = append(ids, t.ID)
		}
		if len(ids) == 0 {
			return nil, errors.New("no downloads to act on")
		}
		return ids, nil
	}
	if req.TaskID == "" {
		return nil, errors.New("taskId or all is required")
	}

	var matches []string
	for _, t := range tasks {
		if t.ID == req.TaskID {
			return []string{t.ID}, nil // exact wins over any prefix
		}
		if strings.HasPrefix(t.ID, req.TaskID) {
			matches = append(matches, t.ID)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no download matches %q", req.TaskID)
	case 1:
		return matches, nil
	default:
		return nil, fmt.Errorf("%q matches %d downloads — use the full id", req.TaskID, len(matches))
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[control] write response: %v", err)
	}
}
