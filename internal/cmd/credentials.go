package cmd

// Ownership of the daemon's mutable credentials.
//
// The daemon booted with a config snapshot and then mutated the auth fields of
// that snapshot from whichever goroutine happened to need it: the Run goroutine
// adopted a freshly-minted per-machine key and picked up a new sign-in while
// parked, the sync goroutine wiped the credential on a revocation, and the
// auto-scan goroutine read the agent id the whole time. Same struct, three
// goroutines, no synchronization — a data race on the values that decide
// whether this machine can talk to the server at all.
//
// So the mutable half moves out of the config and gets a single owner. Every
// read and write goes through here under one lock, and the config snapshot goes
// back to being what it should always have been: read-only after boot.
//
// Persistence lives here too, for the same reason it was worth unifying: saving
// re-reads config.toml first, so clearing a credential never reverts an edit the
// user made after the daemon started.

import (
	"log"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// credentialStore owns the identity this daemon authenticates with.
type credentialStore struct {
	mu      sync.Mutex
	key     string
	agentID string
	// path is where the credential is persisted. Captured once: the same file
	// the daemon booted from, whatever --config or UNARR_CONFIG_DIR resolved to.
	path string
}

func newCredentialStore(cfg config.Config, path string) *credentialStore {
	return &credentialStore{key: cfg.Auth.APIKey, agentID: cfg.Agent.ID, path: path}
}

// apiKey returns the credential currently in force.
func (s *credentialStore) apiKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key
}

// agent returns the identity currently in force. Read by anything that names
// this machine to the server after boot — a wipe can change it underneath.
func (s *credentialStore) agent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}

// adoptKey records and persists a per-machine key the server just minted,
// replacing the general/legacy key the daemon registered with.
func (s *credentialStore) adoptKey(newKey string) {
	s.mu.Lock()
	s.key = newKey
	s.mu.Unlock()
	s.persist(func(c *config.Config) { c.Auth.APIKey = newKey })
}

// wipe clears a credential the server has tombstoned, so the next sign-in mints
// a fresh identity instead of re-offering one that will never be accepted again.
func (s *credentialStore) wipe() {
	s.mu.Lock()
	s.key = ""
	s.agentID = ""
	s.mu.Unlock()
	s.persist(func(c *config.Config) {
		c.Auth.APIKey = ""
		c.Agent.ID = ""
	})
}

// reload re-reads the credential from disk and reports whether it changed.
//
// This is how a parked daemon recovers: signing in from the tray rewrites
// config.toml, and without picking that up the daemon would keep offering the
// rejected key forever, making a successful sign-in look like it did nothing.
// The agent id comes along because a sign-in after a revocation mints BOTH —
// re-registering with the tombstoned id would be refused no matter how good the
// new key is.
func (s *credentialStore) reload() (key, agentID string, changed bool) {
	fresh, err := config.Load(s.path)
	if err != nil {
		log.Printf("[agent] could not re-read %s: %v", s.path, err)
		return "", "", false
	}
	fresh.ApplyEnvOverrides()
	if fresh.Auth.APIKey == "" {
		return "", "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if fresh.Auth.APIKey == s.key && fresh.Agent.ID == s.agentID {
		return s.key, s.agentID, false
	}
	s.key = fresh.Auth.APIKey
	if fresh.Agent.ID != "" {
		s.agentID = fresh.Agent.ID
	}
	return s.key, s.agentID, true
}

// persist applies a change to the config file. It re-reads the file first
// rather than writing back the snapshot this process booted with, which would
// silently revert every edit the user has made since — clearing a credential
// must not cost them their settings.
func (s *credentialStore) persist(apply func(*config.Config)) {
	onDisk, err := config.Load(s.path)
	if err != nil {
		log.Printf("[agent] could not re-read %s before saving (%v) - credential kept in memory only", s.path, err)
		return
	}
	apply(&onDisk)
	if err := config.Save(onDisk, s.path); err != nil {
		log.Printf("[agent] could not save %s (the old credential stays on disk): %v", s.path, err)
	}
}
