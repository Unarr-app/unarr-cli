package vpn

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun/netstack"
)

type readResult struct {
	data string
	err  error
}

func localUDP(t *testing.T) net.PacketConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return c
}

func readAsync(w *tunnelPacketConn) <-chan readResult {
	out := make(chan readResult, 1)
	go func() {
		b := make([]byte, 64)
		n, _, err := w.ReadFrom(b)
		out <- readResult{string(b[:n]), err}
	}()
	return out
}

func sendTo(t *testing.T, dst net.Addr, msg string) {
	t.Helper()
	src := localUDP(t)
	defer src.Close()
	if _, err := src.WriteTo([]byte(msg), dst); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func waitRead(t *testing.T, ch <-chan readResult) readResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFrom did not return")
		return readResult{}
	}
}

func assertBlocked(t *testing.T, ch <-chan readResult) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-ch:
		t.Fatalf("ReadFrom returned while it should wait: %+v", r)
	default:
	}
}

var anyUDP = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

// The reader anacrolix parks on a tracker socket must survive a rebind: the old
// socket closing is not an error it sees, and it keeps reading on the new one.
// Before this, the read error made anacrolix close the tracker client for good.
func TestTunnelPacketConnReaderSurvivesRebind(t *testing.T) {
	first, second := localUDP(t), localUDP(t)
	w := newTunnelPacketConn(connHooks{})
	defer w.Close()
	w.rebind(first)

	got := readAsync(w)
	time.Sleep(50 * time.Millisecond) // let the reader block on the first socket
	w.rebind(second)
	sendTo(t, second.LocalAddr(), "after-reconnect")

	r := waitRead(t, got)
	if r.err != nil || r.data != "after-reconnect" {
		t.Fatalf("ReadFrom = %q, %v; want the packet sent to the new socket", r.data, r.err)
	}
	if _, err := first.WriteTo([]byte("x"), second.LocalAddr()); err == nil {
		t.Error("the replaced socket must be closed")
	}
}

// While the tunnel is down writes fail closed and the reader waits for a socket
// instead of erroring out.
func TestTunnelPacketConnDownFailsClosedThenRecovers(t *testing.T) {
	w := newTunnelPacketConn(connHooks{})
	defer w.Close()

	if _, err := w.WriteTo([]byte("x"), anyUDP); !errors.Is(err, errTunnelDown) {
		t.Fatalf("WriteTo while down = %v, want errTunnelDown", err)
	}
	got := readAsync(w)
	assertBlocked(t, got)

	up := localUDP(t)
	w.rebind(up)
	sendTo(t, up.LocalAddr(), "up")
	if r := waitRead(t, got); r.err != nil || r.data != "up" {
		t.Fatalf("ReadFrom after bind = %q, %v", r.data, r.err)
	}
}

// A socket that failed to open during a swap is retried on the next write, so a
// healthy tunnel's tracker does not stay mute until the next reconnect.
func TestTunnelPacketConnWriteReopensMissingSocket(t *testing.T) {
	var w *tunnelPacketConn
	sink := localUDP(t)
	defer sink.Close()
	reopens := 0
	w = newTunnelPacketConn(connHooks{reopen: func() {
		reopens++
		w.rebind(localUDP(t))
	}})
	defer w.Close()

	if _, err := w.WriteTo([]byte("hello"), sink.LocalAddr()); err != nil {
		t.Fatalf("WriteTo with a reopenable socket = %v", err)
	}
	if reopens != 1 {
		t.Fatalf("reopen calls = %d, want 1", reopens)
	}
}

// flakyConn closes itself mid-write the way a concurrent rebind does.
type flakyConn struct {
	net.PacketConn
	onWrite func()
}

func (f *flakyConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	f.onWrite()
	return 0, net.ErrClosed
}

// A write that lands on a socket a rebind just closed is retried on the new one.
func TestTunnelPacketConnWriteRetriesAfterConcurrentRebind(t *testing.T) {
	w := newTunnelPacketConn(connHooks{})
	defer w.Close()
	next := localUDP(t)
	w.rebind(&flakyConn{PacketConn: localUDP(t), onWrite: func() { w.rebind(next) }})

	sink := localUDP(t)
	defer sink.Close()
	if n, err := w.WriteTo([]byte("hello"), sink.LocalAddr()); err != nil || n != 5 {
		t.Fatalf("WriteTo across a rebind = %d, %v; want it retried on the new socket", n, err)
	}
}

// Close is the one read error that must surface, and it unregisters the socket.
func TestTunnelPacketConnCloseUnblocksReader(t *testing.T) {
	unregistered := false
	w := newTunnelPacketConn(connHooks{unregister: func() { unregistered = true }})
	w.rebind(localUDP(t))

	got := readAsync(w)
	time.Sleep(50 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r := waitRead(t, got); !errors.Is(r.err, net.ErrClosed) {
		t.Fatalf("ReadFrom after Close = %v, want net.ErrClosed", r.err)
	}
	if !unregistered {
		t.Error("Close must unregister the socket from its tunnel")
	}
	late := localUDP(t)
	w.rebind(late) // a swap racing Close must not resurrect it
	if _, err := late.WriteTo([]byte("x"), late.LocalAddr()); err == nil {
		t.Error("a socket handed to a closed wrapper must be closed")
	}
}

// A deadline the caller set is carried over to the socket a rebind installs.
func TestTunnelPacketConnKeepsDeadlineAcrossRebind(t *testing.T) {
	w := newTunnelPacketConn(connHooks{})
	defer w.Close()
	w.rebind(localUDP(t))
	if err := w.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	w.rebind(localUDP(t))
	r := waitRead(t, readAsync(w))
	if !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadFrom = %v, want the deadline to still apply", r.err)
	}
}

// With the tunnel down the read deadline still holds — including one set while
// the reader is already waiting.
func TestTunnelPacketConnReadDeadlineWhileDown(t *testing.T) {
	w := newTunnelPacketConn(connHooks{})
	defer w.Close()

	got := readAsync(w)
	assertBlocked(t, got) // no deadline: waits for a socket
	if err := w.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline while down: %v", err)
	}
	if r := waitRead(t, got); !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadFrom while down = %v, want os.ErrDeadlineExceeded", r.err)
	}
}

func netstackInner(t *testing.T, addr string) *tunnelInner {
	t.Helper()
	_, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{netip.MustParseAddr(addr)},
		[]netip.Addr{netip.MustParseAddr("1.1.1.1")},
		1420,
	)
	if err != nil {
		t.Fatalf("netstack: %v", err)
	}
	return &tunnelInner{net: tnet, startedAt: time.Now()}
}

func liveSocket(w *tunnelPacketConn) net.PacketConn {
	c, _, _, _ := w.snapshot()
	return c
}

// swapInner (the heart of Reconnect) moves every live tracker socket onto the
// new device, and a swap to nil leaves them failing closed.
func TestTunnelSwapInnerRebindsTrackerSockets(t *testing.T) {
	tun := &Tunnel{}
	tun.inner.Store(netstackInner(t, "10.0.0.2"))

	pc, err := tun.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	w := pc.(*tunnelPacketConn)
	before := liveSocket(w)
	if before == nil {
		t.Fatal("ListenPacket on a live tunnel must bind a socket")
	}

	tun.swapInner(netstackInner(t, "10.0.0.3"))
	after := liveSocket(w)
	if after == nil || after == before {
		t.Fatal("a swap must install a socket on the new device")
	}

	tun.swapInner(nil)
	if _, err := w.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 6969}); !errors.Is(err, errTunnelDown) {
		t.Fatalf("WriteTo with no device = %v, want errTunnelDown", err)
	}

	// The device is back without a swap reaching this socket (an open that
	// failed): the reopen hook a write fires binds it. (Called directly: with no
	// WireGuard device draining it, netstack blocks a real send; the write → hook
	// path is TestTunnelPacketConnWriteReopensMissingSocket.)
	tun.inner.Store(netstackInner(t, "10.0.0.4"))
	tun.reopen(w)
	if liveSocket(w) == nil {
		t.Fatal("reopen with a live device must bind the socket")
	}

	_ = w.Close()
	if n := len(tun.registered()); n != 0 {
		t.Errorf("closed socket still registered (%d)", n)
	}
}

// Close ends every tracker socket's reader and refuses new sockets: a closed
// tunnel is not a tunnel that is temporarily down.
func TestTunnelCloseEndsTrackerSockets(t *testing.T) {
	tun := &Tunnel{}
	tun.inner.Store(netstackInner(t, "10.0.0.5"))
	pc, err := tun.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	got := readAsync(pc.(*tunnelPacketConn))
	time.Sleep(50 * time.Millisecond)

	tun.Close()
	if r := waitRead(t, got); !errors.Is(r.err, net.ErrClosed) {
		t.Fatalf("reader after Tunnel.Close = %v, want net.ErrClosed", r.err)
	}
	if _, err := tun.ListenPacket("udp", ":0"); err == nil {
		t.Error("ListenPacket on a closed tunnel must error")
	}
	if err := tun.Reconnect("irrelevant"); err == nil {
		t.Error("Reconnect on a closed tunnel must error")
	}
}
