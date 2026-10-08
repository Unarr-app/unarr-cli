package agent

import (
	"context"
	"errors"
)

// ErrIdentityChanged asks the daemon owner to end its old register/sync cycle
// and adopt the current saved identity before beginning another cycle.
var ErrIdentityChanged = errors.New("agent identity changed")

type requestCredentialKey struct{}
type requestIdentityChanges struct{}

func requestIdentityChanged(ctx context.Context) bool {
	changes, _ := ctx.Value(requestIdentityChanges{}).(<-chan struct{})
	return identityChanged(changes)
}

func (d *Daemon) registerCurrentIdentity(ctx context.Context, park bool) (resultErr error) {
	// Only the daemon owner applies key/ID; the poll touches credentialStore.
	if d.ReloadCredential != nil {
		d.ReloadCredential()
	}
	var changes <-chan struct{}
	if d.CredentialChanges != nil {
		changes = d.CredentialChanges()
	}
	ctx = context.WithValue(ctx, requestIdentityChanges{}, changes)
	ctx, stop := credentialLifetime(ctx, changes)
	defer stop()
	defer func() {
		if resultErr != nil && identityChanged(changes) {
			resultErr = ErrIdentityChanged
		}
	}()
	return d.register(ctx, park)
}

func (d *Daemon) adoptRegistrationKey(resp *RegisterResponse) error {
	if resp.AgentKey == "" {
		return nil
	}
	if d.OnAgentKeyMintedForIdentity != nil {
		if !d.OnAgentKeyMintedForIdentity(resp.credentialKey, resp.agentID, resp.AgentKey) {
			return ErrIdentityChanged
		}
		d.client.SetAPIKey(resp.AgentKey)
	} else if d.OnAgentKeyMinted != nil {
		d.OnAgentKeyMinted(resp.AgentKey)
	}
	return nil
}

func credentialRequestContext(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, requestCredentialKey{}, key)
}

func identityChanged(changes <-chan struct{}) bool {
	select {
	case <-changes:
		return true
	default:
		return false
	}
}

// Both the watcher and its request are joined by the caller. A nil channel is
// the legacy path without a mutable credential owner.
func credentialLifetime(ctx context.Context, changes <-chan struct{}) (context.Context, func()) {
	if changes == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-changes:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(); <-done }
}
