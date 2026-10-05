package vpn

import (
	"errors"
	"log"
	"net"
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
	// bound is closed (and replaced) every time conn changes, to wake a reader
	// waiting for a socket to exist.
	bound  chan struct{}
	closed bool
	// Deadlines are re-applied to every new socket so a rebind does not drop them.
	readDeadline, writeDeadline time.Time
	// onClose unregisters the wrapper from its tunnel.
	onClose func()
}

func newTunnelPacketConn(onClose func()) *tunnelPacketConn {
	return &tunnelPacketConn{bound: make(chan struct{}), onClose: onClose}
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
		applyDeadlines(nc, w.readDeadline, w.writeDeadline)
	}
	old := w.conn
	w.conn = nc
	close(w.bound)
	w.bound = make(chan struct{})
	w.mu.Unlock()
	if old != nil {
		_ = old.Close() // its reader moves to nc (see ReadFrom)
	}
}

// current returns the live socket (nil while down), the channel that signals
// the next change, and net.ErrClosed once the wrapper is closed.
func (w *tunnelPacketConn) current() (net.PacketConn, <-chan struct{}, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, net.ErrClosed
	}
	return w.conn, w.bound, nil
}

// replaced reports whether c is no longer the live socket (rebound or closed).
func (w *tunnelPacketConn) replaced(c net.PacketConn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed || w.conn != c
}

// ReadFrom reads from the live socket. A read error caused by a rebind is not
// surfaced: anacrolix closes the whole tracker client on the first read error,
// which is exactly the permanent death this type exists to prevent.
func (w *tunnelPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c, bound, err := w.current()
		if err != nil {
			return 0, nil, err
		}
		if c == nil {
			<-bound // tunnel down: wait for a socket (or Close)
			continue
		}
		n, addr, err := c.ReadFrom(b)
		if err == nil || !w.replaced(c) {
			return n, addr, err
		}
	}
}

// WriteTo writes on the live socket, and fails closed while the tunnel is down.
func (w *tunnelPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c, _, err := w.current()
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, errTunnelDown
	}
	return c.WriteTo(b, addr)
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
	close(w.bound)
	w.mu.Unlock()
	if w.onClose != nil {
		w.onClose()
	}
	if old != nil {
		return old.Close()
	}
	return nil
}

// LocalAddr is the live socket's address, or an unspecified UDP address while
// the tunnel is down.
func (w *tunnelPacketConn) LocalAddr() net.Addr {
	if c, _, _ := w.current(); c != nil {
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
	}
	if write != nil {
		w.writeDeadline = *write
	}
	c, rd, wd := w.conn, w.readDeadline, w.writeDeadline
	w.mu.Unlock()
	if c != nil {
		return applyDeadlines(c, rd, wd)
	}
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
// up, instead of failing the tracker client's construction for good.
func (t *Tunnel) ListenPacket(_ string, _ string) (net.PacketConn, error) {
	if t == nil {
		return nil, errTunnelDown
	}
	var w *tunnelPacketConn
	w = newTunnelPacketConn(func() { t.unregister(w) })

	// bindMu orders this against Reconnect: a swap either happens before the
	// rebind below (and the load sees the new device) or after the register
	// (and rebindAll reaches this wrapper). Never a socket on a dead device.
	t.bindMu.Lock()
	defer t.bindMu.Unlock()
	t.register(w)
	w.rebind(t.listenUDP(t.inner.Load()))
	return w, nil
}

// listenUDP opens a socket on in's netstack, or nil when there is no device or
// the stack refuses (the wrapper then fails closed until the next rebind).
func (t *Tunnel) listenUDP(in *tunnelInner) net.PacketConn {
	if in == nil || in.net == nil {
		return nil
	}
	c, err := in.net.ListenUDP(&net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Printf("[vpn] open tunnel UDP socket: %v - tracker announces paused until the next reconnect", err)
		return nil
	}
	return c
}

// swapInner installs in as the live device and re-binds every tunnel socket to
// it, returning the previous device for the caller to close AFTER the sockets
// have moved. in may be nil (Close).
func (t *Tunnel) swapInner(in *tunnelInner) *tunnelInner {
	t.bindMu.Lock()
	defer t.bindMu.Unlock()
	old := t.inner.Swap(in)

	t.connsMu.Lock()
	conns := make([]*tunnelPacketConn, 0, len(t.conns))
	for w := range t.conns {
		conns = append(conns, w)
	}
	t.connsMu.Unlock()

	for _, w := range conns {
		w.rebind(t.listenUDP(in))
	}
	if in != nil && len(conns) > 0 {
		log.Printf("[vpn] moved %d tracker socket(s) to the new tunnel", len(conns))
	}
	return old
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
