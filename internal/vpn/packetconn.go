package vpn

import (
	"errors"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// errTunnelDown is what a tunnel socket answers while no device is up: the
// write fails instead of falling back to the clear net (no IP leak).
var errTunnelDown = errors.New("vpn tunnel is down")

// tunnelPacketConn is a UDP socket that outlives a Reconnect.
//
// anacrolix creates ONE UDP socket per tracker URL — the first time it meets
// that tracker — and caches the client for the whole life of the torrent
// client (regularTrackerAnnounceDispatcher.trackerClients). A plain netstack
// socket dies with the device Reconnect closes, so after the first reconnect
// every UDP tracker announce went into a closed socket until the daemon was
// restarted: no peers, every new torrent stuck "waiting for metadata" with 30+
// seeders while debrid kept working (PROD 2026-10-04, 10 days up after 13
// reconnects on 26-09; a fresh one-shot on the same tunnel found 37 peers).
//
// This wrapper keeps the identity anacrolix holds and swaps the socket under
// it: Reconnect re-binds every live wrapper to the new netstack and closes the
// old socket, which unblocks the reader parked on it, and the reader carries on
// with the new one.
type tunnelPacketConn struct {
	mu   sync.Mutex
	conn net.PacketConn // current socket; nil while the tunnel is down
	// bound is closed (and replaced) whenever a waiting reader must look again:
	// a new socket, Close, or a new read deadline.
	bound  chan struct{}
	closed bool
	// Deadlines are re-applied to every new socket so a rebind does not drop them.
	readDeadline, writeDeadline time.Time
	hooks                       connHooks
}

// connHooks ties a socket to its tunnel. Both are optional (nil in unit tests).
type connHooks struct {
	// unregister drops the socket from its tunnel on Close.
	unregister func()
	// reopen tries to bind a socket right now when there is none: a failed
	// open during a swap must not leave a healthy tunnel's tracker mute until
	// the next reconnect.
	reopen func()
}

func newTunnelPacketConn(hooks connHooks) *tunnelPacketConn {
	return &tunnelPacketConn{bound: make(chan struct{}), hooks: hooks}
}

// wakeLocked tells waiting readers to look again. Caller holds w.mu.
func (w *tunnelPacketConn) wakeLocked() {
	close(w.bound)
	w.bound = make(chan struct{})
}

// rebind installs nc (nil = tunnel down) as the live socket and closes the
// previous one. A wrapper already closed just closes nc.
func (w *tunnelPacketConn) rebind(nc net.PacketConn) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		if nc != nil {
			_ = nc.Close() // nobody will ever read it
		}
		return
	}
	if nc != nil {
		_ = applyDeadlines(nc, w.readDeadline, w.writeDeadline)
	}
	old := w.conn
	w.conn = nc
	w.wakeLocked()
	w.mu.Unlock()
	if old != nil {
		_ = old.Close() // its reader moves to nc (see ReadFrom)
	}
}

// snapshot is the state a reader or writer acts on: the live socket (nil while
// down), the channel that signals the next change, the read deadline, and
// net.ErrClosed once the wrapper is closed.
func (w *tunnelPacketConn) snapshot() (net.PacketConn, <-chan struct{}, time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, time.Time{}, net.ErrClosed
	}
	return w.conn, w.bound, w.readDeadline, nil
}

// replaced reports whether c is no longer the live socket (rebound or closed).
func (w *tunnelPacketConn) replaced(c net.PacketConn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed || w.conn != c
}

// waitBound blocks until the socket state changes or the read deadline passes.
func waitBound(bound <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		<-bound
		return nil
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-bound:
		return nil
	case <-timer.C:
		return os.ErrDeadlineExceeded
	}
}

// ReadFrom reads from the live socket. A read error caused by a rebind is not
// surfaced: anacrolix closes the whole tracker client on the first read error,
// which is exactly the permanent death this type exists to prevent.
func (w *tunnelPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c, bound, deadline, err := w.snapshot()
		if err != nil {
			return 0, nil, err
		}
		if c == nil {
			// Tunnel down: wait for a socket, honouring the read deadline.
			if err := waitBound(bound, deadline); err != nil {
				return 0, nil, err
			}
			continue
		}
		n, addr, err := c.ReadFrom(b)
		if err == nil || !w.replaced(c) {
			return n, addr, err
		}
	}
}

// liveConn returns the live socket, trying to open one first when there is
// none; errTunnelDown when the tunnel still has no device.
func (w *tunnelPacketConn) liveConn() (net.PacketConn, error) {
	c, _, _, err := w.snapshot()
	if err != nil || c != nil {
		return c, err
	}
	if w.hooks.reopen != nil {
		w.hooks.reopen()
		if c, _, _, err = w.snapshot(); err != nil || c != nil {
			return c, err
		}
	}
	return nil, errTunnelDown
}

// WriteTo writes on the live socket, and fails closed while the tunnel is down.
// A write that lands on a socket a concurrent rebind just closed is retried on
// the new one instead of failing the announce.
func (w *tunnelPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	for {
		c, err := w.liveConn()
		if err != nil {
			return 0, err
		}
		n, err := c.WriteTo(b, addr)
		if err == nil || !w.replaced(c) {
			return n, err
		}
	}
}

// Close closes the live socket, wakes any waiting reader and unregisters the
// wrapper from its tunnel. Idempotent.
func (w *tunnelPacketConn) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	old := w.conn
	w.conn = nil
	w.wakeLocked()
	w.mu.Unlock()
	if w.hooks.unregister != nil {
		w.hooks.unregister()
	}
	if old != nil {
		return old.Close()
	}
	return nil
}

// LocalAddr is the live socket's address, or an unspecified UDP address while
// the tunnel is down.
func (w *tunnelPacketConn) LocalAddr() net.Addr {
	if c, _, _, _ := w.snapshot(); c != nil {
		return c.LocalAddr()
	}
	return &net.UDPAddr{IP: net.IPv4zero}
}

func (w *tunnelPacketConn) SetDeadline(t time.Time) error {
	return w.setDeadlines(&t, &t)
}

func (w *tunnelPacketConn) SetReadDeadline(t time.Time) error {
	return w.setDeadlines(&t, nil)
}

func (w *tunnelPacketConn) SetWriteDeadline(t time.Time) error {
	return w.setDeadlines(nil, &t)
}

func (w *tunnelPacketConn) setDeadlines(read, write *time.Time) error {
	w.mu.Lock()
	if read != nil {
		w.readDeadline = *read
		w.wakeLocked() // a reader waiting for a socket re-arms its timer
	}
	if write != nil {
		w.writeDeadline = *write
	}
	c, rd, wd := w.conn, w.readDeadline, w.writeDeadline
	w.mu.Unlock()
	if c == nil {
		return nil
	}
	if err := applyDeadlines(c, rd, wd); err != nil && !w.replaced(c) {
		return err
	}
	// A concurrent rebind closed c; the new socket got these deadlines there.
	return nil
}

func applyDeadlines(c net.PacketConn, read, write time.Time) error {
	return errors.Join(c.SetReadDeadline(read), c.SetWriteDeadline(write))
}

// --- registry on Tunnel ------------------------------------------------------

// ListenPacket adapts the tunnel's UDP for anacrolix TrackerListenPacket so UDP
// tracker announces also go through the VPN (no IP leak to trackers). The socket
// it returns survives Reconnect (see tunnelPacketConn). On a tunnel that is down
// it still returns one: writes fail closed until the supervisor brings a device
// up, instead of failing the tracker client's construction for good. Only a
// nil or Closed tunnel refuses.
func (t *Tunnel) ListenPacket(_ string, _ string) (net.PacketConn, error) {
	if t == nil || t.closed.Load() {
		return nil, errTunnelDown
	}
	var w *tunnelPacketConn
	w = newTunnelPacketConn(connHooks{
		unregister: func() { t.unregister(w) },
		reopen:     func() { t.reopen(w) },
	})

	// bindMu orders this against a swap: the swap either happens before the
	// rebind below (and the load sees the new device) or after the register
	// (and swapInner reaches this wrapper). Never a socket on a dead device.
	t.bindMu.Lock()
	defer t.bindMu.Unlock()
	t.register(w)
	w.rebind(t.listenUDP(t.inner.Load()))
	return w, nil
}

// reopen binds w to the live device if it has no socket yet (an earlier open
// failed). Serialised with swaps through bindMu.
func (t *Tunnel) reopen(w *tunnelPacketConn) {
	t.bindMu.Lock()
	defer t.bindMu.Unlock()
	if c, _, _, err := w.snapshot(); err != nil || c != nil {
		return
	}
	if nc := t.listenUDP(t.inner.Load()); nc != nil {
		w.rebind(nc)
	}
}

// listenUDP opens a socket on in's netstack, or nil when there is no device or
// the stack refuses (the wrapper then fails closed and retries on its next write).
func (t *Tunnel) listenUDP(in *tunnelInner) net.PacketConn {
	if in == nil || in.net == nil {
		return nil
	}
	c, err := in.net.ListenUDP(&net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Printf("[vpn] open tunnel UDP socket: %v - retrying on the next tracker announce", err)
		return nil
	}
	return c
}

// swapInner installs in as the live device and re-binds every tunnel socket to
// it, returning the previous device for the caller to close AFTER the sockets
// have moved. in may be nil (sockets then fail closed).
func (t *Tunnel) swapInner(in *tunnelInner) *tunnelInner {
	t.bindMu.Lock()
	defer t.bindMu.Unlock()
	old := t.inner.Swap(in)

	conns := t.registered()
	for _, w := range conns {
		w.rebind(t.listenUDP(in))
	}
	if in != nil && len(conns) > 0 {
		log.Printf("[vpn] moved %d tracker socket(s) to the new tunnel", len(conns))
	}
	return old
}

// closeSockets closes every registered socket (Tunnel.Close): their readers
// return net.ErrClosed instead of waiting forever for a device that will not
// come back.
func (t *Tunnel) closeSockets() {
	for _, w := range t.registered() {
		_ = w.Close()
	}
}

func (t *Tunnel) registered() []*tunnelPacketConn {
	t.connsMu.Lock()
	defer t.connsMu.Unlock()
	conns := make([]*tunnelPacketConn, 0, len(t.conns))
	for w := range t.conns {
		conns = append(conns, w)
	}
	return conns
}

func (t *Tunnel) register(w *tunnelPacketConn) {
	t.connsMu.Lock()
	defer t.connsMu.Unlock()
	if t.conns == nil {
		t.conns = make(map[*tunnelPacketConn]struct{})
	}
	t.conns[w] = struct{}{}
}

func (t *Tunnel) unregister(w *tunnelPacketConn) {
	t.connsMu.Lock()
	defer t.connsMu.Unlock()
	delete(t.conns, w)
}
