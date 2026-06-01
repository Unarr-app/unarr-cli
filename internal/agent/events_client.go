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
)

// DownlinkEvent is one parsed Server-Sent Event from the agent events stream
// (GET /api/internal/agent/events). Event is the SSE "event:" name; Data is the
// raw "data:" payload (nil for heartbeat pings).
type DownlinkEvent struct {
	Event string
	Data  json.RawMessage
}

// CommandEvent is the payload of an "command" downlink event — typed control
// actions the server pushes for instant application (cancel/pause). Mirrors the
// `controls` field of /agent/sync so the same OnControl callback handles both.
type CommandEvent struct {
	Controls []ControlAction `json:"controls"`
}

// Downlink event names. Heartbeat pings surface as a distinct event so the
// consumer can reset its liveness deadline without acting on them.
const (
	DownlinkEventPing    = "ping"    // SSE comment line (`: hb`) — liveness only
	DownlinkEventSync    = "sync"    // nudge: run a full /agent/sync
	DownlinkEventCommand = "command" // typed control actions
)

// Bounds on the SSE reader, identical in spirit to the retired WebRTC signal
// reader: a hostile or buggy server must not be able to grow daemon memory by
// streaming one unbounded line or unbounded `data:` continuation lines.
const (
	eventsSSEMaxLineBytes  = 256 * 1024
	eventsSSEMaxEventBytes = 1024 * 1024
)

// EventStream wraps an open SSE downlink connection. Read from Events() until
// the channel closes (server recycle, network drop, or ctx cancel), then call
// Close() and reopen if you want to keep listening. Always defer Close().
type EventStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	events chan DownlinkEvent
	errs   chan error
	done   chan struct{}
}

// Events streams server-pushed downlink events. Heartbeat comments surface as
// DownlinkEvent{Event: DownlinkEventPing}. The channel closes when the
// connection ends.
func (s *EventStream) Events() <-chan DownlinkEvent { return s.events }

// Err returns the terminating error (if any) once Events() has closed.
func (s *EventStream) Err() error {
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

// Close cancels the request and waits for the reader goroutine to drain.
// Safe to call more than once.
func (s *EventStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.resp != nil {
		s.resp.Body.Close()
	}
	<-s.done
	return nil
}

// OpenEventStream opens a long-lived SSE connection to the agent events
// downlink. Routed through MirrorPool failover for the INITIAL connect only
// (a mid-stream drop is surfaced as a closed channel, not retried here — the
// caller reopens). Caller MUST Close() (or cancel ctx) to free resources.
func (c *Client) OpenEventStream(ctx context.Context) (*EventStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	var resp *http.Response
	err := c.withMirrorFailover(func(base string) error {
		req, reqErr := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/api/internal/agent/events", nil)
		if reqErr != nil {
			return fmt.Errorf("create events request: %w", reqErr)
		}
		c.setHeaders(req)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		// No-timeout client: the connection is intentionally long-lived; ctx
		// controls cancellation (same as the wake long-poll).
		r, doErr := c.wakeClient.Do(req)
		if doErr != nil {
			return fmt.Errorf("events request failed: %w", doErr)
		}
		if r.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<10))
			r.Body.Close()
			return &HTTPError{StatusCode: r.StatusCode, Message: strings.TrimSpace(string(body))}
		}
		resp = r
		return nil
	})
	if err != nil {
		cancel()
		return nil, err
	}

	stream := &EventStream{
		resp:   resp,
		cancel: cancel,
		events: make(chan DownlinkEvent, 8),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}
	go stream.read()
	return stream, nil
}

func (s *EventStream) read() {
	defer close(s.done)
	defer close(s.events)

	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 16*1024), eventsSSEMaxLineBytes)

	ctx := s.resp.Request.Context()
	var dataBuf bytes.Buffer
	var eventName string

	emit := func(ev DownlinkEvent) bool {
		select {
		case s.events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")

		if line == "" {
			// Blank line ends an event — dispatch if we accumulated data.
			if dataBuf.Len() > 0 {
				name := eventName
				if name == "" {
					name = "message"
				}
				data := make([]byte, dataBuf.Len())
				copy(data, dataBuf.Bytes())
				if !emit(DownlinkEvent{Event: name, Data: json.RawMessage(data)}) {
					return
				}
			}
			dataBuf.Reset()
			eventName = ""
			continue
		}

		if strings.HasPrefix(line, ":") {
			// SSE comment / heartbeat — surface as a ping so the consumer resets
			// its liveness deadline (and can tell a live stream from a silently
			// buffered one that never delivers anything).
			if !emit(DownlinkEvent{Event: DownlinkEventPing}) {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[len("data:"):])
			if dataBuf.Len()+len(payload)+1 > eventsSSEMaxEventBytes {
				select {
				case s.errs <- fmt.Errorf("sse: event exceeded %d bytes", eventsSSEMaxEventBytes):
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
		// id:, retry:, unknown fields — ignored.
	}
	if err := scanner.Err(); err != nil {
		select {
		case s.errs <- err:
		default:
		}
	}
}
