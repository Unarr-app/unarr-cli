package main

// Reading the logs WITHOUT the CLI, for when the CLI is the thing that is broken.
//
// Every log the tray collects goes through `unarr daemon logs`, which means one
// exec has to succeed for a crash report to carry any evidence at all. A field
// report (windows/amd64, agent v1.11.2) came back with this as its ENTIRE log
// tail:
//
//	COULD NOT READ THE LOGS: running `unarr daemon logs` failed
//	(exit status 0xc0000142).
//
// 0xc0000142 is STATUS_DLL_INIT_FAILED: the Windows loader killed the collector
// before main(). Both reads exec the SAME binary, so the boot log — the file
// that actually holds panic dumps — was lost to the same fault, and a report
// about a dead agent arrived with zero bytes about the agent.
//
// The exec is worth keeping as the primary: the CLI knows where its own logs
// live, and on a systemd install they are in the journal, not in any file. But
// it must not be the ONLY way in. When it fails, read the files off disk
// directly — the tray already resolves config.DataDir() for other work, and the
// names are fixed by internal/cmd (logFileName, bootLogFileName).
//
// This recovers exactly the case that lost the evidence: a binary that cannot
// be exec'd (a corrupt image, an AV holding the exe) while the log files it
// wrote earlier sit there perfectly readable. It does NOT paper over a missing
// log: no file means no section, same as before.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// Log file names, fixed by internal/cmd (logFileName / bootLogFileName). They
// are duplicated rather than exported because the tray must keep building when
// paired with a CLI of a different vintage — the desktop binary and the agent
// binary ship and update independently, so a compile-time coupling here would
// be a lie about which agent is actually installed on the box.
const (
	fallbackDaemonLogName = "unarr.log"
	fallbackBootLogName   = "unarr.boot.log"
)

// fallbackLogBudget caps how much of a file the fallback reads. The report
// sections apply their own budget afterwards; this bound is about not slurping
// a multi-hundred-MB log into memory on a box where rotation is off (it is
// opt-in and off by default — see internal/cmd.rotationEnabled).
const fallbackLogBudget = 256 << 10

// readLogFileTail returns the last fallbackLogBudget bytes of path.
//
// It seeks rather than reading the whole file: an unrotated log on a long-lived
// install is routinely far larger than the budget, and the tray reads these
// from a menu click.
func readLogFileTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := fi.Size()
	if size > fallbackLogBudget {
		if _, err := f.Seek(size-fallbackLogBudget, io.SeekStart); err != nil {
			return nil, err
		}
		size = fallbackLogBudget
	}

	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	// A short read is not a failure worth discarding: the daemon appends to this
	// file while we read it, and partial evidence beats none.
	if n == 0 && err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// fallbackDaemonLog reads the daemon log straight off disk. The bool reports
// whether anything was recovered.
func fallbackDaemonLog() ([]byte, bool) {
	return readFallbackLog(fallbackDaemonLogName)
}

// fallbackBootLog reads the supervisor-held startup log straight off disk —
// the file panics land in.
func fallbackBootLog() ([]byte, bool) {
	return readFallbackLog(fallbackBootLogName)
}

func readFallbackLog(name string) ([]byte, bool) {
	dir := config.DataDir()
	if dir == "" {
		return nil, false
	}
	out, err := readLogFileTail(filepath.Join(dir, name))
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil, false
	}
	return out, true
}

// fallbackNote prefixes recovered text with why it was needed, so whoever reads
// the report knows the CLI read failed AND still gets the log — the old body
// said only the first half.
//
// A nil execErr is not the same story and must not be printed as "failed
// (<nil>)": the CLI ran fine and simply printed nothing (a foreground daemon
// whose output went to a terminal), yet the file on disk still has content.
func fallbackNote(execErr error, body []byte) []byte {
	reason := "the CLI printed nothing"
	if execErr != nil {
		reason = fmt.Sprintf("running the CLI failed (%v)", execErr)
	}
	head := fmt.Sprintf("[read directly from %s: %s]\n", config.DataDir(), reason)
	return append([]byte(head), body...)
}
