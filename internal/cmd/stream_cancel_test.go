package cmd

import (
	"context"
	"testing"
)

// resetStreamRegistry clears the package-global registry so each test starts clean.
func resetStreamRegistry(t *testing.T) {
	t.Helper()
	streamRegistry.mu.Lock()
	streamRegistry.cancels = map[string]context.CancelFunc{}
	streamRegistry.mu.Unlock()
}

// register adds a cancellable context to the registry under key and returns it.
func register(key string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	streamRegistry.mu.Lock()
	streamRegistry.cancels[key] = cancel
	streamRegistry.mu.Unlock()
	return ctx
}

func done(ctx context.Context) bool { return ctx.Err() != nil }

// TestCancelStreamTaskIsPerTask is the regression guard for the 2026-07-19 incident:
// cancelling one stream (idle timeout / displacement) must NOT cancel another task's
// live stream or in-flight streamability probe.
func TestCancelStreamTaskIsPerTask(t *testing.T) {
	resetStreamRegistry(t)

	a := register("A")            // the task being displaced/idled
	aWatch := register("watch:A") // its paired watch reporter
	b := register("B")            // an UNRELATED live stream / probe — must survive
	bWatch := register("watch:B")

	cancelStreamTask("A")

	if !done(a) {
		t.Error("cancelStreamTask(A) did not cancel A's stream context")
	}
	if !done(aWatch) {
		t.Error("cancelStreamTask(A) did not cancel A's paired watch reporter (watch:A)")
	}
	if done(b) {
		t.Error("cancelStreamTask(A) wrongly cancelled UNRELATED task B (the incident bug)")
	}
	if done(bWatch) {
		t.Error("cancelStreamTask(A) wrongly cancelled UNRELATED watch:B")
	}

	// A and watch:A must be removed from the registry; B and watch:B must remain.
	streamRegistry.mu.Lock()
	_, hasA := streamRegistry.cancels["A"]
	_, hasAWatch := streamRegistry.cancels["watch:A"]
	_, hasB := streamRegistry.cancels["B"]
	streamRegistry.mu.Unlock()
	if hasA || hasAWatch {
		t.Error("A / watch:A were not removed from the registry after cancel")
	}
	if !hasB {
		t.Error("B was removed from the registry by a per-task cancel of A")
	}
}

// TestCancelStreamTaskBlankIsNoop ensures a blank task id (srv.CurrentTaskID() with
// nothing served) never nukes the registry.
func TestCancelStreamTaskBlankIsNoop(t *testing.T) {
	resetStreamRegistry(t)
	b := register("B")

	cancelStreamTask("")

	if done(b) {
		t.Error("cancelStreamTask(\"\") cancelled a live task — must be a no-op")
	}
}

// TestCancelAllStreamContextsNukesEverything documents the shutdown-only nuke: it
// cancels every registered context.
func TestCancelAllStreamContextsNukesEverything(t *testing.T) {
	resetStreamRegistry(t)
	a := register("A")
	b := register("B")
	w := register("watch:B")

	cancelAllStreamContexts()

	if !done(a) || !done(b) || !done(w) {
		t.Error("cancelAllStreamContexts must cancel ALL registered contexts")
	}
	streamRegistry.mu.Lock()
	n := len(streamRegistry.cancels)
	streamRegistry.mu.Unlock()
	if n != 0 {
		t.Errorf("registry not drained: %d entries remain", n)
	}
}
