package agent

import "context"

// HTTPTransport wraps the existing Client to implement Transport.
// This is a thin adapter — no behavioral changes from the current HTTP protocol.
type HTTPTransport struct {
	client *Client
	events chan ServerEvent
}

// NewHTTPTransport creates a new HTTP-based transport.
func NewHTTPTransport(baseURL, apiKey, userAgent string) *HTTPTransport {
	return &HTTPTransport{
		client: NewClient(baseURL, apiKey, userAgent),
		events: make(chan ServerEvent, 10),
	}
}

func (t *HTTPTransport) Connect(_ context.Context) error { return nil }
func (t *HTTPTransport) Close() error                    { return nil }
func (t *HTTPTransport) Mode() string                    { return "http" }
func (t *HTTPTransport) Events() <-chan ServerEvent      { return t.events }

func (t *HTTPTransport) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	return t.client.Register(ctx, req)
}

func (t *HTTPTransport) SendHeartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	return t.client.Heartbeat(ctx, req)
}

func (t *HTTPTransport) SendProgress(ctx context.Context, update StatusUpdate) (*StatusResponse, error) {
	return t.client.ReportStatus(ctx, update)
}

func (t *HTTPTransport) BatchReportStatus(ctx context.Context, updates []StatusUpdate) (*BatchStatusResponse, error) {
	return t.client.BatchReportStatus(ctx, updates)
}

func (t *HTTPTransport) ClaimTasks(ctx context.Context, agentID string) (*TasksResponse, error) {
	return t.client.ClaimTasks(ctx, agentID)
}

func (t *HTTPTransport) Deregister(ctx context.Context, agentID string) error {
	return t.client.Deregister(ctx, agentID)
}

// Client returns the underlying HTTP client for direct use if needed.
func (t *HTTPTransport) Client() *Client { return t.client }
