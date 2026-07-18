package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// accountFetchTimeout bounds the /api/internal/agent/me request; the tray must
// never hang on a dead network.
const accountFetchTimeout = 15 * time.Second

// errNotSignedIn: the agent config has no API key, so there is nothing to
// authenticate with — the caller renders "not signed in" instead of an error.
var errNotSignedIn = errors.New("agent has no API key (not signed in)")

// newAgentClient builds the API client the one way every tray surface must:
// config + env overrides + APIURL fallback. Shared by the account fetch and
// the support-report path so the two can never authenticate differently. The
// returned base is the server the client talks to (for building same-origin
// URLs like the upgrade CTA). It must NEVER call Register: that mutates agent
// rows on the server and can rotate per-machine keys out from under the
// running daemon.
func newAgentClient() (*agent.Client, string, error) {
	cfg, err := config.Load(config.FilePath())
	if err != nil {
		return nil, "", fmt.Errorf("load config: %w", err)
	}
	cfg.ApplyEnvOverrides() // honor UNARR_API_URL / UNARR_API_KEY like the daemon does
	if cfg.Auth.APIKey == "" {
		return nil, "", errNotSignedIn
	}
	base := cfg.Auth.APIURL
	if base == "" {
		base = webBase()
	}
	return agent.NewClient(base, cfg.Auth.APIKey, "unarr-desktop/"+version), base, nil
}

// planLabel maps the server's account fields to the tray label. The server's
// isPro is AUTHORITATIVE — it already encodes plan/trial/future tier
// semantics, so an unrecognized paid plan value must still render as unarr+
// (old shipped binaries can't be fixed server-side); plan/trialActive only
// refine the wording.
func planLabel(plan string, isPro, trialActive bool) string {
	switch {
	case !isPro:
		return "Free"
	case trialActive && plan != "pro":
		return "unarr+ (trial)"
	default:
		return "unarr+"
	}
}

// upgradeURL is where the tray's "Upgrade to unarr+" CTA points, built on the
// SAME base the account was fetched from so the CTA and the plan it advertises
// for always refer to one server.
func upgradeURL(base string) string {
	return base + "/pricing?utm_source=unarr-desktop&utm_medium=tray&utm_campaign=upgrade"
}

// accountTitle renders the disabled "Account: …" menu row for a fetched account.
func accountTitle(info *agent.AccountInfo) string {
	return "Account: " + info.Email + " — " + planLabel(info.Plan, info.IsPro, info.TrialActive)
}

// versionTitle renders the disabled "Version: …" menu row.
func versionTitle(agentVersion, appVersion string) string {
	return "Version: agent " + agentVersion + " · app " + appVersion
}

// fetchAccount asks the server who this agent belongs to. Returns the base it
// asked so the caller can build same-origin URLs.
func fetchAccount() (*agent.AccountInfo, string, error) {
	client, base, err := newAgentClient()
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountFetchTimeout)
	defer cancel()
	info, err := client.Me(ctx)
	return info, base, err
}

// binaryVersionOnce: probing the installed binary execs `unarr version`; the
// answer only changes on an upgrade, which restarts the daemon and repopulates
// the state file (the preferred source below) — so one probe per tray process
// is enough, and Once keeps it race-free across the account/crash/logs
// goroutines that all resolve versions.
var (
	binaryVersionOnce sync.Once
	binaryVersion     string
)

// resolveAgentVersion prefers the running daemon's version from the state file
// and falls back to a cached one-time probe of the installed binary.
func resolveAgentVersion() string {
	if s := readStatus(); s.version != "" {
		return s.version
	}
	binaryVersionOnce.Do(func() { binaryVersion = agentVersionBestEffort() })
	return binaryVersion
}
