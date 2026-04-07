package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WSTransport communicates with the server via WebSocket through a Cloudflare Durable Object.
type WSTransport struct {
	wsURL     string // wss://unarr.torrentclaw.com/ws/{agentId}
	apiKey    string
	agentID   string
	userAgent string

	conn   *websocket.Conn
	mu     sync.Mutex
	events chan ServerEvent
	closed atomic.Bool

	// Cached auth response from the DO
	authResp     *RegisterResponse
	authMu       sync.Mutex
	authDone     chan struct{}
	authDoneOnce sync.Once
}

// NewWSTransport creates a WebSocket-based transport.
func NewWSTransport(wsURL, apiKey, agentID, userAgent string) *WSTransport {
	return &WSTransport{
		wsURL:     wsURL,
		apiKey:    apiKey,
		agentID:   agentID,
		userAgent: userAgent,
		events:    make(chan ServerEvent, 50),
		authDone:  make(chan struct{}),
	}
}

func (t *WSTransport) Mode() string               { return "ws" }
func (t *WSTransport) Events() <-chan ServerEvent { return t.events }

// Connect dials the WebSocket server and starts the read loop.
func (t *WSTransport) Connect(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	header := http.Header{}
	header.Set("User-Agent", t.userAgent)

	// Append API key as query param for auth on WS upgrade
	wsURLWithKey := t.wsURL
	if t.apiKey != "" {
		sep := "?"
		if strings.Contains(wsURLWithKey, "?") {
			sep = "&"
		}
		wsURLWithKey += sep + "key=" + t.apiKey
	}

	conn, wsResp, err := dialer.DialContext(ctx, wsURLWithKey, header)
	if wsResp != nil && wsResp.Body != nil {
		defer wsResp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}

	t.mu.Lock()
	t.conn = conn
	t.closed.Store(false)
	t.authDone = make(chan struct{})
	t.authDoneOnce = sync.Once{}
	t.mu.Unlock()

	go t.readLoop(conn)
	return nil
}

// Close sends a close frame and shuts down the connection.
func (t *WSTransport) Close() error {
	t.closed.Store(true)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != nil {
		_ = t.conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}

// Register sends auth message and waits for the registered response.
func (t *WSTransport) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	msg := wsAuthMessage{
		Type:           "auth",
		APIKey:         t.apiKey,
		AgentID:        req.AgentID,
		Name:           req.Name,
		OS:             req.OS,
		Arch:           req.Arch,
		Version:        req.Version,
		DownloadDir:    req.DownloadDir,
		DiskFreeBytes:  req.DiskFreeBytes,
		DiskTotalBytes: req.DiskTotalBytes,
	}

	if err := t.send(msg); err != nil {
		return nil, fmt.Errorf("ws auth send: %w", err)
	}

	// Wait for the auth response or context cancellation
	select {
	case <-t.authDone:
		t.authMu.Lock()
		resp := t.authResp
		t.authMu.Unlock()
		if resp == nil {
			return nil, fmt.Errorf("ws auth: no response received")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("ws auth: timeout waiting for registered response")
	}
}

// SendHeartbeat sends a heartbeat message. No blocking response in WS mode.
func (t *WSTransport) SendHeartbeat(_ context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	msg := struct {
		Type string `json:"type"`
		Disk *struct {
			Free  int64 `json:"free"`
			Total int64 `json:"total"`
		} `json:"disk,omitempty"`
	}{Type: "heartbeat"}

	if req.DiskFreeBytes > 0 || req.DiskTotalBytes > 0 {
		msg.Disk = &struct {
			Free  int64 `json:"free"`
			Total int64 `json:"total"`
		}{Free: req.DiskFreeBytes, Total: req.DiskTotalBytes}
	}

	if err := t.send(msg); err != nil {
		return nil, err
	}
	// WS mode: heartbeat is fire-and-forget. Upgrade signals arrive via Events().
	return &HeartbeatResponse{Success: true}, nil
}

// SendProgress sends a progress update. Control signals arrive async via Events().
func (t *WSTransport) SendProgress(_ context.Context, update StatusUpdate) (*StatusResponse, error) {
	msg := struct {
		Type            string `json:"type"`
		TaskID          string `json:"taskId"`
		Status          string `json:"status,omitempty"`
		Progress        int    `json:"progress,omitempty"`
		DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
		TotalBytes      int64  `json:"totalBytes,omitempty"`
		SpeedBps        int64  `json:"speedBps,omitempty"`
		ETA             int    `json:"eta,omitempty"`
		ResolvedMethod  string `json:"resolvedMethod,omitempty"`
		FileName        string `json:"fileName,omitempty"`
		FilePath        string `json:"filePath,omitempty"`
		StreamURL       string `json:"streamUrl,omitempty"`
		StreamReady     bool   `json:"streamReady,omitempty"`
		ErrorMessage    string `json:"errorMessage,omitempty"`
	}{
		Type:            "progress",
		TaskID:          update.TaskID,
		Status:          update.Status,
		Progress:        update.Progress,
		DownloadedBytes: update.DownloadedBytes,
		TotalBytes:      update.TotalBytes,
		SpeedBps:        update.SpeedBps,
		ETA:             update.ETA,
		ResolvedMethod:  update.ResolvedMethod,
		FileName:        update.FileName,
		FilePath:        update.FilePath,
		StreamURL:       update.StreamURL,
		StreamReady:     update.StreamReady,
		ErrorMessage:    update.ErrorMessage,
	}

	if err := t.send(msg); err != nil {
		return nil, err
	}
	// In WS mode, control signals come via Events(), not in the progress response.
	return &StatusResponse{Success: true}, nil
}

// ClaimTasks is a no-op in WS mode — tasks arrive via Events().
func (t *WSTransport) ClaimTasks(_ context.Context, _ string) (*TasksResponse, error) {
	return &TasksResponse{}, nil
}

// Deregister is handled by WebSocket close (DO detects disconnection).
func (t *WSTransport) Deregister(_ context.Context, _ string) error {
	return t.Close()
}

// ── Internal ─────────────────────────────────────────────────────────────────

func (t *WSTransport) send(msg any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return fmt.Errorf("ws: not connected")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return t.conn.WriteMessage(websocket.TextMessage, data)
}

func (t *WSTransport) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !t.closed.Load() {
				log.Printf("[ws] read error: %v", err)
				// Signal disconnection to the daemon
				select {
				case t.events <- ServerEvent{Type: "disconnected"}:
				default:
				}
			}
			return
		}

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			log.Printf("[ws] invalid message: %v", err)
			continue
		}

		switch envelope.Type {
		case "registered":
			var resp wsRegisteredMessage
			if json.Unmarshal(msg, &resp) == nil {
				t.authMu.Lock()
				t.authResp = &RegisterResponse{
					Success:  true,
					User:     resp.User,
					Features: resp.Features,
				}
				t.authMu.Unlock()
				// Signal that auth is complete (sync.Once prevents double-close panic)
				t.authDoneOnce.Do(func() { close(t.authDone) })
			}

		case "tasks":
			var resp wsTasksMessage
			if json.Unmarshal(msg, &resp) == nil {
				select {
				case t.events <- ServerEvent{
					Type: "tasks",
					Tasks: &TasksResponse{
						Tasks:          resp.Tasks,
						StreamRequests: resp.StreamRequests,
					},
				}:
				default:
					log.Printf("[ws] events channel full, dropping tasks message")
				}
			}

		case "upgrade":
			var resp wsUpgradeMessage
			if json.Unmarshal(msg, &resp) == nil {
				select {
				case t.events <- ServerEvent{
					Type:    "upgrade",
					Upgrade: &UpgradeSignal{Version: resp.Version},
				}:
				default:
				}
			}

		case "control":
			var resp ControlAction
			if json.Unmarshal(msg, &resp) == nil {
				select {
				case t.events <- ServerEvent{
					Type:    "control",
					Control: &resp,
				}:
				default:
				}
			}

		case "error":
			var resp struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(msg, &resp) == nil {
				log.Printf("[ws] server error: %s", resp.Message)
			}
		}
	}
}

// ── WS message types ─────────────────────────────────────────────────────────

type wsAuthMessage struct {
	Type           string `json:"type"`
	APIKey         string `json:"apiKey"`
	AgentID        string `json:"agentId"`
	Name           string `json:"name,omitempty"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Version        string `json:"version,omitempty"`
	DownloadDir    string `json:"downloadDir,omitempty"`
	DiskFreeBytes  int64  `json:"diskFreeBytes,omitempty"`
	DiskTotalBytes int64  `json:"diskTotalBytes,omitempty"`
}

type wsRegisteredMessage struct {
	Type     string       `json:"type"`
	User     UserInfo     `json:"user"`
	Features FeatureFlags `json:"features"`
}

type wsTasksMessage struct {
	Type           string          `json:"type"`
	Tasks          []Task          `json:"tasks"`
	StreamRequests []StreamRequest `json:"streamRequests,omitempty"`
}

type wsUpgradeMessage struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}
