package cmd

// A dashboard delete has to stick.
//
// Wiping the credential is not enough on a host that supplies the account key
// from the environment: `unarr up` sees a key with no identity, mints one, and
// the machine is back in the account. Under Docker that happens on any restart
// — a NAS reboot, an image update — so a delete would silently undo itself.
//
// So the wipe also leaves a marker next to the config, and `up` refuses to mint
// a new identity while it is there. Clearing it is a deliberate act: a fresh
// auth-key, UNARR_RECONNECT=1, or a sign-in.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// revokedMarker records a dashboard delete this machine has honored.
type revokedMarker struct {
	// AgentID is the tombstoned identity, for support reports.
	AgentID string    `json:"agent_id,omitempty"`
	At      time.Time `json:"at"`
}

// revokedMarkerPath sits with config.toml so a container's /config volume
// carries it across restarts and image updates alike.
func revokedMarkerPath() string {
	return filepath.Join(config.Dir(), "revoked.json")
}

// writeRevokedMarker is best-effort: the credential wipe it accompanies is the
// part that must not fail, and without the marker the worst case is the old
// behaviour (a restart reconnects).
func writeRevokedMarker(agentID string) {
	m := revokedMarker{AgentID: agentID, At: time.Now()}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(revokedMarkerPath(), data, 0o600); err != nil {
		log.Printf("[agent] could not record the revocation at %s: %v", revokedMarkerPath(), err)
	}
}

// readRevokedMarker returns the recorded delete, or nil when there is none (or
// the file is unreadable — an unparseable marker must not lock a machine out).
func readRevokedMarker() *revokedMarker {
	data, err := os.ReadFile(revokedMarkerPath())
	if err != nil {
		return nil
	}
	var m revokedMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}

// clearRevokedMarker is called by every path that mints a new identity on
// purpose — a redeemed auth-key, a sign-in, UNARR_RECONNECT — so the marker
// never outlives the delete it records.
func clearRevokedMarker() {
	os.Remove(revokedMarkerPath())
}

// reconnectRequested reports whether UNARR_RECONNECT asks `up` to override a
// recorded delete. Same truthy set as UNARR_DOCKER.
func reconnectRequested() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UNARR_RECONNECT"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
