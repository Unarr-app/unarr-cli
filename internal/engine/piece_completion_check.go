package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
	"go.etcd.io/bbolt"
	bbolterrors "go.etcd.io/bbolt/errors"
)

// BoltCheckChildEnv, when set in the environment of the unarr binary, turns the
// process into a one-shot integrity checker for the bolt file it names: it runs
// BoltCheckMain and exits with one of the boltExit* codes. cmd/unarr/main.go
// honours it before anything else (and the engine test binary does the same in
// TestMain), so checkPieceCompletionDB can re-exec itself.
const BoltCheckChildEnv = "UNARR_BOLT_CHECK"

// Exit codes of the checker child. Deliberately NOT 0/1/2: a Go panic or a
// runtime fault ("unexpected fault address") exits with 2, and the parent must
// read that as "the file killed the checker", i.e. corrupt.
const (
	boltExitHealthy  = 0
	boltExitCorrupt  = 10
	boltExitSkipped  = 11
	boltExitSalvaged = 12 // corrupt, but a consistent copy was rebuilt next to it
)

// boltCheckChildTimeout bounds the checker child. The DB is small (a few bytes
// per hashed piece, tens of MB after years) and Check + Compact are linear
// walks, so a child still running after this is stuck on I/O, not busy —
// treated as "could not check", never as corrupt.
const boltCheckChildTimeout = 60 * time.Second

// boltCheckMaxFindings caps how many Check findings are read before deciding.
// All of them must be freelist-only for a salvage; one is enough to condemn.
const boltCheckMaxFindings = 32

type boltVerdictKind int

const (
	boltHealthy boltVerdictKind = iota
	boltCorrupt
	boltSkipped  // could not check (locked, unreadable, checker unavailable)
	boltSalvaged // corrupt, and <path>.rebuilt holds a consistent copy with every record
)

type boltVerdict struct {
	kind   boltVerdictKind
	reason string
}

// checkPieceCompletionDB integrity-checks the bolt file at path in a CHILD
// PROCESS and reports the verdict.
//
// Why a child: bbolt's Check walks every page and dereferences child page ids
// read from the file before any bounds check, on a goroutine bbolt spawns
// itself. On a torn page that is an unrecoverable runtime fault (SIGBUS/SIGSEGV
// past the mmap) or an assert panic on a goroutine nothing here can recover —
// in-process it would turn the pre-flight into a deterministic boot crash with
// no quarantine, strictly worse than the crash it is meant to prevent. In a
// child, "the checker died" is just another way of saying "corrupt". The same
// goes for the salvage walk (Compact reads every page too).
func checkPieceCompletionDB(path string) boltVerdict {
	exe, err := os.Executable()
	if err != nil {
		return boltVerdict{boltSkipped, "cannot locate own executable: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), boltCheckChildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), BoltCheckChildEnv+"="+path)
	winproc.HideWindow(cmd)
	out, runErr := cmd.Output()
	msg := strings.TrimSpace(string(out))
	if runErr == nil {
		return boltVerdict{boltHealthy, msg}
	}
	if ctx.Err() != nil {
		return boltVerdict{boltSkipped, "checker timed out after " + boltCheckChildTimeout.String()}
	}
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		return boltVerdict{boltSkipped, "cannot run checker: " + runErr.Error()}
	}
	switch exit.ExitCode() {
	case boltExitCorrupt:
		return boltVerdict{boltCorrupt, msg}
	case boltExitSkipped:
		return boltVerdict{boltSkipped, msg}
	case boltExitSalvaged:
		return boltVerdict{boltSalvaged, msg}
	default:
		// A Go panic/throw (exit 2), a signal (-1), anything unexpected: the
		// file took the checker down, which is the strongest corruption signal
		// there is.
		return boltVerdict{boltCorrupt, fmt.Sprintf("checker died (%v): %s", runErr, firstLines(string(exit.Stderr), 2))}
	}
}

// BoltCheckMain is the checker child's body: check the bolt file at path (and
// rebuild it next to itself when only the freelist lies), print the reason on
// stdout, return the exit code. Exported for cmd/unarr/main.go.
func BoltCheckMain(path string) int {
	v := boltCheckInProcess(path)
	fmt.Println(v.reason)
	switch v.kind {
	case boltHealthy:
		return boltExitHealthy
	case boltCorrupt:
		return boltExitCorrupt
	case boltSalvaged:
		return boltExitSalvaged
	default:
		return boltExitSkipped
	}
}

// boltCheckInProcess inspects the file and, when every finding is about the
// freelist alone, rebuilds a consistent copy at <path>+PieceCompletionRebuiltSuffix
// so the caller can swap it in instead of discarding the records.
func boltCheckInProcess(path string) boltVerdict {
	v := inspectBoltFile(path)
	if v.kind != boltCorrupt || !freelistOnlyDamage(v.reason) {
		return v
	}
	if err := salvageBoltFile(path); err != nil {
		return boltVerdict{boltCorrupt, v.reason + "; salvage failed: " + err.Error()}
	}
	return boltVerdict{boltSalvaged, v.reason}
}

// inspectBoltFile opens the file read-only and runs bbolt's consistency check.
// Only bbolt's OWN verdicts count as damage: both metas invalid, a meta checksum
// mismatch, a format version mismatch, or Check findings. Everything else the
// open can fail with is about the environment, not the content — EACCES,
// ENOLCK/EOPNOTSUPP from flock on some mounts, EMFILE, a 0-byte file that
// read-only mode cannot initialise — and quarantining a healthy DB over those
// would force a full re-hash for nothing, so they are "skipped".
func inspectBoltFile(path string) boltVerdict {
	db, openErr := bbolt.Open(path, 0o660, &bbolt.Options{
		Timeout:  boltLockTimeout,
		ReadOnly: true,
	})
	if openErr != nil {
		switch {
		case errors.Is(openErr, bbolterrors.ErrTimeout):
			return boltVerdict{boltSkipped, "locked by another process"}
		case errors.Is(openErr, bbolterrors.ErrInvalid),
			errors.Is(openErr, bbolterrors.ErrChecksum),
			errors.Is(openErr, bbolterrors.ErrVersionMismatch):
			return boltVerdict{boltCorrupt, "open: " + openErr.Error()}
		default:
			return boltVerdict{boltSkipped, "open: " + openErr.Error()}
		}
	}
	defer db.Close()

	var findings []string
	_ = db.View(func(tx *bbolt.Tx) error {
		// Check streams every finding on an unbuffered channel from its own
		// goroutine: drain it fully (returning early would leave that goroutine
		// blocked on a tx that View is about to close).
		for chkErr := range tx.Check() {
			if len(findings) < boltCheckMaxFindings {
				findings = append(findings, chkErr.Error())
			}
		}
		return nil
	})
	if len(findings) > 0 {
		return boltVerdict{boltCorrupt, "check: " + strings.Join(findings, "; ")}
	}
	return boltVerdict{boltHealthy, "ok"}
}

// freelistOnlyDamage reports whether EVERY finding is one of the three Check
// messages that concern the freelist alone. Reads never consult the freelist,
// so the records are intact and a read-only walk (Compact) recovers all of
// them; anything about the tree itself ("multiple references", "invalid type",
// key order, "out of bounds") means the records cannot be trusted.
func freelistOnlyDamage(reason string) bool {
	reason = strings.TrimPrefix(reason, "check: ")
	if reason == "" {
		return false
	}
	for _, f := range strings.Split(reason, "; ") {
		if !strings.Contains(f, "already freed") &&
			!strings.Contains(f, "reachable freed") &&
			!strings.Contains(f, "unreachable unfreed") {
			return false
		}
	}
	return true
}

// salvageBoltFile writes a consistent copy of path to path+PieceCompletionRebuiltSuffix
// via bbolt.Compact (a read-only walk of the source into a fresh file opened in
// this package's own mode) and verifies the copy before reporting success. Any
// failure removes the partial copy. The source is left untouched: the parent
// process owns the swap.
func salvageBoltFile(path string) error {
	rebuilt := path + PieceCompletionRebuiltSuffix
	_ = os.Remove(rebuilt) // a leftover from a crash mid-swap must not be trusted

	src, err := bbolt.Open(path, 0o660, &bbolt.Options{Timeout: boltLockTimeout, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("reopen source: %w", err)
	}
	defer src.Close()

	dst, err := bbolt.Open(rebuilt, 0o660, boltCompletionOptions())
	if err != nil {
		return fmt.Errorf("create rebuilt file: %w", err)
	}
	if err := bbolt.Compact(dst, src, 1<<20); err != nil {
		dst.Close()
		_ = os.Remove(rebuilt)
		return fmt.Errorf("compact: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(rebuilt)
		return fmt.Errorf("close rebuilt file: %w", err)
	}
	if v := inspectBoltFile(rebuilt); v.kind != boltHealthy {
		_ = os.Remove(rebuilt)
		return errors.New("rebuilt file failed its own check: " + v.reason)
	}
	return nil
}

// firstLines keeps the head of a Go crash: `panic: …` / `fatal error: …` /
// `unexpected fault address …` come first, the goroutine dump after.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
