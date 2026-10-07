package cmd

import (
	"log"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// The daemon loop owns its key+ID handoff. Polling updates credentialStore and
// closes a snapshot channel; register/sync must finish before the owner applies
// a replacement to its runtime config and the sync config.
func wireDaemonCredentials(d *agent.Daemon, creds *credentialStore) {
	applied := creds.identity()
	d.CredentialChanges = func() <-chan struct{} { return applied.changed }
	d.SyncClient().CredentialChanges = d.CredentialChanges
	d.ReloadCredential = func() {
		creds.reload()
		applied = creds.identity()
		d.Client().SetAPIKey(applied.key)
		d.SetAgentID(applied.agentID)
	}
	d.OnCredentialRejectedForIdentity = creds.wipeForIdentity
	d.OnAgentKeyMintedForIdentity = func(key, id, minted string) bool {
		if !creds.adoptKeyForIdentity(key, id, minted) {
			return false
		}
		applied = creds.identity()
		log.Printf("[agent] migrated to a per-machine agent key")
		return true
	}
	d.SyncClient().OnRevokedForIdentity = func(err error, key, id string) {
		if creds.wipeForIdentity(key, id) {
			log.Printf("[agent] credential revoked by server (%v) - this machine was removed from your account", err)
		}
	}
}
