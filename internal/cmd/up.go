package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// newUpCmd creates the top-level `unarr up` command — the unattended-bootstrap
// entry point. Unlike `unarr init` (interactive wizard) or `unarr start` (which
// requires a credential already in config.toml), `up` can self-provision from a
// single-use auth-key: it redeems the key for a durable per-agent API key,
// persists it, then starts the daemon. This is the Tailscale-style flow — the
// user never pastes a tc_ key.
//
// With no auth-key and an already-configured credential, `up` is just an alias
// for `start`.
func newUpCmd() *cobra.Command {
	var (
		authKey string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Provision (via auth-key) and start the daemon",
		Long: `Bring the agent up: redeem an auth-key for a durable credential if needed,
then start the daemon in the foreground.

Provide a single-use auth-key (generated in unarr.app) with --auth-key or the
UNARR_AUTHKEY environment variable. The agent exchanges it for a durable
per-agent API key, saves it to config.toml, and starts — no manual API-key
paste required.

The exchange is idempotent: if a valid API key is already configured, the
auth-key is ignored and the daemon just starts. Pass --force to re-exchange
and overwrite the stored credential.`,
		Example: `  unarr up --auth-key=unarr-authkey-XXXX
  UNARR_AUTHKEY=unarr-authkey-XXXX unarr up
  unarr up                       # already provisioned → just start`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if authKey == "" {
				authKey = strings.TrimSpace(os.Getenv("UNARR_AUTHKEY"))
			}
			if err := runUp(strings.TrimSpace(authKey), force); err != nil {
				return err
			}
			return runDaemonStart()
		},
	}

	cmd.Flags().StringVar(&authKey, "auth-key", "", "single-use auth-key (unarr-authkey-...) to redeem for a durable credential; or set UNARR_AUTHKEY")
	cmd.Flags().BoolVar(&force, "force", false, "re-exchange the auth-key even if a credential is already configured")

	return cmd
}

// runUp resolves the credential before the daemon starts. It exchanges the
// auth-key when one is supplied and the agent is not already provisioned (or
// --force is set), persisting the durable credential to config.toml. When no
// auth-key is given it is a no-op (the daemon's own validation reports a missing
// credential with the "run unarr init" hint).
func runUp(authKey string, force bool) error {
	cfg := loadConfig()

	// Idempotent: a valid key already present and no auth-key (or no --force) →
	// nothing to do, fall through to start.
	hasKey := strings.HasPrefix(cfg.Auth.APIKey, "tc_")
	if authKey == "" {
		return nil
	}
	if hasKey && !force {
		color.New(color.FgHiBlack).Println("  Already provisioned — ignoring auth-key (pass --force to re-exchange).")
		return nil
	}

	if !isValidAuthKeyFormat(authKey) {
		return fmt.Errorf("invalid auth-key format %q (expected prefix unarr-authkey-)", redactAuthKey(authKey))
	}

	// Ensure a stable agent identity exists and is persisted BEFORE the exchange,
	// so the durable key the server mints is bound to this agent id (and a retry
	// reuses the same identity rather than orphaning one per attempt).
	agentID, err := ensureAgentID(&cfg)
	if err != nil {
		return err
	}

	apiURL := cfg.Auth.APIURL
	if apiURL == "" {
		apiURL = "https://torrentclaw.com"
	}

	hostname, _ := os.Hostname()
	platform := runtime.GOOS + "/" + runtime.GOARCH

	fmt.Print("  Redeeming auth-key... ")
	// No api key yet — the exchange endpoint is public and authenticates on the
	// auth-key in the body. Use a single base URL (mirror failover still applies
	// inside the client for transient transport errors).
	ac := agent.NewClient(apiURL, cfg.Auth.APIKey, "unarr/"+Version)
	resp, err := ac.ExchangeAuthKey(context.Background(), agent.ExchangeAuthKeyRequest{
		AuthKey:  authKey,
		AgentID:  agentID,
		Hostname: hostname,
		Platform: platform,
	})
	if err != nil {
		color.Red("FAILED")
		fmt.Println()
		return authKeyExchangeError(err)
	}

	color.New(color.FgGreen).Println("OK")

	// Persist the durable credential. The minted apiKey + (optional) apiUrl are
	// written to config.toml so the daemon — and every future run — uses them
	// without the auth-key.
	cfg.Auth.APIKey = resp.APIKey
	if resp.APIURL != "" {
		cfg.Auth.APIURL = resp.APIURL
	}
	if err := config.Save(cfg, resolvedConfigPath()); err != nil {
		return fmt.Errorf("save credential after exchange: %w", err)
	}
	appCfg = cfg // refresh the cached config so runDaemonStart sees the new key

	fmt.Printf("  Provisioned agent %s\n", agentID)
	fmt.Println()
	return nil
}

// ensureAgentID returns the agent id from config, generating and PERSISTING a
// new UUID when none exists. Shared between `unarr up` and `unarr init` so a
// fresh identity is minted identically (and saved) regardless of entry point.
// On generation it writes config.toml immediately so the id survives a crash
// between exchange and the first register.
func ensureAgentID(cfg *config.Config) (string, error) {
	if cfg.Agent.ID != "" {
		return cfg.Agent.ID, nil
	}
	id := uuid.New().String()
	cfg.Agent.ID = id
	if err := config.Save(*cfg, resolvedConfigPath()); err != nil {
		return "", fmt.Errorf("persist new agent id: %w", err)
	}
	appCfg = *cfg
	return id, nil
}

// isValidAuthKeyFormat does a cheap structural check on an auth-key before we
// spend a network round-trip on it. The web mints them as unarr-authkey-<random>.
func isValidAuthKeyFormat(k string) bool {
	const prefix = "unarr-authkey-"
	return strings.HasPrefix(k, prefix) && len(k) > len(prefix)
}

// redactAuthKey returns a safe-to-log form of an auth-key (prefix + a few
// trailing chars) so an error message never echoes the full secret.
func redactAuthKey(k string) string {
	if len(k) <= 8 {
		return "***"
	}
	return k[:8] + "…" + k[len(k)-3:]
}

// authKeyExchangeError maps the server's structured exchange errors
// (invalid|expired|used|revoked) to actionable user-facing messages. Anything
// else (network, 5xx, unexpected body) is surfaced verbatim.
func authKeyExchangeError(err error) error {
	var he *agent.HTTPError
	if !errors.As(err, &he) {
		// Transport / non-HTTP failure — surface as-is.
		return fmt.Errorf("auth-key exchange failed: %w", err)
	}

	// HTTPError.Message carries the parsed JSON `error` token for 4xx bodies.
	switch authKeyErrorToken(he.Message) {
	case "expired":
		return fmt.Errorf("auth-key expired — generate a new one in unarr.app")
	case "used":
		return fmt.Errorf("auth-key already used (single-use) — generate a new one in unarr.app")
	case "revoked":
		return fmt.Errorf("auth-key was revoked — generate a new one in unarr.app")
	case "invalid":
		return fmt.Errorf("auth-key is invalid — check it and generate a new one in unarr.app if needed")
	default:
		return fmt.Errorf("auth-key exchange failed (HTTP %d): %s", he.StatusCode, he.Message)
	}
}

// authKeyErrorToken extracts the canonical error token from an HTTPError
// message. handleResponse parses { "error": "<token>" } into the message, but a
// non-JSON / wrapped body may embed it — match defensively on substring so the
// 4 documented tokens are always recognised.
func authKeyErrorToken(msg string) string {
	m := strings.ToLower(strings.TrimSpace(msg))
	switch {
	case m == "expired" || strings.Contains(m, "expired"):
		return "expired"
	case m == "used" || strings.Contains(m, "used"):
		return "used"
	case m == "revoked" || strings.Contains(m, "revoked"):
		return "revoked"
	case m == "invalid" || strings.Contains(m, "invalid"):
		return "invalid"
	default:
		return ""
	}
}
