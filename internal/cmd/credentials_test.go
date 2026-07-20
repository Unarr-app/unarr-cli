package cmd

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// storeAt builds a store backed by a real config file in a temp dir.
func storeAt(t *testing.T, key, agentID string) (*credentialStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Auth.APIKey = key
	cfg.Agent.ID = agentID
	if err := config.Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	return newCredentialStore(cfg, path), path
}

func TestCredentialStoreSurvivesConcurrentUse(t *testing.T) {
	// The race this type exists to remove: the Run goroutine adopted a minted
	// key and picked up a sign-in, the sync goroutine wiped on a revocation, and
	// the auto-scan goroutine read the agent id throughout — all against one
	// unsynchronized config struct. Run with -race; without the lock this fails.
	s, _ := storeAt(t, "boot-key", "boot-agent")

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				s.adoptKey("minted-key")
			case 1:
				s.wipe()
			case 2:
				s.reload()
			case 3:
				_, _ = s.apiKey(), s.agent()
			}
		}(i)
	}
	wg.Wait()
}

func TestCredentialStoreAdoptsAMintedKey(t *testing.T) {
	s, path := storeAt(t, "general-key", "agent-1")

	s.adoptKey("per-machine-key")

	if got := s.apiKey(); got != "per-machine-key" {
		t.Errorf("apiKey() = %q, want the minted key", got)
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Auth.APIKey != "per-machine-key" {
		t.Errorf("on disk = %q — the next start would re-mint forever", onDisk.Auth.APIKey)
	}
	if onDisk.Agent.ID != "agent-1" {
		t.Errorf("adopting a key changed the agent id to %q", onDisk.Agent.ID)
	}
}

func TestCredentialStoreWipeClearsBothHalvesOfTheIdentity(t *testing.T) {
	// A tombstoned agent needs a whole new identity, not just a new key.
	s, path := storeAt(t, "dead-key", "dead-agent")

	s.wipe()

	if s.apiKey() != "" || s.agent() != "" {
		t.Errorf("in memory: key=%q agent=%q, want both cleared", s.apiKey(), s.agent())
	}
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Auth.APIKey != "" || onDisk.Agent.ID != "" {
		t.Errorf("on disk: key=%q agent=%q — a dead credential survived", onDisk.Auth.APIKey, onDisk.Agent.ID)
	}
}

func TestSavingNeverRevertsTheUsersEdits(t *testing.T) {
	// Saving used to write back the snapshot the daemon booted with, silently
	// undoing anything the user changed since. Clearing a credential must not
	// cost them their settings.
	s, path := storeAt(t, "key", "agent-1")

	// The user edits config.toml while the daemon runs.
	edited, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	edited.Download.Dir = "/mnt/somewhere-else"
	if err := config.Save(edited, path); err != nil {
		t.Fatal(err)
	}

	s.wipe()

	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Download.Dir != "/mnt/somewhere-else" {
		t.Errorf("download dir = %q, want the user's edit preserved", after.Download.Dir)
	}
	if after.Auth.APIKey != "" {
		t.Error("the credential was not cleared")
	}
}

func TestCredentialStoreReloadReportsRealChangeOnly(t *testing.T) {
	s, path := storeAt(t, "old-key", "old-agent")

	if _, _, changed := s.reload(); changed {
		t.Error("reload() reported a change when nothing moved — it would restart work for nothing")
	}

	// The user signs in: a new key AND a new identity land on disk.
	next, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Auth.APIKey = "new-key"
	next.Agent.ID = "new-agent"
	if err := config.Save(next, path); err != nil {
		t.Fatal(err)
	}

	key, agentID, changed := s.reload()
	if !changed {
		t.Fatal("reload() missed a fresh sign-in — a parked daemon would never recover")
	}
	if key != "new-key" || agentID != "new-agent" {
		t.Errorf("reload() = %q/%q, want the new key and the new identity", key, agentID)
	}
}

func TestReloadIgnoresAnEmptyOrUnreadableConfig(t *testing.T) {
	// A half-written file must not blank a working credential.
	s, path := storeAt(t, "good-key", "agent-1")

	if err := os.WriteFile(path, []byte("this is not toml {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, changed := s.reload(); changed {
		t.Error("reload() accepted an unreadable config")
	}
	if s.apiKey() != "good-key" {
		t.Errorf("apiKey() = %q — a broken file cost the daemon its credential", s.apiKey())
	}
}
