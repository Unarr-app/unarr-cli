package vpn

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
)

// b64key encodes exactly 32 bytes so b64ToHex's length guard accepts it.
func b64key(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func hexOfB64(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode %q: %v", b64, err)
	}
	return hex.EncodeToString(raw)
}

// TestB64ToHex covers the WireGuard key decode + 32-byte length guard — the
// guard that stops a malformed/short key from silently producing a broken tunnel.
func TestB64ToHex(t *testing.T) {
	// A valid, known 32-byte key → correct lowercase 64-char hex.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	got, err := b64ToHex(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("valid 32-byte key errored: %v", err)
	}
	want := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got != want {
		t.Errorf("hex = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Errorf("hex length = %d, want 64", len(got))
	}
	if got != strings.ToLower(got) {
		t.Errorf("hex %q is not lowercase", got)
	}

	// Invalid base64 → "invalid base64 key".
	if _, err := b64ToHex("@@@@not-base64@@@@"); err == nil {
		t.Error("invalid base64 key should error")
	} else if !strings.Contains(err.Error(), "invalid base64 key") {
		t.Errorf("error = %q, want it to mention 'invalid base64 key'", err)
	}

	// Valid base64 but the wrong length (16 bytes) → "key must be 32 bytes, got 16".
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := b64ToHex(short); err == nil {
		t.Error("16-byte key should error")
	} else if !strings.Contains(err.Error(), "must be 32 bytes, got 16") {
		t.Errorf("error = %q, want it to mention 'must be 32 bytes, got 16'", err)
	}

	// A too-long key (33 bytes) is rejected too.
	long := base64.StdEncoding.EncodeToString(make([]byte, 33))
	if _, err := b64ToHex(long); err == nil || !strings.Contains(err.Error(), "got 33") {
		t.Errorf("33-byte key err = %v, want 'got 33'", err)
	}
}

// TestParseConf asserts on the returned wgConf (not just no-error): a silent
// mis-parse (dropped allowedIPs / wrong key / missing default) weakens the
// tunnel without an error, so every field is checked.
func TestParseConf(t *testing.T) {
	privB64 := b64key(1)
	pubB64 := b64key(100)
	pskB64 := b64key(200)
	privHex := hexOfB64(t, privB64)
	pubHex := hexOfB64(t, pubB64)
	pskHex := hexOfB64(t, pskB64)

	full := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.0.0.2/32
DNS = 10.0.0.1
MTU = 1420

[Peer]
PublicKey = %s
PresharedKey = %s
Endpoint = 1.2.3.4:51820
AllowedIPs = 10.0.0.0/24, ::/0
PersistentKeepalive = 15
`, privB64, pubB64, pskB64)

	tests := []struct {
		name           string
		conf           string
		wantErrContain string
		check          func(t *testing.T, w *wgConf)
	}{
		{
			name: "full config parses every field",
			conf: full,
			check: func(t *testing.T, w *wgConf) {
				if w.privateKey != privHex {
					t.Errorf("privateKey = %q, want %q", w.privateKey, privHex)
				}
				if w.peerPublicKey != pubHex {
					t.Errorf("peerPublicKey = %q, want %q", w.peerPublicKey, pubHex)
				}
				if w.presharedKey != pskHex {
					t.Errorf("presharedKey = %q, want %q (optional PSK must be parsed)", w.presharedKey, pskHex)
				}
				if len(w.addresses) != 1 || w.addresses[0].String() != "10.0.0.2" {
					t.Errorf("addresses = %v, want [10.0.0.2]", w.addresses)
				}
				if len(w.dns) != 1 || w.dns[0].String() != "10.0.0.1" {
					t.Errorf("dns = %v, want [10.0.0.1]", w.dns)
				}
				if w.mtu != 1420 {
					t.Errorf("mtu = %d, want 1420", w.mtu)
				}
				if w.endpoint != "1.2.3.4:51820" {
					t.Errorf("endpoint = %q, want 1.2.3.4:51820", w.endpoint)
				}
				if fmt.Sprint(w.allowedIPs) != "[10.0.0.0/24 ::/0]" {
					t.Errorf("allowedIPs = %v, want [10.0.0.0/24 ::/0]", w.allowedIPs)
				}
				if w.keepalive != 15 {
					t.Errorf("keepalive = %d, want 15", w.keepalive)
				}
			},
		},
		{
			name: "defaults: no DNS/AllowedIPs/keepalive, bare address, comments and blanks",
			conf: fmt.Sprintf(`# leading comment
[Interface]
PrivateKey = %s
Address = 10.0.0.9

# a peer follows
[Peer]
PublicKey = %s
Endpoint = 5.6.7.8:51820
`, privB64, pubB64),
			check: func(t *testing.T, w *wgConf) {
				// bare Address (no /prefix) is accepted.
				if len(w.addresses) != 1 || w.addresses[0].String() != "10.0.0.9" {
					t.Errorf("addresses = %v, want [10.0.0.9] (bare address)", w.addresses)
				}
				// DNS defaults to Cloudflare.
				if len(w.dns) != 1 || w.dns[0].String() != "1.1.1.1" {
					t.Errorf("dns = %v, want [1.1.1.1] (default)", w.dns)
				}
				// AllowedIPs defaults to full tunnel.
				if fmt.Sprint(w.allowedIPs) != "[0.0.0.0/0 ::/0]" {
					t.Errorf("allowedIPs = %v, want [0.0.0.0/0 ::/0] (default)", w.allowedIPs)
				}
				// keepalive stays at the constructor default of 25.
				if w.keepalive != 25 {
					t.Errorf("keepalive = %d, want 25 (default)", w.keepalive)
				}
				if w.presharedKey != "" {
					t.Errorf("presharedKey = %q, want empty (none given)", w.presharedKey)
				}
			},
		},
		{
			name:           "missing private key",
			conf:           fmt.Sprintf("[Interface]\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = %s\n", pubB64),
			wantErrContain: "config missing keys",
		},
		{
			name:           "missing peer public key",
			conf:           fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.0.0.2/32\n[Peer]\nEndpoint = 1.2.3.4:51820\n", privB64),
			wantErrContain: "config missing keys",
		},
		{
			name:           "missing interface address",
			conf:           fmt.Sprintf("[Interface]\nPrivateKey = %s\n[Peer]\nPublicKey = %s\n", privB64, pubB64),
			wantErrContain: "config missing interface address",
		},
		{
			name:           "invalid base64 private key propagates from b64ToHex",
			conf:           fmt.Sprintf("[Interface]\nPrivateKey = @@@@\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = %s\n", pubB64),
			wantErrContain: "invalid base64 key",
		},
		{
			name:           "invalid base64 public key propagates from b64ToHex",
			conf:           fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = ####\n", privB64),
			wantErrContain: "invalid base64 key",
		},
		{
			name:           "short private key hits the 32-byte length guard",
			conf:           fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = %s\n", base64.StdEncoding.EncodeToString(make([]byte, 16)), pubB64),
			wantErrContain: "must be 32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parseConf(tt.conf)
			if tt.wantErrContain != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (w=%+v)", tt.wantErrContain, w)
				}
				if !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w == nil {
				t.Fatal("nil wgConf without error")
			}
			tt.check(t, w)
		})
	}
}

// TestUAPIRoundTrip renders a parsed wgConf to WireGuard UAPI text and asserts
// the exact field names + conditional omission. A wrong UAPI field name silently
// breaks IpcSet, so this is paired with parseConf as a round-trip.
func TestUAPIRoundTrip(t *testing.T) {
	privB64, pubB64, pskB64 := b64key(2), b64key(70), b64key(150)
	conf := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.0.0.2/32
[Peer]
PublicKey = %s
PresharedKey = %s
Endpoint = 1.2.3.4:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 21
`, privB64, pubB64, pskB64)

	w, err := parseConf(conf)
	if err != nil {
		t.Fatalf("parseConf: %v", err)
	}
	out := w.uapi()

	for _, want := range []string{
		"private_key=" + hexOfB64(t, privB64),
		"public_key=" + hexOfB64(t, pubB64),
		"preshared_key=" + hexOfB64(t, pskB64),
		"endpoint=1.2.3.4:51820",
		"persistent_keepalive_interval=21",
		"allowed_ip=0.0.0.0/0",
		"allowed_ip=::/0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("uapi() missing %q\n---\n%s", want, out)
		}
	}
	if n := strings.Count(out, "allowed_ip="); n != 2 {
		t.Errorf("allowed_ip line count = %d, want 2 (one per entry)", n)
	}

	// Empty/zero optional fields must be OMITTED (not emitted with empty values).
	minimal := &wgConf{privateKey: "aa", peerPublicKey: "bb", allowedIPs: []string{"0.0.0.0/0"}}
	mout := minimal.uapi()
	if !strings.Contains(mout, "private_key=aa") || !strings.Contains(mout, "public_key=bb") {
		t.Errorf("minimal uapi() missing required keys:\n%s", mout)
	}
	for _, absent := range []string{"preshared_key=", "endpoint=", "persistent_keepalive_interval="} {
		if strings.Contains(mout, absent) {
			t.Errorf("minimal uapi() must omit %q when unset:\n%s", absent, mout)
		}
	}
	if n := strings.Count(mout, "allowed_ip="); n != 1 {
		t.Errorf("minimal allowed_ip count = %d, want 1", n)
	}
}

// TestResolveEndpoint covers only the deterministic (no-DNS) branches: a literal
// ip:port is returned unchanged; a malformed input without a port errors.
func TestResolveEndpoint(t *testing.T) {
	if got, err := resolveEndpoint("1.2.3.4:51820"); err != nil || got != "1.2.3.4:51820" {
		t.Errorf("resolveEndpoint(literal v4) = (%q, %v), want (1.2.3.4:51820, nil)", got, err)
	}
	if got, err := resolveEndpoint("[2001:db8::1]:51820"); err != nil || got != "[2001:db8::1]:51820" {
		t.Errorf("resolveEndpoint(literal v6) = (%q, %v), want unchanged, nil", got, err)
	}
	if _, err := resolveEndpoint("hostonly"); err == nil {
		t.Error("resolveEndpoint with no port should error (missing port)")
	} else if !strings.Contains(err.Error(), "invalid endpoint") {
		t.Errorf("error = %q, want it to mention 'invalid endpoint'", err)
	}
}

// TestFetchConfig drives an httptest server and asserts the HTTP-status → ErrCode
// classification that gates the agent's VPN-vs-clear fallback, plus the outgoing
// auth header and agentId/probe query params.
func TestFetchConfig(t *testing.T) {
	const confBody = "[Interface]\nPrivateKey = x\n"

	tests := []struct {
		name        string
		status      int
		body        string
		wantContent string
		wantCode    ErrCode // "" = expect success
		wantMsg     string  // optional substring of FetchError.Msg
	}{
		{name: "200 with content", status: 200, body: `{"content":"` + strings.ReplaceAll(confBody, "\n", `\n`) + `"}`, wantContent: confBody},
		{name: "200 empty body", status: 200, body: `{"content":""}`, wantCode: ErrUpstream, wantMsg: "empty config"},
		// A 503 is only "the feature is off" when the APPLICATION said so (it
		// answers a JSON error body). A bare 503 is an edge/proxy/deploy blip and
		// must stay retryable — reporting "VPN disabled server-side" for one told
		// a paying user their add-on was broken when it was fine.
		{name: "503 from the app is disabled", status: 503, body: `{"error":"VPN disabled"}`, wantCode: ErrDisabled},
		{name: "bare 503 from a proxy is upstream, not disabled", status: 503, body: ``, wantCode: ErrUpstream, wantMsg: "temporarily unavailable"},
		{name: "403 not provisioned", status: 403, body: `{}`, wantCode: ErrNotProvisioned},
		{name: "409 slot on device", status: 409, body: `{}`, wantCode: ErrSlotOnDevice},
		{name: "500 unknown", status: 500, body: `{}`, wantCode: ErrUpstream},
		{name: "418 unknown status carries server error message", status: 418, body: `{"error":"teapot"}`, wantCode: ErrUpstream, wantMsg: "teapot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth, gotUA, gotAccept, gotQuery, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				gotUA = r.Header.Get("User-Agent")
				gotAccept = r.Header.Get("Accept")
				gotQuery = r.URL.RawQuery
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			content, err := FetchConfig(context.Background(), FetchRequest{APIURL: srv.URL, APIKey: "secret-key", UserAgent: "unarr/test", AgentID: "agent-123", Probe: true})

			// Request shape is the same regardless of the response.
			if gotAuth != "Bearer secret-key" {
				t.Errorf("Authorization = %q, want 'Bearer secret-key'", gotAuth)
			}
			if gotUA != "unarr/test" {
				t.Errorf("User-Agent = %q, want unarr/test", gotUA)
			}
			if gotAccept != "application/json" {
				t.Errorf("Accept = %q, want application/json", gotAccept)
			}
			if gotPath != "/api/internal/agent/vpn-config" {
				t.Errorf("path = %q, want /api/internal/agent/vpn-config", gotPath)
			}
			if !strings.Contains(gotQuery, "agentId=agent-123") || !strings.Contains(gotQuery, "probe=1") {
				t.Errorf("query = %q, want it to carry agentId=agent-123 and probe=1", gotQuery)
			}

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if content != tt.wantContent {
					t.Errorf("content = %q, want %q", content, tt.wantContent)
				}
				return
			}

			if content != "" {
				t.Errorf("content = %q, want empty on error", content)
			}
			var fe *FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("error %v is not a *FetchError", err)
			}
			if fe.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", fe.Code, tt.wantCode)
			}
			if tt.wantMsg != "" && !strings.Contains(fe.Msg, tt.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", fe.Msg, tt.wantMsg)
			}
		})
	}
}

// TestFetchConfigOmitsProbeAndAgent asserts the query params are only added when
// requested (agentId="" + probe=false → no query string).
// TestFetchErrorCarriesHost locks the host into the error. "The VPN is disabled
// server-side" is only actionable together with WHICH server said it: the agent
// defaults to unarr.app, and a deployment missing the VPN env answers 503 for
// every account. Without the host the user cannot tell a wrong `auth.api_url`
// from a broken subscription — the exact confusion that produced a bug report
// from a correctly provisioned PRO account.
func TestFetchErrorCarriesHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"VPN disabled"}`)
	}))
	defer srv.Close()

	_, err := FetchConfig(context.Background(), FetchRequest{APIURL: srv.URL, APIKey: "k", UserAgent: "unarr/test"})
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a *FetchError", err)
	}
	// httptest serves on 127.0.0.1:<port> — the host label must be the authority,
	// never the full URL (it is rendered inline in a user-facing sentence).
	wantHost := strings.TrimPrefix(srv.URL, "http://")
	if fe.Host != wantHost {
		t.Errorf("Host = %q, want %q", fe.Host, wantHost)
	}
	if !strings.Contains(err.Error(), wantHost) {
		t.Errorf("Error() = %q, want it to name the host %q", err.Error(), wantHost)
	}
}

func TestFetchConfigOmitsProbeAndAgent(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"content":"ok"}`)
	}))
	defer srv.Close()

	if _, err := FetchConfig(context.Background(), FetchRequest{APIURL: srv.URL, APIKey: "k", UserAgent: "ua"}); err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty when agentId unset and probe=false", gotQuery)
	}
}

// TestHealthyIpcGetErrorFailsClosed: a live device whose IpcGet returns an error
// is treated as DOWN (fail-closed) — the kill-switch's degraded-but-present-device
// guarantee. Uses the ipcGet seam so no real WireGuard device is needed.
func TestHealthyIpcGetErrorFailsClosed(t *testing.T) {
	orig := ipcGet
	t.Cleanup(func() { ipcGet = orig })

	tun := &Tunnel{}
	tun.inner.Store(&tunnelInner{dev: &device.Device{}, startedAt: time.Now()})

	// Positive control: a fresh handshake via the seam → Healthy() true (proves the
	// seam is actually wired into Healthy).
	ipcGet = func(*device.Device) (string, error) {
		return fmt.Sprintf("last_handshake_time_sec=%d\n", time.Now().Unix()), nil
	}
	if !tun.Healthy() {
		t.Fatal("Healthy() = false with a fresh handshake via the seam; want true")
	}

	// The branch under test: IpcGet error → fail-closed DOWN.
	ipcGet = func(*device.Device) (string, error) { return "", errors.New("ipc boom") }
	if tun.Healthy() {
		t.Error("Healthy() = true when IpcGet errors; want false (fail-closed on a degraded device)")
	}
}
