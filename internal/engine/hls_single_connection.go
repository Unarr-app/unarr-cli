package engine

import (
	"log"
	"time"
)

// singleConnectionReapWait bounds how long a single-connection Close waits for
// its ffmpeg to be reaped. The process was SIGKILLed a moment earlier, so this
// is normally milliseconds; the bound only guards a wedged process (D state on
// a dead NFS mount) from stalling the teardown — and the next session — forever.
var singleConnectionReapWait = 5 * time.Second

// waitProcsReaped blocks until every continuous ffmpeg this session spawned has
// been reaped (its provider socket closed with it), or limit elapses. Past the
// bound it kills the current process once more and gives up, logging it.
func (s *HLSSession) waitProcsReaped(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for {
		s.readyMu.Lock()
		live := s.liveProcs
		s.readyMu.Unlock()
		if live <= 0 {
			return
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			cmd := s.cmd
			s.mu.Unlock()
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			log.Printf("[hls %s] ffmpeg not reaped within %s - releasing the source anyway",
				shortHLSID(s.cfg.SessionID), limit)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
