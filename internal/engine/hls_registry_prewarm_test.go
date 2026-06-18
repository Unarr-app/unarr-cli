package engine

import (
	"testing"
	"time"
)

// bare session: no ffmpeg, no tmpdir — exercises pure registry semantics.
func bareSession(id string, prewarm bool, exited bool) *HLSSession {
	s := &HLSSession{cfg: HLSSessionConfig{SessionID: id, Prewarm: prewarm}}
	s.exited = exited
	return s
}

// A prewarm registered via RegisterKeep must NOT evict the viewer's live
// session (the old Register-for-everything path killed the stream being
// watched when the next-episode prewarm got claimed mid-playback).
func TestRegisterKeepDoesNotEvict(t *testing.T) {
	r := NewHLSSessionRegistry(1)
	live := bareSession("live", false, false)
	r.Register(live)

	pre := bareSession("pre", true, false)
	r.RegisterKeep(pre)

	if r.Get("live") == nil {
		t.Fatal("RegisterKeep evicted the live session")
	}
	if r.Get("pre") == nil {
		t.Fatal("RegisterKeep did not register the prewarm")
	}
	if live.isClosed() {
		t.Fatal("RegisterKeep closed the live session")
	}

	// A REAL session via Register still evicts everything (single viewer).
	real2 := bareSession("real2", false, false)
	r.Register(real2)
	if r.Get("live") != nil || r.Get("pre") != nil {
		t.Fatal("Register must evict every other session")
	}
	if !live.isClosed() || !pre.isClosed() {
		t.Fatal("Register must close the evicted sessions")
	}
}

func TestCloseWherePrewarmsOnly(t *testing.T) {
	r := NewHLSSessionRegistry(1)
	live := bareSession("live", false, false)
	pre1 := bareSession("pre1", true, false)
	pre2 := bareSession("pre2", true, true)
	r.Register(live)
	r.RegisterKeep(pre1)
	r.RegisterKeep(pre2)

	n := r.CloseWhere(func(s *HLSSession) bool { return s.IsPrewarm() })
	if n != 2 {
		t.Fatalf("CloseWhere closed %d sessions, want 2", n)
	}
	if r.Get("live") == nil || live.isClosed() {
		t.Fatal("CloseWhere must not touch the live session")
	}
	if r.Get("pre1") != nil || r.Get("pre2") != nil {
		t.Fatal("CloseWhere must remove the prewarms from the registry")
	}
}

// With maxSessions > 1, Register keeps up to the cap and evicts only the
// least-recently-touched once the cap is exceeded (the shared/server agent).
func TestRegisterEvictsByCapLRU(t *testing.T) {
	r := NewHLSSessionRegistry(3)
	mk := func(id string, ageSec int) *HLSSession {
		s := bareSession(id, false, false)
		s.lastTouch = time.Now().Add(-time.Duration(ageSec) * time.Second)
		return s
	}
	a, b, c := mk("a", 30), mk("b", 20), mk("c", 10) // a = oldest
	r.Register(a)
	r.Register(b)
	r.Register(c)
	if r.Count() != 3 {
		t.Fatalf("3 sessions under cap 3 should all survive, got %d", r.Count())
	}
	// 4th session exceeds the cap → evict the least-recently-touched (a).
	d := mk("d", 5)
	r.Register(d)
	if r.Count() != 3 {
		t.Fatalf("after eviction want 3, got %d", r.Count())
	}
	if r.Get("a") != nil || !a.isClosed() {
		t.Fatal("least-recently-touched session a must be evicted and closed")
	}
	if r.Get("b") == nil || r.Get("c") == nil || r.Get("d") == nil {
		t.Fatal("b, c, d must survive")
	}
}

// Eviction prefers prewarm encodes over real viewer sessions even when the
// prewarm was touched more recently — a live viewer is killed last.
func TestRegisterEvictsPrewarmBeforeViewer(t *testing.T) {
	r := NewHLSSessionRegistry(2)
	live := bareSession("live", false, false)
	live.lastTouch = time.Now().Add(-60 * time.Second) // older than the prewarm
	r.Register(live)
	pre := bareSession("pre", true, false)
	pre.lastTouch = time.Now() // newest, but a prewarm
	r.RegisterKeep(pre)
	// New viewer at the cap → evict the prewarm first, keep the older live one.
	v2 := bareSession("v2", false, false)
	r.Register(v2)
	if r.Get("pre") != nil || !pre.isClosed() {
		t.Fatal("prewarm should be evicted first (before a real viewer)")
	}
	if r.Get("live") == nil {
		t.Fatal("live viewer must survive despite being older than the prewarm")
	}
	if r.Get("v2") == nil {
		t.Fatal("new viewer must be registered")
	}
}

// At the cap, a re-open / quality-change / audio-switch (a new session with the
// SAME CacheID as an existing one) must evict ITS OWN predecessor, not an
// unrelated viewer — even when the predecessor was touched more recently than
// the stranger. This is what stops an audio-switch on a full box from killing
// another user's live stream.
func TestRegisterEvictsSameContentPredecessorBeforeStranger(t *testing.T) {
	r := NewHLSSessionRegistry(2)
	mk := func(id, cacheID string, ageSec int) *HLSSession {
		s := bareSession(id, false, false)
		s.cfg.CacheID = cacheID
		s.lastTouch = time.Now().Add(-time.Duration(ageSec) * time.Second)
		return s
	}
	a := mk("a", "hashA", 1)  // predecessor of the incoming content, but NEWEST
	b := mk("b", "hashB", 60) // unrelated stranger, oldest
	r.Register(a)
	r.Register(b)
	// Incoming C re-opens hashA (audio-switch / quality-change of A's content).
	r.Register(mk("c", "hashA", 0))
	if r.Get("a") != nil || !a.isClosed() {
		t.Fatal("same-content predecessor (a) must be evicted+closed even though it was newer than the stranger")
	}
	if r.Get("b") == nil {
		t.Fatal("unrelated stranger (b) must survive — only the incoming content's predecessor is reclaimed")
	}
	if r.Get("c") == nil {
		t.Fatal("incoming session c must be registered")
	}
}

func TestHasLiveEncode(t *testing.T) {
	r := NewHLSSessionRegistry(1)
	if r.HasLiveEncode() {
		t.Fatal("empty registry must report no live encode")
	}
	done := bareSession("done", false, true) // encode finished / cache HIT
	r.Register(done)
	if r.HasLiveEncode() {
		t.Fatal("an exited encode must not count as live")
	}
	running := bareSession("running", true, false)
	r.RegisterKeep(running)
	if !r.HasLiveEncode() {
		t.Fatal("a running encode must count as live")
	}
}
