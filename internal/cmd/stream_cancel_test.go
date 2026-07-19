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

// TestDisplacePriorStreamsReapsNotYetServedProbe is the regression guard for the
// leak found in code review: a new stream claim must cancel a PRIOR stream that is
// still in its streamability probe (not yet SetFile, so invisible to CurrentTaskID),
// not just the currently-served one. The old cancelStreamTask(CurrentTaskID()) left
// the probing usenet stream's goroutine + NNTP handle orphaned forever.
func TestDisplacePriorStreamsReapsNotYetServedProbe(t *testing.T) {
	resetStreamRegistry(t)

	// A = a prior usenet stream still probing (never became CurrentTaskID).
	a := register("A")
	aWatch := register("watch:A")
	// B = the newly-claimed stream that displaces A. Registered by the claim loop
	// AFTER displacePriorStreams runs, so it is NOT present at displacement time —
	// but the keepID guard must still protect it if a re-claim races.
	b := register("B")
	bWatch := register("watch:B")

	displacePriorStreams("B")

	if !done(a) {
		t.Error("displacePriorStreams(B) did not cancel the prior still-probing stream A (the leak)")
	}
	if !done(aWatch) {
		t.Error("displacePriorStreams(B) did not cancel A's paired watch reporter")
	}
	if done(b) {
		t.Error("displacePriorStreams(B) wrongly cancelled the keeper B")
	}
	if done(bWatch) {
		t.Error("displacePriorStreams(B) wrongly cancelled the keeper's watch reporter watch:B")
	}

	streamRegistry.mu.Lock()
	_, hasA := streamRegistry.cancels["A"]
	_, hasAWatch := streamRegistry.cancels["watch:A"]
	_, hasB := streamRegistry.cancels["B"]
	_, hasBWatch := streamRegistry.cancels["watch:B"]
	streamRegistry.mu.Unlock()
	if hasA || hasAWatch {
		t.Error("A / watch:A not removed from the registry after displacement")
	}
	if !hasB || !hasBWatch {
		t.Error("keeper B / watch:B were wrongly removed from the registry")
	}
}

// TestDisplacePriorStreamsBlankKeepReapsAll covers displacement with no keeper (the
// claim loop calls it before the new id is registered): every prior stream goroutine
// is reaped. A blank keepID must not accidentally spare a "watch:" entry.
func TestDisplacePriorStreamsBlankKeepReapsAll(t *testing.T) {
	resetStreamRegistry(t)
	a := register("A")
	aWatch := register("watch:A")

	displacePriorStreams("")

	if !done(a) || !done(aWatch) {
		t.Error("displacePriorStreams(\"\") must reap every prior stream + watch reporter")
	}
	streamRegistry.mu.Lock()
	n := len(streamRegistry.cancels)
	streamRegistry.mu.Unlock()
	if n != 0 {
		t.Errorf("registry not drained by blank-keep displacement: %d entries remain", n)
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
