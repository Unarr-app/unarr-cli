package cmd

import (
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
)

// newAgentClientFromConfig builds an agent.Client wired with the mirror pool
// from the user's TOML config. Use this instead of agent.NewClient in any
// long-running command (daemon, status loop, etc.) so a `.com` outage rolls
// over to `.to` / .onion without restarting the agent.
//
// The function lives in cmd/ rather than agent/ because it has to know
// about the config struct, and cmd/ is the only place that owns the
// "wire defaults + user overrides" rule.
func newAgentClientFromConfig(cfg config.Config, userAgent string) *agent.Client {
	return agent.NewClientWithMirrors(
		cfg.Auth.APIURL,
		cfg.Auth.Mirrors,
		cfg.Auth.APIKey,
		userAgent,
	)
}
