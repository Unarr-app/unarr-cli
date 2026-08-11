package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/vpn"
)

// This file wires the managed-VPN split-tunnel + the optional fail-CLOSED P2P
// kill-switch ([downloads.vpn] required). It is split out of daemon.go so the
// bring-up, the reconnect supervisor, and their helpers stay a single, small
// responsibility. runDaemonStart calls bringUpVPN + (when required) launches
// superviseVPNTunnel.

// bringUpVPN brings up the managed-VPN split-tunnel per config, returning the
// tunnel (or nil) and the mode label ("managed" | "self-hosted" | "").
//
// required=false (default): best-effort. A failure returns (nil, mode) and the
// torrent client downloads in the clear — the historical behavior, unchanged.
//
// required=true: fail-CLOSED. A failure still returns a non-nil but EMPTY tunnel
// (no live device) as long as the VPN is configured, so it is wired into the
// torrent client (its dials fail closed — no clear-net P2P) and the reconnect
// supervisor can later heal it into a live one. When the VPN is not configured at
// all (mode ""), it returns nil and the Available() gate alone keeps torrent off.
func bringUpVPN(cfg config.Config, required bool) (*vpn.Tunnel, string) {
	mode := vpnMode(cfg)
	if mode == "" {
		return nil, "" // VPN not configured — nothing to bring up
	}

	conf, err := loadVPNConf(context.Background(), cfg)
	if err != nil {
		logVPNBringUpError(err, required)
		return failedTunnel(required), mode
	}

	tunnel, err := vpn.Up(conf)
	if err != nil {
		if required {
			log.Printf("[vpn] tunnel failed to start (%v) - VPN REQUIRED, so torrent/P2P is DISABLED (debrid/usenet still available); the supervisor will keep retrying", err)
		} else {
			log.Printf("[vpn] tunnel failed to start (%v) - downloading in the clear", err)
		}
		return failedTunnel(required), mode
	}

	log.Printf("[vpn] tunnel active (%s) - torrent traffic split-tunnelled through WireGuard", mode)
	return tunnel, mode
}

// failedTunnel returns the tunnel to wire in after a failed bring-up: an EMPTY
// (device-less, fail-closed + reconnectable) tunnel when the kill-switch is on, or
// nil when it is off (best-effort clear-net path).
func failedTunnel(required bool) *vpn.Tunnel {
	if required {
		return &vpn.Tunnel{}
	}
	return nil
}

// bringUpVPNForOneShot brings the managed-VPN tunnel up for a one-shot CLI command
// (`unarr download` / `unarr stream`), returning the tunnel (nil when VPN is not
// configured). Unlike the daemon it does NOT launch the reconnect supervisor — a
// one-shot lives for a single transfer. The kill-switch still holds: the tunnel's
// dials fail closed and, with required=true, a device-less/failed tunnel keeps
// torrent/P2P disabled via the Available() / NewStreamEngine gate. The caller must
// Close the returned tunnel.
func bringUpVPNForOneShot(cfg config.Config, required bool) *vpn.Tunnel {
	tunnel, _ := bringUpVPN(cfg, required)
	return tunnel
}

// vpnMode reports the configured VPN mode: self-hosted (local config_file) takes
// precedence over managed (fetched from the account); "" means VPN is off.
func vpnMode(cfg config.Config) string {
	switch {
	case cfg.Download.VPN.ConfigFile != "":
		return "self-hosted"
	case cfg.Download.VPN.Enabled:
		return "managed"
	default:
		return ""
	}
}

// vpnEndpoint is the exit server label to record in the daemon state (nil-safe).
func vpnEndpoint(t *vpn.Tunnel) string {
	if t == nil {
		return ""
	}
	return t.Endpoint
}

// apiURLOrDefault is the API base to talk to. Belt-and-braces: config.Load now
// normalizes an empty api_url to config.DefaultAPIURL, so this only fires for a
// Config built by hand (tests, or a caller that skipped Load).
//
// It exists at all because the three VPN/agent call sites used to hardcode a
// fallback of "https://torrentclaw.com" while config.Default() returns
// "https://unarr.app" — two different deployments, each with its own env, so the
// VPN fetch could land somewhere the rest of the agent never talks to. Whatever
// the default is, it must be decided in ONE place.
func apiURLOrDefault(cfg config.Config) string {
	if cfg.Auth.APIURL != "" {
		return cfg.Auth.APIURL
	}
	return config.DefaultAPIURL
}

// logVPNBringUpError logs the initial bring-up failure with a message tuned to
// both the cause (the single WireGuard slot being held by another device is a
// distinct, common case) and whether the kill-switch is on.
func logVPNBringUpError(err error, required bool) {
	var fe *vpn.FetchError
	slotHeld := errors.As(err, &fe) && fe.Code == vpn.ErrSlotOnDevice
	// "The server says the VPN feature is off" is NOT a problem with the user's
	// account, and the old wording ("VPN disabled server-side") read exactly like
	// one — a PRO subscriber with a correctly provisioned add-on filed a bug
	// because of it. Name the host and point at the setting that selects it.
	if errors.As(err, &fe) && fe.Code == vpn.ErrDisabled {
		log.Printf("[vpn] the VPN feature is not available on %s - your account provisioning is NOT the problem. Check `unarr config get auth.api_url` and `unarr update`; if it persists, contact support.", fe.Host)
		if required {
			log.Printf("[vpn] VPN REQUIRED, so torrent/P2P is DISABLED (debrid/usenet still available); the supervisor will keep retrying")
		} else {
			log.Printf("[vpn] downloading in the clear until the VPN becomes available")
		}
		return
	}
	switch {
	case slotHeld && required:
		log.Printf("[vpn] the single WireGuard slot is held by another unarr agent - VPN REQUIRED, so THIS agent's torrent/P2P is DISABLED (debrid/usenet still work). Free the slot or run OpenVPN on this machine. See https://unarr.app/vpn")
	case slotHeld:
		log.Printf("[vpn] the single WireGuard slot is already held by another unarr agent - this one downloads in the clear. To protect this machine too, set up OpenVPN on it (1 agent uses WireGuard, the rest use OpenVPN - up to 10). See https://unarr.app/vpn")
	case required:
		log.Printf("[vpn] could not fetch VPN config (%v) - VPN REQUIRED, torrent/P2P DISABLED (debrid/usenet still available); the supervisor will keep retrying", err)
	default:
		log.Printf("[vpn] could not enable VPN (%v) - downloading in the clear", err)
	}
}

// loadVPNConf returns the WireGuard .conf text for the configured mode: a local
// config_file (self-hosted) or a fresh fetch from the account (managed). Shared by
// the initial bring-up and the reconnect supervisor.
func loadVPNConf(ctx context.Context, cfg config.Config) (string, error) {
	if cfg.Download.VPN.ConfigFile != "" {
		raw, err := os.ReadFile(cfg.Download.VPN.ConfigFile)
		if err != nil {
			return "", fmt.Errorf("read config_file %q: %w", cfg.Download.VPN.ConfigFile, err)
		}
		return string(raw), nil
	}

	apiURL := apiURLOrDefault(cfg)
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return vpn.FetchConfig(fetchCtx, vpn.FetchRequest{
		APIURL:    apiURL,
		APIKey:    cfg.Auth.APIKey,
		UserAgent: "unarr/" + Version,
		AgentID:   cfg.Agent.ID,
	})
}

// superviseVPNTunnel is the kill-switch reconnect loop. While the VPN is required
// and a tunnel was attempted, it watches tunnel health and, on a drop, records the
// blocking state (so new torrents stay gated and `unarr vpn status`/doctor show
// BLOCKED) and retries bring-up with capped exponential backoff. A recovered
// tunnel re-opens the gate. Debrid/usenet are never affected. Returns on ctx cancel.
func superviseVPNTunnel(ctx context.Context, d *agent.Daemon, tunnel *vpn.Tunnel, cfg config.Config, mode string) {
	const (
		checkVPNInterval = 15 * time.Second
		minVPNBackoff    = 5 * time.Second
		maxVPNBackoff    = 2 * time.Minute
	)
	ticker := time.NewTicker(checkVPNInterval)
	defer ticker.Stop()

	backoff := minVPNBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if tunnel.Healthy() {
				backoff = minVPNBackoff
				continue
			}
			// Tunnel down: block P2P (torrent), then try to bring it back.
			d.SetVPNState(false, true, mode, tunnel.Endpoint)
			log.Printf("[vpn] tunnel DOWN - P2P blocked (torrent disabled; debrid/usenet unaffected); reconnecting...")
			if err := reconnectVPN(ctx, tunnel, cfg); err != nil {
				log.Printf("[vpn] reconnect failed (%v) - staying blocked, retrying in %s", err, backoff)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, maxVPNBackoff)
				continue
			}
			d.SetVPNState(true, true, mode, tunnel.Endpoint)
			log.Printf("[vpn] tunnel reconnected - P2P re-enabled")
			backoff = minVPNBackoff
		}
	}
}

// reconnectVPN fetches a fresh WireGuard config and hot-swaps it into the live
// tunnel so the long-lived torrent client transparently routes through the new
// device (it dials via the stable Tunnel methods, not a captured netstack).
func reconnectVPN(ctx context.Context, tunnel *vpn.Tunnel, cfg config.Config) error {
	conf, err := loadVPNConf(ctx, cfg)
	if err != nil {
		return err
	}
	return tunnel.Reconnect(conf)
}

// sleepCtx sleeps for d, or returns early (false) if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
