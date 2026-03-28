package agent

import "context"

// Transport abstracts the communication protocol between the agent and server.
// Both WebSocket (via CF Durable Object) and HTTP (direct to origin) implement this.
type Transport interface {
	// Connect establishes the transport connection.
	Connect(ctx context.Context) error

	// Close tears down the connection gracefully.
	Close() error

	// Mode returns the current transport mode ("ws" or "http").
	Mode() string

	// Register sends agent registration and returns user info + features.
	Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error)

	// SendHeartbeat sends a periodic keep-alive.
	SendHeartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error)

	// SendProgress reports download progress for a task.
	SendProgress(ctx context.Context, update StatusUpdate) (*StatusResponse, error)

	// ClaimTasks polls for new tasks (HTTP mode only; WS receives via Events).
	ClaimTasks(ctx context.Context, agentID string) (*TasksResponse, error)

	// Deregister notifies the server of graceful shutdown.
	Deregister(ctx context.Context, agentID string) error

	// ReportUpgradeResult reports upgrade outcome.
	ReportUpgradeResult(ctx context.Context, result UpgradeResult) error

	// Events returns a channel that emits server-initiated events.
	// In HTTP mode this channel is never written to (polling handles it).
	// In WS mode, tasks/upgrade/control arrive here.
	Events() <-chan ServerEvent
}

// ServerEvent represents a server-initiated message received via WebSocket.
type ServerEvent struct {
	Type    string         // "tasks", "upgrade", "control", "disconnected"
	Tasks   *TasksResponse // populated when Type == "tasks"
	Upgrade *UpgradeSignal // populated when Type == "upgrade"
	Control *ControlAction // populated when Type == "control"
}

// ControlAction represents a server push for task control.
type ControlAction struct {
	Action string `json:"action"` // "pause", "resume", "cancel", "stream"
	TaskID string `json:"taskId"`
}
