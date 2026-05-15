package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SignalRole identifies who produced a signalling message. The opposite role
// receives it.
type SignalRole string

const (
	SignalRoleBrowser SignalRole = "browser"
	SignalRoleAgent   SignalRole = "agent"
)

// SignalMessageType matches the server-side z.enum on
// /api/internal/stream/signal/[sessionId] route.
type SignalMessageType string

const (
	SignalMsgOffer        SignalMessageType = "offer"
	SignalMsgAnswer       SignalMessageType = "answer"
	SignalMsgCandidate    SignalMessageType = "candidate"
	SignalMsgCandidateEnd SignalMessageType = "candidate-end"
	SignalMsgBye          SignalMessageType = "bye"
)

// SignalMessage mirrors the bus envelope on the web side.
type SignalMessage struct {
	From    SignalRole        `json:"from"`
	Type    SignalMessageType `json:"type"`
	Payload string            `json:"payload"`
	TS      int64             `json:"ts"`
}

// PostSignal enqueues a signalling message produced by this agent. The
// browser receives it on its next SSE event push.
func (c *Client) PostSignal(ctx context.Context, sessionID string, msg SignalMessage) error {
	body := map[string]any{
		"from":    string(SignalRoleAgent),
		"type":    string(msg.Type),
		"payload": msg.Payload,
	}
	path := fmt.Sprintf("/api/internal/stream/signal/%s", sessionID)
	return c.doPost(ctx, path, body, &struct {
		OK bool `json:"ok"`
	}{})
}

// SignalEventStream wraps an open SSE connection. Read messages from Events()
// until the channel closes (server timeout or context cancel). Always defer
// Close() to release the underlying response body.
type SignalEventStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	events chan SignalMessage
	errs   chan error
	done   chan struct{}
}

// Events streams browser-produced messages addressed to the agent.
// The channel closes when the SSE connection ends; the caller should then
// call Close() and reopen if it wants to keep listening.
func (s *SignalEventStream) Events() <-chan SignalMessage { return s.events }

// Err returns the terminating error (if any) once Events() has closed.
func (s *SignalEventStream) Err() error {
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

// Close cancels the underlying HTTP request and waits for the reader goroutine
// to drain. Safe to call more than once.
func (s *SignalEventStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.resp != nil {
		s.resp.Body.Close()
	}
	<-s.done
	return nil
}

// OpenSignalStream opens a long-lived SSE connection to the signal events
// endpoint. Caller MUST cancel ctx (or call Close()) to free resources.
//
// The server caps each response at ~25 s; OpenSignalStream surfaces the
// disconnect by closing the events channel. Caller should reopen until the
// session ends.
func (c *Client) OpenSignalStream(ctx context.Context, sessionID string) (*SignalEventStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	url := fmt.Sprintf("%s/api/internal/stream/signal/%s/events", c.baseURL(), sessionID)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open signal stream: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")

	// Use a per-call client with no timeout (SSE connections are long).
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open signal stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("open signal stream: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	stream := &SignalEventStream{
		resp:   resp,
		cancel: cancel,
		events: make(chan SignalMessage, 8),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}

	go stream.read()
	return stream, nil
}

// sseMaxLineBytes caps the size of a single SSE line. Real signalling lines
// are JSON payloads of a few hundred bytes; 256 KiB is generous enough to
// survive a future schema bump but small enough that a hostile or buggy
// server cannot grow daemon memory by streaming a single line forever.
const sseMaxLineBytes = 256 * 1024

// sseMaxEventBytes caps the total bytes buffered across the lines of one
// SSE event. Without a cap, a peer could send unbounded `data:` continuation
// lines and OOM the daemon between blank-line dispatches.
const sseMaxEventBytes = 1024 * 1024

func (s *SignalEventStream) read() {
	defer close(s.done)
	defer close(s.events)

	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 16*1024), sseMaxLineBytes)

	var dataBuf bytes.Buffer
	var eventName string

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			// End of an event — dispatch if we have data.
			if dataBuf.Len() == 0 {
				eventName = ""
				continue
			}
			if eventName == "" || eventName == "signal" {
				var msg SignalMessage
				if err := json.Unmarshal(dataBuf.Bytes(), &msg); err == nil {
					select {
					case s.events <- msg:
					case <-s.resp.Request.Context().Done():
						return
					}
				}
			}
			dataBuf.Reset()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			// SSE comment (heartbeat); ignore.
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[len("data:"):])
			// Refuse to grow the event buffer past the cap. Reset so a
			// well-formed event after the offender can still be parsed,
			// and surface an error so SignalLoop reconnects.
			if dataBuf.Len()+len(payload)+1 > sseMaxEventBytes {
				dataBuf.Reset()
				eventName = ""
				select {
				case s.errs <- fmt.Errorf("sse: event exceeded %d bytes", sseMaxEventBytes):
				default:
				}
				return
			}
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(payload)
			continue
		}
		// id:, retry:, anything else — ignore for now.
	}
	if err := scanner.Err(); err != nil {
		select {
		case s.errs <- err:
		default:
		}
	}
}

// SignalLoop runs an SSE consumer that reconnects automatically on disconnect.
// onMessage is called for every browser-produced message. Returns when ctx is
// cancelled. Reconnect backoff is fixed at 1 s — the server already paces
// reconnects with `retry: 1500` headers so churn is bounded.
func (c *Client) SignalLoop(ctx context.Context, sessionID string, onMessage func(SignalMessage)) error {
	for ctx.Err() == nil {
		stream, err := c.OpenSignalStream(ctx, sessionID)
		if err != nil {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		for msg := range stream.Events() {
			onMessage(msg)
		}
		streamErr := stream.Err()
		stream.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Server closes the SSE every ~25 s; reconnect immediately.
		// Hard error → small backoff so we don't hammer.
		if streamErr != nil {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}
