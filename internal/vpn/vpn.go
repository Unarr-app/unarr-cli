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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// ErrCode classifies fetch failures the agent should react to differently.
type ErrCode string

const (
	ErrDisabled       ErrCode = "disabled"        // 503 — VPN feature off server-side
	ErrNotProvisioned ErrCode = "not_provisioned" // 403 — user has no active VPN
	ErrSlotOnDevice   ErrCode = "slot_on_device"  // 409 — slot claimed by a device
	ErrUpstream       ErrCode = "upstream"        // network / 5xx / parse
)

// FetchError carries an ErrCode so callers can decide whether to retry, warn, or
// fall back to a clear (non-VPN) download.
type FetchError struct {
	Code ErrCode
	Msg  string
}

func (e *FetchError) Error() string { return fmt.Sprintf("vpn fetch: %s (%s)", e.Msg, e.Code) }

type fetchResponse struct {
	Content  string `json:"content"`
	Filename string `json:"filename"`
	ServerID int    `json:"serverId"`
	Mode     string `json:"mode"`
	Error    string `json:"error"`
	CodeStr  string `json:"code"`
}

// FetchConfig retrieves the agent's WireGuard .conf from the web API. Auth is
// `Authorization: Bearer <apiKey>` (the agent-auth scheme). agentId lets the web
// arbitrate the single WireGuard slot (first agent to ask claims it; others get
// 409 → ErrSlotOnDevice and should use OpenVPN on their host instead).
func FetchConfig(ctx context.Context, apiURL, apiKey, userAgent, agentID string, probe bool) (string, error) {
	q := neturl.Values{}
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	if probe {
		// Validate provisioning without claiming the WireGuard slot (status --check).
		q.Set("probe", "1")
	}
	url := strings.TrimSuffix(apiURL, "/") + "/api/internal/agent/vpn-config"
	if len(q) > 0 {
		url += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", &FetchError{ErrUpstream, err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", &FetchError{ErrUpstream, err.Error()}
	}
	defer resp.Body.Close()

	var body fetchResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	switch resp.StatusCode {
	case http.StatusOK:
		if body.Content == "" {
			return "", &FetchError{ErrUpstream, "empty config"}
		}
		return body.Content, nil
	case http.StatusServiceUnavailable:
		return "", &FetchError{ErrDisabled, "VPN disabled server-side"}
	case http.StatusForbidden:
		return "", &FetchError{ErrNotProvisioned, "no active VPN for this account"}
	case http.StatusConflict:
		return "", &FetchError{ErrSlotOnDevice, "VPN slot is active on one of your devices"}
	default:
		msg := body.Error
		if msg == "" {
			msg = "unexpected status " + strconv.Itoa(resp.StatusCode)
		}
		return "", &FetchError{ErrUpstream, msg}
	}
}

// Tunnel is a live userspace WireGuard tunnel. Net exposes a DialContext +
// ListenUDP backed by the tunnel; wire these into the torrent client.
type Tunnel struct {
	dev *device.Device
	Net *netstack.Net
	// Endpoint is the resolved ip:port of the WireGuard server this tunnel
	// exits through — surfaced in `unarr vpn status` so the user can see which
	// VPN server their torrent traffic is routed out of.
	Endpoint string
}

// Up parses a WireGuard .conf and brings up the tunnel in userspace.
func Up(confText string) (*Tunnel, error) {
	wc, err := parseConf(confText)
	if err != nil {
		return nil, err
	}

	mtu := wc.mtu
	if mtu == 0 {
		mtu = 1420
	}

	tunDev, tnet, err := netstack.CreateNetTUN(wc.addresses, wc.dns, mtu)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg-unarr "))
	if err := dev.IpcSet(wc.uapi()); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard ipc set: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard up: %w", err)
	}

	return &Tunnel{dev: dev, Net: tnet, Endpoint: wc.endpoint}, nil
}

// Close tears the tunnel down.
func (t *Tunnel) Close() {
	if t != nil && t.dev != nil {
		t.dev.Close()
	}
}

// ListenPacket adapts the tunnel's UDP for anacrolix TrackerListenPacket so UDP
// tracker announces also go through the VPN (no IP leak to trackers).
func (t *Tunnel) ListenPacket(_ string, _ string) (net.PacketConn, error) {
	return t.Net.ListenUDP(&net.UDPAddr{IP: net.IPv4zero, Port: 0})
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
