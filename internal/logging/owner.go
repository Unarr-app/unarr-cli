package logging

import (
	"errors"
	"fmt"
)

// ErrOwnedByLiveProcess is returned by RotateNow when a LIVE process owns the
// log and rotates it from the inside. Nothing was touched. Callers that can
// talk to a user (`unarr logs rotate`) match it with errors.Is and explain the
// situation instead of failing.
var ErrOwnedByLiveProcess = errors.New("log file is owned by a running process")

// Owner describes the live process that owns a log file.
type Owner struct {
	// PID is the owning process, so the message can name it.
	PID int
	// What names the owner in prose ("the unarr daemon"). Empty is fine.
	What string
}

// OwnerProbe answers "is there a LIVE process that owns this file and rotates
// it itself?" for one path. Returning ok=false means nobody does and an
// external rotation may proceed.
//
// This is EXPLICIT ownership, and it has to be: the only thing a filesystem
// probe can detect is a holder that denies write access, and a Go owner is
// never that holder — Go opens files with FILE_SHARE_WRITE on Windows and takes
// no lock at all on POSIX. A probe therefore says "go ahead" in exactly the
// case that matters, which is how `unarr self-update` came to copy-truncate the
// file the running daemon was writing to. The answer must come from the daemon
// having SAID which file it owns, plus a liveness check on that process.
//
// A stale record must never win: the probe is responsible for rejecting an
// owner whose process is gone, or a rotation blocked by a crashed daemon would
// stay blocked forever.
type OwnerProbe func(path string) (Owner, bool)

// refuseIfOwned reports the refusal an external rotation owes its caller when a
// live owner holds Path, or nil when the rotation may proceed.
func (o Options) refuseIfOwned() error {
	if o.Owner == nil {
		return nil
	}
	owner, ok := o.Owner(o.Path)
	if !ok {
		return nil
	}
	who := owner.What
	if who == "" {
		who = "a running process"
	}
	return fmt.Errorf("%w: %s is owned by %s (pid %d), which rotates it itself; "+
		"stop it first if you need to rotate from here", ErrOwnedByLiveProcess, o.Path, who, owner.PID)
}
