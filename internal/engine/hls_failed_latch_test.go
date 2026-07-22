package engine

import "testing"

// HLSSession.Failed() drives the daemon's "report transcode_failed to the web"
// path, so a stale latch is user-visible: the player would show a hard error for
// a session that is streaming fine.
//
// The latch is set in waitFFmpeg when the auto-restart supervisor gives up, and
// must be cleared by restartFromSegment — the single funnel every relaunch goes
// through (seek-restart AND the auto-restart supervisor AND the copy→transcode
// fallback, which relaunches via that same path).

func TestFailed_FalseWhileHealthy(t *testing.T) {
	s := &HLSSession{}
	if failed, _ := s.Failed(); failed {
		t.Fatal("a fresh session must not report Failed")
	}
}

// A crash that still has retry budget left is NOT a permanent failure: the
// supervisor is about to relaunch, and reporting an error there would fail a
// session that recovers on the next attempt.
func TestFailed_FalseWhenExitedButRestartPending(t *testing.T) {
	s := &HLSSession{}
	s.readyMu.Lock()
	s.exited = true
	s.exitErr = errFakeExit{}
	s.readyMu.Unlock()

	if failed, _ := s.Failed(); failed {
		t.Fatal("an exited-but-restarting session must not report Failed (gaveUp not latched)")
	}
}

func TestFailed_TrueAfterGivingUp(t *testing.T) {
	s := &HLSSession{}
	s.readyMu.Lock()
	s.exited = true
	s.exitErr = errFakeExit{}
	s.readyMu.Unlock()
	s.mu.Lock()
	s.gaveUp = true
	s.mu.Unlock()

	failed, err := s.Failed()
	if !failed {
		t.Fatal("a session that exhausted its restarts must report Failed")
	}
	if err == nil {
		t.Fatal("Failed must surface the last ffmpeg exit error for the reported message")
	}
}

// Regression: the copy→transcode fallback and seek-restart both reset the retry
// budget to give the session a fresh life. If gaveUp survived that reset, the
// ready-watcher would report transcode_failed for a healthy relaunched run.
func TestFailed_LatchClearedOnRelaunch(t *testing.T) {
	s := &HLSSession{}
	s.mu.Lock()
	s.gaveUp = true
	s.mu.Unlock()

	// What restartFromSegment does once the new ffmpeg is running.
	s.mu.Lock()
	s.gaveUp = false
	s.mu.Unlock()
	s.readyMu.Lock()
	s.exited = false
	s.exitErr = nil
	s.readyMu.Unlock()

	if failed, _ := s.Failed(); failed {
		t.Fatal("relaunching must clear the permanent-failure latch")
	}
}

type errFakeExit struct{}

func (errFakeExit) Error() string { return "exit status 218" }
