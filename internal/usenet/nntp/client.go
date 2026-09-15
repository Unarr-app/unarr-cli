package nntp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"
)

// dialTimeout bounds each phase of opening a connection: TCP connect plus TLS
// handshake (the dialer), then greeting plus AUTHINFO (a connection deadline).
const dialTimeout = 30 * time.Second

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
//
// Accounting: open counts the connections that exist, idle in the pool or held
// by a Body call. It grows only when a dial succeeds (Connect, acquire's redial of
// a lost slot, replace) and shrinks only in retire, exactly once per connection.
// A connection whose state became unknown is never put back in the pool.
type Client struct {
	cfg  Config
	pool chan *conn
	mu   sync.Mutex
	open int
	done chan struct{} // closed on Close()
	// freed wakes callers blocked in acquire when a slot is retired, so they redial
	// it instead of waiting for a release that will never come.
	freed chan struct{}

	// tlsOverrideStale is set once a handshake proved cfg.TLSServerName does not
	// match the server's certificate while Host does (see dialTLS).
	tlsOverrideStale atomic.Bool
	tlsFallbackLog   sync.Once
	// rootCAs overrides the system trust store. Unexported: only this package's
	// tests set it, to trust a locally generated CA.
	rootCAs *x509.CertPool
	// handshakeTimeout bounds greeting plus AUTHINFO; 0 means dialTimeout. Only
	// this package's tests shorten it.
	handshakeTimeout time.Duration
}

// conn is a single NNTP connection. Only its current holder touches it.
type conn struct {
	tp  *textproto.Conn
	raw net.Conn
	// broken: a command failed with anything but a clean 430/423, so where the
	// connection stands in its response stream is unknown — a timed-out BODY may
	// still be answered later, and reading that reply as the NEXT command's answer
	// would hand one article's bytes out under another's message-id. Never reused.
	broken bool
	// retired: closed and removed from open (see retire).
	retired bool
}

var errClientClosed = errors.New("nntp: client closed")

// NewClient creates a new NNTP client (does not connect yet).
func NewClient(cfg Config) *Client {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 10
	}
	return &Client{
		cfg:   cfg,
		pool:  make(chan *conn, cfg.MaxConnections),
		done:  make(chan struct{}),
		freed: make(chan struct{}, cfg.MaxConnections),
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
		cn, err := c.dial(ctx)
		if err != nil {
			// Close any connections we already opened, but keep client reusable
			c.drainPool()
			return fmt.Errorf("nntp: connect %d/%d: %w", i+1, c.cfg.MaxConnections, err)
		}
		c.adopt()
		c.pool <- cn
	}
	return nil
}

// drainPool retires every idle connection without closing the done channel.
func (c *Client) drainPool() {
	for {
		select {
		case cn := <-c.pool:
			c.retire(cn)
		default:
			return
		}
	}
}

// Body downloads the body of an NNTP article by message-ID and returns the raw
// (typically yEnc encoded) bytes.
//
// A 430/423 comes back as *ArticleNotFoundError on the connection that answered
// it, which stays in the pool. Any other failure leaves that connection broken: it
// is retired and the BODY is re-issued ONCE on a freshly dialled connection, whose
// outcome is what the caller gets. The retry is unconditional on purpose. A
// timeout on a pooled connection says nothing about the article — an idle
// connection silently dropped by a NAT, a firewall or a host sleep accepts the
// write and never answers — while a fresh connection has just proven itself alive
// with its greeting and AUTHINFO. So *StallError (IsStalled) only ever reports an
// article that stalled on a live connection.
func (c *Client) Body(ctx context.Context, messageID string) ([]byte, error) {
	return c.BodyInto(ctx, messageID, nil)
}

// BodyInto is Body reading into buf's backing array (from buf[:0]) and growing
// it only if the body does not fit, so a caller that knows an article's size
// pays for one allocation instead of a buffer doubled a dozen times. The result
// aliases buf when it fit; buf's contents are overwritten either way.
func (c *Client) BodyInto(ctx context.Context, messageID string, buf []byte) ([]byte, error) {
	cn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	data, err := c.bodyOnConn(ctx, cn, messageID, buf)
	if !cn.broken {
		// Success, or a clean 430/423: the connection is in sync and reusable.
		c.release(cn)
		return data, err
	}

	// The dial deliberately ignores the caller's cancellation (it stays bounded
	// by dialTimeout per phase): the connection being replaced is a POOL slot, not
	// the caller's, and a reconnect that fails because a player closed its range
	// would drop that slot for good.
	cn2, dialErr := c.replace(context.WithoutCancel(ctx), cn)
	if dialErr != nil {
		// A transport failure, never an article verdict: neither error is wrapped,
		// so neither a dial timeout nor the dead connection's timeout reads as a stall.
		return nil, fmt.Errorf("nntp: body failed (%v) and reconnect failed: %v", err, dialErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The slot is restored; the caller is gone, so do not issue its BODY.
		c.release(cn2)
		return nil, fmt.Errorf("nntp: body cancelled: %w (original: %v)", ctxErr, err)
	}
	data, err = c.bodyOnConn(ctx, cn2, messageID, buf)
	c.release(cn2)
	return data, err
}

// ActiveConnections returns the number of open connections.
func (c *Client) ActiveConnections() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open
}

// MaxConcurrency reports how many Body calls can run in parallel before they
// block on connection acquisition — i.e. the pool size. Concurrent header-probe
// fan-out uses this to size its worker pool to the connections actually available.
func (c *Client) MaxConcurrency() int {
	return c.cfg.MaxConnections
}

// Close shuts down all connections in the pool. Connections held by a Body call
// are retired when it releases them.
func (c *Client) Close() error {
	select {
	case <-c.done:
		return nil // already closed
	default:
		close(c.done)
	}
	c.drainPool()
	return nil
}

// --- Internal ---

func (c *Client) dial(ctx context.Context) (*conn, error) {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)

	dialer := &net.Dialer{Timeout: dialTimeout}

	var rawConn net.Conn
	var err error

	if c.cfg.SSL {
		// Verified against TLSServerName when set, falling back to Host only on a
		// certificate name mismatch (see dialTLS).
		rawConn, err = c.dialTLS(ctx, dialer, addr)
	} else {
		rawConn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// The dialer bounds only connect and TLS handshake. A server, or a proxy in
	// front of one, that accepts and never greets would otherwise hold this dial
	// forever — and with it the Body call and its pool slot, since replace dials
	// with the caller's cancellation removed.
	rawConn.SetDeadline(c.handshakeDeadline(ctx))
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

	rawConn.SetDeadline(time.Time{})
	return cn, nil
}

// handshakeDeadline is the deadline for greeting plus AUTHINFO: handshakeTimeout
// (dialTimeout by default), or ctx's deadline when that is earlier.
func (c *Client) handshakeDeadline(ctx context.Context) time.Time {
	timeout := c.handshakeTimeout
	if timeout <= 0 {
		timeout = dialTimeout
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
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

// bodyOnConn issues BODY on cn. Anything but success or a clean 430/423 marks cn
// broken: a timeout, a reset, a partial body or an unexpected reply all leave the
// connection's position in its response stream unknown.
func (c *Client) bodyOnConn(ctx context.Context, cn *conn, messageID string, buf []byte) ([]byte, error) {
	body, err := c.bodyExchange(ctx, cn, messageID, buf)
	if err != nil && !IsArticleMissing(err) {
		cn.broken = true
	}
	return body, err
}

func (c *Client) bodyExchange(ctx context.Context, cn *conn, messageID string, buf []byte) ([]byte, error) {
	// Set deadline from context
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(60 * time.Second)
	}
	status, stallBounded := statusDeadline(ctx, deadline)
	cn.raw.SetDeadline(status)
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
		// 430 is the RFC 3977 answer for an unknown message-id; 423 is the
		// by-number code some servers send for it anyway. Both are final.
		if code == 430 || code == 423 {
			return nil, &ArticleNotFoundError{MessageID: messageID, Code: code}
		}
		if stallBounded && isNetTimeout(err) {
			return nil, &StallError{MessageID: messageID, Err: err}
		}
		return nil, fmt.Errorf("BODY response: %d %s: %w", code, msg, err)
	}
	// The stall bound covers the status line only: the body keeps the command deadline.
	cn.raw.SetDeadline(deadline)

	// Read dot-terminated body
	body, err := readDotBody(cn.tp.R, buf)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return body, nil
}

// readDotBody reads a dot-terminated text block from the NNTP server, appending
// it to buf[:0]. Lines beginning with a dot have the dot removed (dot-stuffing),
// each line is stored with a bare '\n', and the final ".\r\n" line signals the
// end. Lines are read in place (ReadSlice), so the body costs no allocation
// beyond growing buf.
func readDotBody(r *bufio.Reader, buf []byte) ([]byte, error) {
	out := buf[:0]
	for {
		start := len(out)
		var err error
		if out, err = appendLine(r, out); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}

		line := bytes.TrimRight(out[start:], "\r\n")
		if len(line) == 1 && line[0] == '.' {
			return out[:start], nil // terminator
		}
		if len(line) > 0 && line[0] == '.' {
			line = line[1:] // dot-unstuffing
		}
		out = append(append(out[:start], line...), '\n')
	}
}

// appendLine appends the next line, '\n' included, to out. A line longer than
// the reader's buffer arrives in several fragments. On an error the partial line
// is dropped and the error returned.
func appendLine(r *bufio.Reader, out []byte) ([]byte, error) {
	start := len(out)
	for {
		frag, err := r.ReadSlice('\n')
		out = append(out, frag...)
		if err == nil {
			return out, nil
		}
		if err != bufio.ErrBufferFull {
			return out[:start], err
		}
	}
}

// ArticleNotFoundError is returned when the server responds with 430 (or 423).
// See IsArticleMissing.
type ArticleNotFoundError struct {
	MessageID string
	Code      int // 430 or 423; 0 when constructed without a server answer
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
