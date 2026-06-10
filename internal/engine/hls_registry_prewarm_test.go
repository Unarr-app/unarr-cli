package engine

import "testing"

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
	r := NewHLSSessionRegistry()
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
	r := NewHLSSessionRegistry()
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

func TestHasLiveEncode(t *testing.T) {
	r := NewHLSSessionRegistry()
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
