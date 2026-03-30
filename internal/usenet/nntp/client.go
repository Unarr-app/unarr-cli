package nntp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"sync"
	"time"
)

// Config holds NNTP server connection parameters.
type Config struct {
	Host           string
	Port           int
	SSL            bool
	TLSServerName  string // override for TLS cert validation (e.g., "xsnews.nl" when Host is a CNAME)
	Username       string
	Password       string
	MaxConnections int // default 10
}

// Client manages a pool of authenticated NNTP connections.
type Client struct {
	cfg  Config
	pool chan *conn
	mu   sync.Mutex
	open int
	done chan struct{} // closed on Close()
}

// conn is a single NNTP connection.
type conn struct {
	tp     *textproto.Conn
	raw    net.Conn
	closed bool
}

// NewClient creates a new NNTP client (does not connect yet).
func NewClient(cfg Config) *Client {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 10
	}
	return &Client{
		cfg:  cfg,
		pool: make(chan *conn, cfg.MaxConnections),
		done: make(chan struct{}),
	}
}

// Connect opens and authenticates all connections in the pool.
// Safe to call again after a previous Connect failure.
func (c *Client) Connect(ctx context.Context) error {
	// Reset done channel if previously closed (allows retry after failure)
	select {
	case <-c.done:
		c.done = make(chan struct{})
	default:
	}

	for i := 0; i < c.cfg.MaxConnections; i++ {
		conn, err := c.dial(ctx)
		if err != nil {
			// Close any connections we already opened, but keep client reusable
			c.drainPool()
			return fmt.Errorf("nntp: connect %d/%d: %w", i+1, c.cfg.MaxConnections, err)
		}
		c.pool <- conn
		c.mu.Lock()
		c.open++
		c.mu.Unlock()
	}
	return nil
}

// drainPool closes all connections in the pool without closing the done channel.
func (c *Client) drainPool() {
	for {
		select {
		case cn := <-c.pool:
			c.closeConn(cn)
		default:
			c.mu.Lock()
			c.open = 0
			c.mu.Unlock()
			return
		}
	}
}

// Body downloads the body of an NNTP article by message-ID.
// Returns the raw body reader (typically yEnc encoded).
// The caller MUST call release() when done reading.
func (c *Client) Body(ctx context.Context, messageID string) ([]byte, error) {
	cn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	data, err := c.bodyOnConn(ctx, cn, messageID)
	if err != nil {
		// Connection might be broken, try to reconnect
		cn2, reconErr := c.reconnect(ctx, cn)
		if reconErr != nil {
			c.discard(cn)
			return nil, fmt.Errorf("nntp: body failed and reconnect failed: %w (original: %v)", reconErr, err)
		}
		// Retry once on the fresh connection
		data, err = c.bodyOnConn(ctx, cn2, messageID)
		if err != nil {
			c.release(cn2)
			return nil, err
		}
		c.release(cn2)
		return data, nil
	}

	c.release(cn)
	return data, nil
}

// ActiveConnections returns the number of open connections.
func (c *Client) ActiveConnections() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open
}

// Close shuts down all connections in the pool.
func (c *Client) Close() error {
	select {
	case <-c.done:
		return nil // already closed
	default:
		close(c.done)
	}

	// Drain pool and close connections
	for {
		select {
		case cn := <-c.pool:
			c.closeConn(cn)
		default:
			return nil
		}
	}
}

// --- Internal ---

func (c *Client) dial(ctx context.Context) (*conn, error) {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)

	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var rawConn net.Conn
	var err error

	if c.cfg.SSL {
		// Use TLSServerName if set (e.g., cert is for xsnews.nl but host is reader.torrentclaw.com)
		serverName := c.cfg.TLSServerName
		if serverName == "" {
			serverName = c.cfg.Host
		}
		tlsCfg := &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		}
		rawConn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		rawConn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	tp := textproto.NewConn(rawConn)
	cn := &conn{tp: tp, raw: rawConn}

	// Read welcome banner (200 or 201)
	code, msg, err := tp.ReadCodeLine(200)
	if err != nil {
		// Also accept 201 (posting not allowed)
		if code != 201 {
			rawConn.Close()
			return nil, fmt.Errorf("welcome: %d %s: %w", code, msg, err)
		}
	}

	// Authenticate
	if c.cfg.Username != "" {
		if err := c.auth(tp); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}

	return cn, nil
}

func (c *Client) auth(tp *textproto.Conn) error {
	id, err := tp.Cmd("AUTHINFO USER %s", c.cfg.Username)
	if err != nil {
		return err
	}
	tp.StartResponse(id)
	code, msg, err := tp.ReadCodeLine(381)
	tp.EndResponse(id)
	if err != nil {
		// 281 means no password required (unlikely but valid)
		if code == 281 {
			return nil
		}
		return fmt.Errorf("AUTHINFO USER: %d %s: %w", code, msg, err)
	}

	id, err = tp.Cmd("AUTHINFO PASS %s", c.cfg.Password)
	if err != nil {
		return err
	}
	tp.StartResponse(id)
	code, msg, err = tp.ReadCodeLine(281)
	tp.EndResponse(id)
	if err != nil {
		return fmt.Errorf("AUTHINFO PASS: %d %s: %w", code, msg, err)
	}

	return nil
}

func (c *Client) bodyOnConn(ctx context.Context, cn *conn, messageID string) ([]byte, error) {
	// Set deadline from context
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(60 * time.Second)
	}
	cn.raw.SetDeadline(deadline)
	defer cn.raw.SetDeadline(time.Time{})

	// Send BODY command
	id, err := cn.tp.Cmd("BODY <%s>", messageID)
	if err != nil {
		return nil, fmt.Errorf("BODY cmd: %w", err)
	}

	cn.tp.StartResponse(id)
	defer cn.tp.EndResponse(id)

	// Read response code
	code, msg, err := cn.tp.ReadCodeLine(222)
	if err != nil {
		if code == 430 {
			return nil, &ArticleNotFoundError{MessageID: messageID}
		}
		return nil, fmt.Errorf("BODY response: %d %s: %w", code, msg, err)
	}

	// Read dot-terminated body
	body, err := readDotBody(cn.tp.R)
	if err != nil {
		// Partial read leaves textproto in broken state — mark connection as dead
		cn.closed = true
		return nil, fmt.Errorf("read body: %w", err)
	}

	return body, nil
}

// readDotBody reads a dot-terminated text block from the NNTP server.
// Lines beginning with a dot have the dot removed (dot-stuffing).
// The final ".\r\n" line signals the end.
func readDotBody(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		// Trim trailing \r\n
		line = bytes.TrimRight(line, "\r\n")

		// Check for terminator: single dot
		if len(line) == 1 && line[0] == '.' {
			break
		}

		// Dot-unstuffing: remove leading dot if present
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}

		buf.Write(line)
		buf.WriteByte('\n')
	}

	return buf.Bytes(), nil
}

func (c *Client) acquire(ctx context.Context) (*conn, error) {
	for {
		select {
		case cn := <-c.pool:
			if cn.closed {
				c.mu.Lock()
				c.open--
				c.mu.Unlock()
				continue // discard and try next
			}
			return cn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, fmt.Errorf("nntp: client closed")
		}
	}
}

func (c *Client) release(cn *conn) {
	if cn == nil || cn.closed {
		return
	}
	select {
	case c.pool <- cn:
	default:
		// Pool full, close the connection
		c.closeConn(cn)
	}
}

func (c *Client) discard(cn *conn) {
	c.closeConn(cn)
	c.mu.Lock()
	c.open--
	c.mu.Unlock()
}

func (c *Client) reconnect(ctx context.Context, old *conn) (*conn, error) {
	c.closeConn(old)
	newConn, err := c.dial(ctx)
	if err != nil {
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
		return nil, err
	}
	return newConn, nil
}

func (c *Client) closeConn(cn *conn) {
	if cn == nil || cn.closed {
		return
	}
	cn.closed = true
	// Best-effort QUIT
	cn.tp.Cmd("QUIT")
	cn.raw.Close()
}

// ArticleNotFoundError is returned when the server responds with 430.
type ArticleNotFoundError struct {
	MessageID string
}

func (e *ArticleNotFoundError) Error() string {
	return fmt.Sprintf("nntp: article not found: %s", e.MessageID)
}

// Status returns a human-readable status string.
func (c *Client) Status() string {
	c.mu.Lock()
	open := c.open
	c.mu.Unlock()

	pooled := len(c.pool)
	return fmt.Sprintf("%d connections (%d pooled) to %s:%d", open, pooled, c.cfg.Host, c.cfg.Port)
}
