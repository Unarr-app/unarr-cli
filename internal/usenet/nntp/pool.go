package nntp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// redialBackoff spaces acquire's dials while the provider refuses new
// connections and the live ones are all busy. A provider that caps connections
// keeps counting one we just closed for a while, so it can answer "too many
// connections" right after a slot was freed; dialling again at once would spin.
const redialBackoff = time.Second

// errRedialDeferred: a redial failed, but other connections are live and one
// will be released, so acquire waits for it instead of failing the caller.
var errRedialDeferred = errors.New("nntp: redial deferred")

// acquire returns an idle pooled connection, dials one into a slot a retired
// connection gave up, or waits for a release. It never waits on a pool that has
// nothing left to give back (all connections gone), and never fails a caller
// just because a redial was refused while other connections are still live.
func (c *Client) acquire(ctx context.Context) (*conn, error) {
	for {
		if cn, err := c.takeIdle(ctx); cn != nil || err != nil {
			return cn, err
		}
		var backoff <-chan time.Time // nil: wait for a release or a freed slot
		if c.reserveSlot() {
			cn, err := c.dialReserved(ctx)
			if !errors.Is(err, errRedialDeferred) {
				return cn, err
			}
			backoff = time.After(redialBackoff)
		}
		if cn, err := c.awaitConn(ctx, backoff); cn != nil || err != nil {
			return cn, err
		}
	}
}

// takeIdle returns an idle pooled connection without blocking, or the reason
// the caller must not get one. Both nil: nothing idle.
func (c *Client) takeIdle(ctx context.Context) (*conn, error) {
	select {
	case <-c.done:
		return nil, errClientClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case cn := <-c.pool:
		return cn, nil
	default:
		return nil, nil
	}
}

// dialReserved dials into a slot reserveSlot counted. On failure the reservation
// is given back, and the error is errRedialDeferred while other connections are
// still live; only a pool with nothing left fails with the dial error.
func (c *Client) dialReserved(ctx context.Context) (*conn, error) {
	cn, err := c.dial(ctx)
	if err == nil {
		return cn, nil
	}
	if c.unreserve() > 0 {
		return nil, errRedialDeferred
	}
	return nil, fmt.Errorf("nntp: redial: %w", err)
}

// awaitConn blocks until a connection is released to the pool (returned), or
// until acquire should look again: a slot was freed or, after a failed redial
// (backoff non-nil), the backoff ran out. A failed redial ignores freed slots, so
// a provider refusing every dial cannot turn the wake-ups into a dial loop.
func (c *Client) awaitConn(ctx context.Context, backoff <-chan time.Time) (*conn, error) {
	freed := c.freed
	if backoff != nil {
		freed = nil
	}
	select {
	case cn := <-c.pool:
		return cn, nil
	case <-freed:
	case <-backoff:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errClientClosed
	}
	return nil, nil
}

// release returns a healthy connection to the pool. A broken connection, one the
// pool has no room for, or any connection of a closed client is retired instead.
func (c *Client) release(cn *conn) {
	if cn == nil || cn.retired {
		return
	}
	select {
	case <-c.done:
		c.retire(cn)
		return
	default:
	}
	if cn.broken {
		c.retire(cn)
		return
	}
	select {
	case c.pool <- cn:
	default:
		c.retire(cn)
	}
}

// replace dials a fresh connection to take over broken cn's slot, then retires
// cn. Dialling first keeps open from dipping while the dial runs, which would let
// acquire dial a surplus connection into the apparently free slot.
func (c *Client) replace(ctx context.Context, cn *conn) (*conn, error) {
	fresh, err := c.dial(ctx)
	if err == nil {
		c.adopt()
	}
	c.retire(cn)
	return fresh, err
}

// retire takes cn out of service for good: closes it and frees its slot, exactly
// once per connection whichever path gets there first. Every way a connection
// leaves (broken, pool full, client closed, Connect rollback) goes through here.
func (c *Client) retire(cn *conn) {
	if cn == nil || cn.retired {
		return
	}
	cn.retired = true
	if !cn.broken {
		cn.tp.Cmd("QUIT") // best effort; pointless on a broken connection
	}
	cn.raw.Close()
	c.freeSlot()
}

// adopt counts a freshly dialled connection.
func (c *Client) adopt() {
	c.mu.Lock()
	c.open++
	c.mu.Unlock()
}

// reserveSlot counts a connection about to be dialled into a free slot,
// reporting false when the pool is already at MaxConnections.
func (c *Client) reserveSlot() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open >= c.cfg.MaxConnections {
		return false
	}
	c.open++
	return true
}

// unreserve gives back a reservation whose dial failed and returns how many
// connections remain. It wakes a waiter only when none remain: that waiter has
// no release to wait for and must dial (and fail) itself rather than hang.
func (c *Client) unreserve() int {
	c.mu.Lock()
	c.open--
	open := c.open
	c.mu.Unlock()
	if open == 0 {
		c.wake()
	}
	return open
}

// freeSlot uncounts a retired connection and wakes one caller waiting in acquire.
func (c *Client) freeSlot() {
	c.mu.Lock()
	c.open--
	c.mu.Unlock()
	c.wake()
}

func (c *Client) wake() {
	select {
	case c.freed <- struct{}{}:
	default:
	}
}
