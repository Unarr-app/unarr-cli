package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/google/uuid"
)

// effectiveAPIKey returns the API key doctor acts on: the global --api-key
// flag wins over the persisted config value. Empty means "no key".
func effectiveAPIKey(cfg *config.Config) string {
	if apiKeyFlag != "" {
		return apiKeyFlag
	}
	return cfg.Auth.APIKey
}

// classifyAuthError distinguishes "the server rejected this credential"
// (401/403 → the key is invalid, expired, or bound to another machine) from
// transport/other failures, and attaches the actionable remedy. Doctor uses it
// so a user sees "run `unarr login`" instead of a bare HTTP status.
func classifyAuthError(err error) error {
	var he *agent.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("API key rejected (HTTP %d) — run `unarr login` to re-authenticate: %w", he.StatusCode, err)
		}
	}
	return err
}

// planAgentRegistrationRepair returns the register-this-machine repair when an
// API key is configured but no agent identity was ever persisted (interrupted
// `unarr init`, hand-written config). Nil when not applicable. The repair is
// safe: it uses the existing credential against the configured api_url — the
// same Register call doctor's read-only check performs — and only persists the
// minted identity on success.
func planAgentRegistrationRepair(cfg *config.Config) *repair {
	if effectiveAPIKey(cfg) == "" || cfg.Agent.ID != "" {
		return nil
	}
	return &repair{
		desc:  "Register this machine with the server (persist agent id + name)",
		apply: func() error { return applyAgentRegistration(cfg) },
	}
}

// applyAgentRegistration mints an agent UUID (matching ensureAgentID's
// identity scheme), registers it with the server using the existing API key,
// and writes the identity — plus a server-minted per-machine key, when the
// manual-paste bootstrap returns one (same swap `unarr init` does) — into cfg
// for the caller to persist. On failure cfg is left untouched.
func applyAgentRegistration(cfg *config.Config) error {
	id := uuid.New().String()
	name := cfg.Agent.Name
	if name == "" {
		name, _ = os.Hostname()
	}

	ac := agent.NewClient(cfg.Auth.APIURL, effectiveAPIKey(cfg), "unarr/"+Version)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := ac.Register(ctx, agent.RegisterRequest{
		AgentID:     id,
		Name:        name,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     Version,
		DownloadDir: cfg.Download.Dir,
	})
	if err != nil {
		return classifyAuthError(err)
	}

	cfg.Agent.ID = id
	cfg.Agent.Name = name
	if resp.AgentKey != "" {
		cfg.Auth.APIKey = resp.AgentKey
	}
	return nil
}
