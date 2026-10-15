// Package backup snapshots and restores the daemon's local state.
//
// What is backed up is an explicit allowlist (see Layout.sources): config.toml
// and active-tasks.json, the download resume queue. Ephemeral runtime files are
// never included — daemon.state.json carries the PID and the control-plane
// token, unarr.lock is a kernel lock, certs/ holds the agent's private key.
//
// The same allowlist drives restore: destinations come from it, never from the
// names inside the archive, so a hostile archive cannot write anywhere else.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the manifest/archive layout version. Bump it on any
// incompatible change; Restore refuses a newer schema than it understands.
const SchemaVersion = 1

// ManifestName is the archive member that describes all the others.
const ManifestName = "manifest.json"

// Secrets modes recorded in the manifest.
const (
	SecretsRedacted = "redacted" // config credentials (RedactedConfigKeys) and task URLs/passwords blanked
	SecretsIncluded = "included" // verbatim copy; only written encrypted or as a local pre-restore snapshot
)

// FileEntry is one backed-up file with its integrity data.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes an archive. Restore trusts nothing in the archive until
// every file matches its entry here.
type Manifest struct {
	Schema       int         `json:"schema"`
	UnarrVersion string      `json:"unarrVersion"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	CreatedAt    time.Time   `json:"createdAt"`
	Secrets      string      `json:"secrets"`
	Files        []FileEntry `json:"files"`
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// parseManifest decodes and sanity-checks manifest.json.
func parseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrBadManifest, err)
	}
	if m.Schema < 1 {
		return m, fmt.Errorf("%w: missing schema", ErrBadManifest)
	}
	if m.Schema > SchemaVersion {
		return m, fmt.Errorf("%w: archive schema %d is newer than this unarr understands (%d)", ErrBadManifest, m.Schema, SchemaVersion)
	}
	if m.Secrets != SecretsRedacted && m.Secrets != SecretsIncluded {
		return m, fmt.Errorf("%w: unknown secrets mode %q", ErrBadManifest, m.Secrets)
	}
	return m, nil
}
