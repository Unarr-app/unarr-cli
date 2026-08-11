// Package vpn brings up an in-process WireGuard tunnel (userspace, via
// wireguard-go + gVisor netstack) and exposes it as a dialer so the BitTorrent
// client's peer/tracker traffic can be split-tunnelled through it — without
// touching the OS routing table or requiring root.
//
// The config is a standard WireGuard .conf fetched from the web
// (/api/internal/agent/vpn-config). Only the torrent client uses this tunnel;
// unarr's control-plane traffic (API, heartbeats) keeps using the normal net.
package vpn

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	// handshakeStaleAfter is how long since the last successful WireGuard
	// handshake we still consider the tunnel live. WireGuard rekeys ~every 120s
	// and persistent-keepalive is 25s, so a peer that has not handshaked in 180s
	// is almost certainly unreachable → the kill-switch should treat it as down.
	handshakeStaleAfter = 180 * time.Second
	// postUpGrace treats a freshly brought-up tunnel that has not completed its
	// first handshake yet (last_handshake_time_sec=0) as pending-healthy: the
	// first handshake only lands after the initial keepalive round-trip, so
	// without a grace window the very first torrent task would be wrongly blocked.
	postUpGrace = 45 * time.Second
)

// tunnelInner is the live userspace WireGuard device + its gVisor netstack. It is
// held behind an atomic pointer on Tunnel so Reconnect can hot-swap a fresh device
// in without rebuilding the long-lived torrent client (which dials through the
// stable Tunnel methods, not a captured *netstack.Net).
type tunnelInner struct {
	dev       *device.Device
	net       *netstack.Net
	startedAt time.Time // when this inner was brought up (for the post-Up handshake grace)
}

// Tunnel is a live userspace WireGuard tunnel. Wire the torrent client to its
// DialContext + ListenPacket so peer/tracker traffic is split-tunnelled through
// it. The methods are stable across a Reconnect (which atomically swaps the inner
// device), so a reconnected tunnel transparently keeps routing the existing client.
type Tunnel struct {
	inner atomic.Pointer[tunnelInner]
	// Endpoint is the resolved ip:port of the WireGuard server this tunnel exits
	// through — surfaced in `unarr vpn status`. Set at Up, or on the first
	// successful Reconnect if Up never produced one (a fail-closed tunnel that
	// failed to start has an empty Endpoint until the supervisor heals it). Once
	// non-empty it is left unchanged (same exit server). It is only ever written on
	// the supervisor goroutine (via Reconnect) after the startup read, so it is
	// safe to read without locking.
	Endpoint string
	// mu serializes Reconnect (the bring-up + atomic swap + close-old sequence).
	mu sync.Mutex
}

// Up parses a WireGuard .conf and brings up the tunnel in userspace.
func Up(confText string) (*Tunnel, error) {
	inner, endpoint, err := bringUp(confText)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{Endpoint: endpoint}
	t.inner.Store(inner)
	return t, nil
}

// bringUp parses confText and starts a fresh userspace WireGuard device+netstack,
// returning the inner state and the resolved exit endpoint. Shared by Up and
// Reconnect.
func bringUp(confText string) (*tunnelInner, string, error) {
	wc, err := parseConf(confText)
	if err != nil {
		return nil, "", err
	}

	mtu := wc.mtu
	if mtu == 0 {
		mtu = 1420
	}

	tunDev, tnet, err := netstack.CreateNetTUN(wc.addresses, wc.dns, mtu)
	if err != nil {
		return nil, "", fmt.Errorf("create netstack tun: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg-unarr "))
	if err := dev.IpcSet(wc.uapi()); err != nil {
		dev.Close()
		return nil, "", fmt.Errorf("wireguard ipc set: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, "", fmt.Errorf("wireguard up: %w", err)
	}

	return &tunnelInner{dev: dev, net: tnet, startedAt: time.Now()}, wc.endpoint, nil
}

// load returns the current inner, or nil for a nil / closed tunnel.
func (t *Tunnel) load() *tunnelInner {
	if t == nil {
		return nil
	}
	return t.inner.Load()
}

// DialContext dials through the tunnel. It is wired into the torrent client's
// TrackerDialContext/HTTPDialContext and (via NetworkDialer) its peer dialer, so
// every torrent dial is gated on a live tunnel: a nil/closed tunnel returns an
// error (fail-closed) rather than leaking the user's IP to the clear net.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	in := t.load()
	if in == nil {
		return nil, errors.New("vpn tunnel is down")
	}
	return in.net.DialContext(ctx, network, address)
}

// ListenPacket adapts the tunnel's UDP for anacrolix TrackerListenPacket so UDP
// tracker announces also go through the VPN (no IP leak to trackers).
func (t *Tunnel) ListenPacket(_ string, _ string) (net.PacketConn, error) {
	in := t.load()
	if in == nil {
		return nil, errors.New("vpn tunnel is down")
	}
	return in.net.ListenUDP(&net.UDPAddr{IP: net.IPv4zero, Port: 0})
}

// Close tears the tunnel down. Idempotent and nil-safe.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	if in := t.inner.Swap(nil); in != nil {
		in.dev.Close()
	}
}

// ipcGet reads a device's WireGuard UAPI state. It is a package-level seam (in
// the same spirit as the repo's other function-var seams) so tests can drive
// Healthy's IpcGet-error fail-closed branch without a live WireGuard device.
// Production wiring is dev.IpcGet verbatim.
var ipcGet = func(dev *device.Device) (string, error) { return dev.IpcGet() }

// Healthy reports whether the tunnel is up AND its peer handshaked recently. A
// nil/closed tunnel is unhealthy (fail-closed). Used by the torrent kill-switch
// gate and by the daemon's reconnect supervisor.
func (t *Tunnel) Healthy() bool {
	in := t.load()
	if in == nil {
		return false
	}
	ipc, err := ipcGet(in.dev)
	if err != nil {
		// A live device that cannot report its own state is treated as down so
		// the gate fails closed; log it rather than swallowing the error.
		log.Printf("[vpn] health check: IpcGet failed (%v) - treating tunnel as down", err)
		return false
	}
	return handshakeFresh(ipc, time.Now(), in.startedAt)
}

// handshakeFresh judges tunnel liveness from a WireGuard UAPI dump. A peer that
// handshaked within handshakeStaleAfter is live. Before the first handshake
// (last_handshake_time_sec=0) the tunnel is pending-healthy only within
// postUpGrace of startedAt — after that, no handshake means the peer is
// unreachable. Pure + deterministic so it is unit-testable without a live device.
func handshakeFresh(ipc string, now, startedAt time.Time) bool {
	var newest int64 // unix seconds of the most recent peer handshake
	sc := bufio.NewScanner(strings.NewReader(ipc))
	for sc.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "last_handshake_time_sec=")
		if !ok {
			continue
		}
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > newest {
			newest = secs
		}
	}
	if newest > 0 {
		return now.Sub(time.Unix(newest, 0)) <= handshakeStaleAfter
	}
	return now.Sub(startedAt) <= postUpGrace
}

// Reconnect brings up a fresh device+netstack from confText and atomically swaps
// it in, then closes the old device. The long-lived torrent client keeps dialing
// through the stable Tunnel methods, so all subsequent peer/tracker dials
// transparently use the new tunnel. An already-set exit Endpoint label is left
// unchanged (same exit server); when the tunnel was created without one (a
// fail-closed tunnel that failed to start at Up), the first successful Reconnect
// records the resolved endpoint so `unarr vpn status` can show the exit server.
func (t *Tunnel) Reconnect(confText string) error {
	if t == nil {
		return errors.New("nil tunnel")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	inner, endpoint, err := bringUp(confText)
	if err != nil {
		return err
	}
	if t.Endpoint == "" {
		t.Endpoint = endpoint
	}
	if old := t.inner.Swap(inner); old != nil {
		old.dev.Close()
	}
	return nil
}

// --- .conf parsing ----------------------------------------------------------

type wgConf struct {
	privateKey string // hex
	addresses  []netip.Addr
	dns        []netip.Addr
	mtu        int

	peerPublicKey string // hex
	presharedKey  string // hex (optional)
	endpoint      string // resolved ip:port
	allowedIPs    []string
	keepalive     int
}

func (w *wgConf) uapi() string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", w.privateKey)
	fmt.Fprintf(&b, "public_key=%s\n", w.peerPublicKey)
	if w.presharedKey != "" {
		fmt.Fprintf(&b, "preshared_key=%s\n", w.presharedKey)
	}
	if w.endpoint != "" {
		fmt.Fprintf(&b, "endpoint=%s\n", w.endpoint)
	}
	if w.keepalive > 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", w.keepalive)
	}
	for _, a := range w.allowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", a)
	}
	return b.String()
}

func b64ToHex(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("invalid base64 key: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key must be 32 bytes, got %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

func parseConf(text string) (*wgConf, error) {
	w := &wgConf{keepalive: 25}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		switch section {
		case "interface":
			switch key {
			case "privatekey":
				hexKey, err := b64ToHex(val)
				if err != nil {
					return nil, err
				}
				w.privateKey = hexKey
			case "address":
				for _, part := range strings.Split(val, ",") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					pfx, err := netip.ParsePrefix(part)
					if err != nil {
						// allow bare address
						if a, e2 := netip.ParseAddr(part); e2 == nil {
							w.addresses = append(w.addresses, a)
						}
						continue
					}
					w.addresses = append(w.addresses, pfx.Addr())
				}
			case "dns":
				for _, part := range strings.Split(val, ",") {
					if a, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil {
						w.dns = append(w.dns, a)
					}
				}
			case "mtu":
				w.mtu, _ = strconv.Atoi(val)
			}
		case "peer":
			switch key {
			case "publickey":
				hexKey, err := b64ToHex(val)
				if err != nil {
					return nil, err
				}
				w.peerPublicKey = hexKey
			case "presharedkey":
				if hexKey, err := b64ToHex(val); err == nil {
					w.presharedKey = hexKey
				}
			case "endpoint":
				ep, err := resolveEndpoint(val)
				if err != nil {
					return nil, err
				}
				w.endpoint = ep
			case "allowedips":
				for _, part := range strings.Split(val, ",") {
					part = strings.TrimSpace(part)
					if part != "" {
						w.allowedIPs = append(w.allowedIPs, part)
					}
				}
			case "persistentkeepalive":
				if k, err := strconv.Atoi(val); err == nil {
					w.keepalive = k
				}
			}
		}
	}

	if w.privateKey == "" || w.peerPublicKey == "" {
		return nil, fmt.Errorf("config missing keys")
	}
	if len(w.addresses) == 0 {
		return nil, fmt.Errorf("config missing interface address")
	}
	if len(w.dns) == 0 {
		// Resolve tracker hostnames through the tunnel rather than leaking to the
		// local resolver. Fall back to Cloudflare.
		w.dns = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	if len(w.allowedIPs) == 0 {
		w.allowedIPs = []string{"0.0.0.0/0", "::/0"}
	}
	return w, nil
}

// resolveEndpoint turns host:port into ip:port — wireguard-go's IpcSet endpoint
// expects a literal IP (it does not resolve DNS). Resolution uses the real net.
func resolveEndpoint(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", hostport, err)
	}
	if ip := net.ParseIP(host); ip != nil {
		return hostport, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("resolve endpoint %q: %w", host, err)
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}
