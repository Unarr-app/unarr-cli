package agent

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// HybridTransport tries WebSocket first, falls back to HTTP if WS fails.
// Automatically reconnects WS in the background.
type HybridTransport struct {
	ws   *WSTransport
	http *HTTPTransport

	mode   atomic.Value // "ws" or "http"
	events chan ServerEvent

	reconnectMu      sync.Mutex
	reconnectRunning bool
	reconnectStop    chan struct{}
	closed           atomic.Bool
}

// NewHybridTransport creates a transport that prefers WS with HTTP fallback.
func NewHybridTransport(ws *WSTransport, http *HTTPTransport) *HybridTransport {
	h := &HybridTransport{
		ws:            ws,
		http:          http,
		events:        make(chan ServerEvent, 50),
		reconnectStop: make(chan struct{}),
	}
	h.mode.Store("http") // start in HTTP, upgrade to WS on Connect
	return h
}

func (h *HybridTransport) Mode() string               { return h.mode.Load().(string) }
func (h *HybridTransport) Events() <-chan ServerEvent { return h.events }

// Connect tries WS first. If it fails, falls back to HTTP and starts reconnection loop.
func (h *HybridTransport) Connect(ctx context.Context) error {
	// Try WebSocket first
	if err := h.ws.Connect(ctx); err != nil {
		log.Printf("[transport] WebSocket connect failed (%v), using HTTP fallback", err)
		h.mode.Store("http")
		h.startReconnectLoop()
		return h.http.Connect(ctx)
	}

	h.mode.Store("ws")
	log.Println("[transport] Connected via WebSocket")

	// Forward WS events to unified channel + watch for disconnection
	go h.forwardWSEvents()

	return nil
}

// Close shuts down both transports and stops reconnection.
func (h *HybridTransport) Close() error {
	h.closed.Store(true)
	select {
	case <-h.reconnectStop:
	default:
		close(h.reconnectStop)
	}
	_ = h.ws.Close()
	return h.http.Close()
}

// Register delegates to the active transport.
func (h *HybridTransport) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	if h.mode.Load() == "ws" {
		return h.ws.Register(ctx, req)
	}
	return h.http.Register(ctx, req)
}

// SendHeartbeat delegates to the active transport.
func (h *HybridTransport) SendHeartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	if h.mode.Load() == "ws" {
		resp, err := h.ws.SendHeartbeat(ctx, req)
		if err != nil {
			// WS write failed — switch to HTTP
			h.switchToHTTP()
			return h.http.SendHeartbeat(ctx, req)
		}
		return resp, nil
	}
	return h.http.SendHeartbeat(ctx, req)
}

// SendProgress delegates to the active transport.
func (h *HybridTransport) SendProgress(ctx context.Context, update StatusUpdate) (*StatusResponse, error) {
	if h.mode.Load() == "ws" {
		resp, err := h.ws.SendProgress(ctx, update)
		if err != nil {
			h.switchToHTTP()
			return h.http.SendProgress(ctx, update)
		}
		return resp, nil
	}
	return h.http.SendProgress(ctx, update)
}

// ClaimTasks delegates to the active transport.
func (h *HybridTransport) ClaimTasks(ctx context.Context, agentID string) (*TasksResponse, error) {
	if h.mode.Load() == "ws" {
		return h.ws.ClaimTasks(ctx, agentID) // no-op in WS mode
	}
	return h.http.ClaimTasks(ctx, agentID)
}

// Deregister delegates to the active transport.
func (h *HybridTransport) Deregister(ctx context.Context, agentID string) error {
	if h.mode.Load() == "ws" {
		return h.ws.Deregister(ctx, agentID)
	}
	return h.http.Deregister(ctx, agentID)
}

// ── Internal ─────────────────────────────────────────────────────────────────

func (h *HybridTransport) switchToHTTP() {
	if h.mode.Load() == "http" {
		return
	}
	log.Println("[transport] Switching to HTTP fallback")
	h.mode.Store("http")
	_ = h.ws.Close()
	h.startReconnectLoop()
}

func (h *HybridTransport) forwardWSEvents() {
	for {
		select {
		case <-h.reconnectStop:
			return
		case event, ok := <-h.ws.Events():
			if !ok {
				return // channel closed
			}
			if event.Type == "disconnected" {
				h.switchToHTTP()
				select {
				case h.events <- event:
				default:
				}
				return
			}
			select {
			case h.events <- event:
			default:
				log.Printf("[transport] events channel full, dropping %s event", event.Type)
			}
		}
	}
}

func (h *HybridTransport) startReconnectLoop() {
	h.reconnectMu.Lock()
	defer h.reconnectMu.Unlock()
	if h.reconnectRunning {
		return
	}
	h.reconnectRunning = true
	go h.reconnectLoop()
}

func (h *HybridTransport) reconnectLoop() {
	backoff := 5 * time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-h.reconnectStop:
			return
		case <-time.After(backoff):
		}

		if h.closed.Load() {
			return
		}

		// Already on WS? (someone else reconnected)
		if h.mode.Load() == "ws" {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := h.ws.Connect(ctx)
		cancel()

		if err != nil {
			log.Printf("[transport] WS reconnect failed: %v (retry in %v)", err, backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// WS reconnected — switch back
		log.Println("[transport] WebSocket reconnected")
		h.mode.Store("ws")

		// Reset reconnect flag so loop can start again if WS drops
		h.reconnectMu.Lock()
		h.reconnectRunning = false
		h.reconnectMu.Unlock()

		// Forward events from new WS connection
		go h.forwardWSEvents()
		return
	}
}
