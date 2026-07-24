package agent

import (
	"context"
	"log"
	"time"
)

// Lifecycle telemetry — onboarding + exit events the daemon reports so the
// server stops being blind to WHY an agent that registered never came back.
// The web side (POST /api/internal/agent/event) records them into agent_event.
//
// Two hard rules:
//   - Fire-and-forget: a telemetry post must NEVER block or fail the caller's
//     real work (startup, sync, shutdown). Every emit runs on a short timeout
//     and swallows its error (logged, never returned).
//   - Opt-out honoured at the SOURCE: when telemetry is disabled the emitter is
//     a no-op — no network call is made at all. The gate lives in the Emitter
//     the caller holds, not scattered across call sites.

// Event type vocabulary — mirrors AGENT_EVENT_TYPES in the web's
// src/lib/services/agent-events.ts. Kept as typed constants so a typo is a
// compile error here rather than an "unknown" bucket on the server.
const (
	EventLoginOK          = "login_ok"
	EventFirstSync        = "first_sync" // recorded server-side on sync; here for completeness
	EventDaemonStartFail  = "daemon_start_fail"
	EventConfigError      = "config_error"
	EventPortInUse        = "port_in_use"
	EventPermissionDenied = "permission_denied"

	EventExitUserQuit = "exit_user_quit"
	EventExitCrash    = "exit_crash"
	EventExitNormal   = "exit_normal"
)

// telemetryTimeout bounds a single event post. Short — telemetry must never
// hold up a shutdown or a start-failure exit path.
const telemetryTimeout = 5 * time.Second

// reportEventRequest is the /api/internal/agent/event single-event body.
// Mirrors the web route's zod schema.
type reportEventRequest struct {
	Event agentEventPayload `json:"event"`
}

type agentEventPayload struct {
	Type    string `json:"type"`
	Detail  string `json:"detail,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
}

// reportEvent posts one lifecycle event. Best-effort — the caller uses the
// Emitter wrapper, not this directly. Returns an error only so the Emitter can
// log it; no caller acts on it.
func (c *Client) reportEvent(ctx context.Context, ev agentEventPayload) error {
	var resp StatusResponse
	return c.doPost(ctx, "/api/internal/agent/event", reportEventRequest{Event: ev}, &resp)
}

// Emitter is the telemetry entry point the daemon and command paths hold. It
// captures the enabled gate + the agent's identity (agentId/version/os) once,
// so call sites only pass a type + detail. A nil Emitter is a safe no-op, and a
// disabled Emitter never touches the network — telemetry is purely additive and
// must degrade to nothing when off.
type Emitter struct {
	client  *Client
	enabled bool
	agentID string
	version string
	os      string
}

// NewEmitter builds a telemetry emitter. Pass enabled=false (from
// config.TelemetryEnabled()) to make every Emit a no-op.
func NewEmitter(client *Client, enabled bool, agentID, version, osName string) *Emitter {
	return &Emitter{client: client, enabled: enabled, agentID: agentID, version: version, os: osName}
}

// Enabled reports whether this emitter will actually send. Used by callers that
// decide whether to attach exitReason to a sync (vs. a standalone event).
func (e *Emitter) Enabled() bool { return e != nil && e.enabled && e.client != nil }

// Emit sends one lifecycle event asynchronously. No-op when disabled. Never
// blocks the caller: the post runs in its own goroutine with a bounded timeout,
// and any failure is logged, not propagated. `detail` is free-form context
// (an error string, a port, a path) — bounded server-side to 500 chars.
func (e *Emitter) Emit(eventType, detail string) {
	if !e.Enabled() {
		return
	}
	ev := agentEventPayload{
		Type:    eventType,
		Detail:  detail,
		AgentID: e.agentID,
		Version: e.version,
		OS:      e.os,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), telemetryTimeout)
		defer cancel()
		if err := e.client.reportEvent(ctx, ev); err != nil {
			log.Printf("[telemetry] emit %s failed (ignored): %v", eventType, err)
		}
	}()
}

// EmitSync sends one event and BLOCKS until it completes (or the timeout). Used
// on the start-failure path, where the process is about to exit and a
// backgrounded goroutine would be killed before its post lands. Still swallows
// its error — a failed telemetry post must not change the exit the caller was
// already returning. No-op when disabled.
func (e *Emitter) EmitSync(eventType, detail string) {
	if !e.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryTimeout)
	defer cancel()
	ev := agentEventPayload{
		Type:    eventType,
		Detail:  detail,
		AgentID: e.agentID,
		Version: e.version,
		OS:      e.os,
	}
	if err := e.client.reportEvent(ctx, ev); err != nil {
		log.Printf("[telemetry] emit %s failed (ignored): %v", eventType, err)
	}
}
