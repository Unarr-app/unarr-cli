package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
// runtime fault ("unexpected fault address") exits with goExitPanic, and the
// parent must read that as "the file killed the checker", i.e. corrupt.
const (
	boltExitHealthy  = 0
	boltExitCorrupt  = 10
	boltExitSkipped  = 11
	boltExitSalvaged = 12 // corrupt, but a consistent copy was rebuilt next to it
)

// goExitPanic is the exit status of a Go process that died of a panic, a
// runtime throw or an unrecovered fault. Nothing else the checker can exit
// with means "the file broke the checker": a SIGKILL from the OOM killer, a
// TerminateProcess from an antivirus or Task Manager report other statuses
// (-1, 1) and say nothing about the file.
const goExitPanic = 2

// boltCheckChildTimeout bounds the checker child. The DB is small (a few bytes
// per hashed piece, tens of MB after years) and Check + Compact are linear
// walks, so a child still running after this is stuck on I/O, not busy —
// treated as "could not check", never as corrupt.
const boltCheckChildTimeout = 60 * time.Second

// boltCheckMaxFindings caps how many Check findings are KEPT AS TEXT for the
// log. The classification (freelist-only vs. tree damage) reads the whole
// stream regardless — a cap there would let a tree finding after N freelist
// findings pass as salvageable, and Compact would copy a damaged tree into a
// clean-looking file.
const boltCheckMaxFindings = 32

// Child stdout protocol: line 1 is the status word — "ok", "corrupt",
// "skipped", or "salvaged <rebuilt-path>" — line 2 onward the reason. The
// parent trusts the exit code for the verdict and the status line for the
// rebuilt path, and treats an exit 0 WITHOUT "ok" as "no checker ran": a
// binary that ignores BoltCheckChildEnv (a root command printing help) exits 0
// too, and must not pass a corrupt file as healthy.
const (
	boltStatusOK       = "ok"
	boltStatusCorrupt  = "corrupt"
	boltStatusSkipped  = "skipped"
	boltStatusSalvaged = "salvaged"
)

type boltVerdictKind int

const (
	boltHealthy boltVerdictKind = iota
	boltCorrupt
	boltSkipped  // could not check (locked, unreadable, checker unavailable)
	boltSalvaged // corrupt, and rebuilt holds a consistent copy with every record
)

type boltVerdict struct {
	kind    boltVerdictKind
	reason  string
	rebuilt string // boltSalvaged only: the per-process rebuilt copy
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
// child, "the checker died of a panic" is just another way of saying "corrupt".
// The same goes for the salvage walk (Compact reads every page too).
func checkPieceCompletionDB(path string) boltVerdict {
	exe, err := os.Executable()
	if err != nil {
		return boltVerdict{kind: boltSkipped, reason: "cannot locate own executable: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), boltCheckChildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), BoltCheckChildEnv+"="+path)
	winproc.HideWindow(cmd)
	out, runErr := cmd.Output()
	status, reason := splitChildOutput(out)
	if runErr == nil {
		if status != boltStatusOK {
			return boltVerdict{kind: boltSkipped, reason: "no checker ran (exit 0 without a verdict): " + firstLines(string(out), 1)}
		}
		return boltVerdict{kind: boltHealthy, reason: reason}
	}
	if ctx.Err() != nil {
		return boltVerdict{kind: boltSkipped, reason: "checker timed out after " + boltCheckChildTimeout.String()}
	}
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		return boltVerdict{kind: boltSkipped, reason: "cannot run checker: " + runErr.Error()}
	}
	switch exit.ExitCode() {
	case boltExitCorrupt:
		return boltVerdict{kind: boltCorrupt, reason: reason}
	case boltExitSkipped:
		return boltVerdict{kind: boltSkipped, reason: reason}
	case boltExitSalvaged:
		rebuilt := strings.TrimSpace(strings.TrimPrefix(status, boltStatusSalvaged))
		if rebuilt == "" {
			return boltVerdict{kind: boltSkipped, reason: "checker reported a salvage without a path"}
		}
		return boltVerdict{kind: boltSalvaged, reason: reason, rebuilt: rebuilt}
	case goExitPanic:
		return boltVerdict{kind: boltCorrupt, reason: "checker died: " + firstLines(string(exit.Stderr), 2)}
	default:
		// Killed from outside (OOM killer, antivirus, Task Manager) or some
		// status this code never assigns: says nothing about the file.
		return boltVerdict{kind: boltSkipped, reason: fmt.Sprintf("checker exited %d before a verdict: %s", exit.ExitCode(), firstLines(string(exit.Stderr), 2))}
	}
}

func splitChildOutput(out []byte) (status, reason string) {
	s := strings.TrimSpace(string(out))
	status, reason, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(status), strings.TrimSpace(reason)
}

// BoltCheckMain is the checker child's body: check the bolt file at path (and
// rebuild it next to itself when only the freelist lies), print the status
// line and the reason on stdout, return the exit code. Exported for
// cmd/unarr/main.go.
func BoltCheckMain(path string) int {
	v := boltCheckInProcess(path)
	switch v.kind {
	case boltHealthy:
		fmt.Printf("%s\n%s\n", boltStatusOK, v.reason)
		return boltExitHealthy
	case boltCorrupt:
		fmt.Printf("%s\n%s\n", boltStatusCorrupt, v.reason)
		return boltExitCorrupt
	case boltSalvaged:
		fmt.Printf("%s %s\n%s\n", boltStatusSalvaged, v.rebuilt, v.reason)
		return boltExitSalvaged
	default:
		fmt.Printf("%s\n%s\n", boltStatusSkipped, v.reason)
		return boltExitSkipped
	}
}

// boltCheckInProcess inspects the file and, when every finding is about the
// freelist alone, rebuilds a consistent copy next to it so the caller can swap
// it in instead of discarding the records.
func boltCheckInProcess(path string) boltVerdict {
	v, salvageable := inspectBoltFile(path)
	if !salvageable {
		return v
	}
	rebuilt, err := salvageBoltFile(path)
	if err != nil {
		return boltVerdict{kind: boltCorrupt, reason: v.reason + "; salvage failed: " + err.Error()}
	}
	return boltVerdict{kind: boltSalvaged, reason: v.reason, rebuilt: rebuilt}
}

func boltReadOnlyOptions() *bbolt.Options {
	return &bbolt.Options{Timeout: boltLockTimeout, ReadOnly: true}
}

// inspectBoltFile opens the file read-only and runs bbolt's consistency check.
// Only bbolt's OWN verdicts count as damage: both metas invalid, a meta checksum
// mismatch, a format version mismatch, or Check findings. Everything else the
// open can fail with is about the environment, not the content — EACCES,
// ENOLCK/EOPNOTSUPP from flock on some mounts, EMFILE, a 0-byte file that
// read-only mode cannot initialise — and quarantining a healthy DB over those
// would force a full re-hash for nothing, so they are "skipped".
//
// salvageable is true only for a corrupt verdict whose EVERY finding concerns
// the freelist alone: reads never consult the freelist, so the records are
// intact and a read-only walk (Compact) recovers all of them. Any finding
// about the tree itself ("multiple references", "invalid type", key order,
// "out of bounds") means the records cannot be trusted.
func inspectBoltFile(path string) (v boltVerdict, salvageable bool) {
	db, openErr := bbolt.Open(path, 0o660, boltReadOnlyOptions())
	if openErr != nil {
		switch {
		case errors.Is(openErr, bbolterrors.ErrTimeout):
			return boltVerdict{kind: boltSkipped, reason: "locked by another process"}, false
		case errors.Is(openErr, bbolterrors.ErrInvalid),
			errors.Is(openErr, bbolterrors.ErrChecksum),
			errors.Is(openErr, bbolterrors.ErrVersionMismatch):
			return boltVerdict{kind: boltCorrupt, reason: "open: " + openErr.Error()}, false
		default:
			return boltVerdict{kind: boltSkipped, reason: "open: " + openErr.Error()}, false
		}
	}
	defer db.Close()

	var findings []string
	var treeDamage bool
	_ = db.View(func(tx *bbolt.Tx) error {
		findings, treeDamage = classifyCheckFindings(tx.Check())
		return nil
	})
	if len(findings) > 0 {
		return boltVerdict{kind: boltCorrupt, reason: "check: " + strings.Join(findings, "; ")}, !treeDamage
	}
	return boltVerdict{kind: boltHealthy, reason: "ok"}, false
}

// classifyCheckFindings drains a Check stream completely (returning early would
// leave bbolt's goroutine blocked on a tx View is about to close), keeps the
// first boltCheckMaxFindings as text, and reports whether ANY finding, kept or
// not, is about the tree rather than the freelist.
func classifyCheckFindings(ch <-chan error) (findings []string, treeDamage bool) {
	for chkErr := range ch {
		msg := chkErr.Error()
		if !isFreelistFinding(msg) {
			treeDamage = true
		}
		if len(findings) < boltCheckMaxFindings {
			findings = append(findings, msg)
		}
	}
	return findings, treeDamage
}

// isFreelistFinding recognises the three Check messages that concern the
// freelist alone (bbolt tx_check.go): a page listed twice in the freelist, a
// tree page also listed as free, and a free page the freelist does not list.
func isFreelistFinding(msg string) bool {
	return strings.Contains(msg, "already freed") ||
		strings.Contains(msg, "reachable freed") ||
		strings.Contains(msg, "unreachable unfreed")
}

// salvageBoltFile writes a consistent copy of path to a per-process rebuilt
// name via bbolt.Compact (a read-only walk of the source into a fresh file
// opened in this package's own mode) and verifies the copy before reporting
// success. Any failure removes the partial copy. The source is left untouched:
// the parent process owns the swap. The pid in the name keeps two daemons
// checking the same file (a dev agent next to the prod one) from writing, and
// deleting, each other's copy; an orphan from a crash mid-swap is reaped by
// `unarr clean`.
func salvageBoltFile(path string) (string, error) {
	rebuilt := path + PieceCompletionRebuiltSuffix + "." + strconv.Itoa(os.Getpid())
	_ = os.Remove(rebuilt) // only ever our own pid's leftover

	src, err := bbolt.Open(path, 0o660, boltReadOnlyOptions())
	if err != nil {
		return "", fmt.Errorf("reopen source: %w", err)
	}
	defer src.Close()

	dst, err := bbolt.Open(rebuilt, 0o660, boltCompletionOptions())
	if err != nil {
		return "", fmt.Errorf("create rebuilt file: %w", err)
	}
	if err := bbolt.Compact(dst, src, 1<<20); err != nil {
		dst.Close()
		_ = os.Remove(rebuilt)
		return "", fmt.Errorf("compact: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(rebuilt)
		return "", fmt.Errorf("close rebuilt file: %w", err)
	}
	if v, _ := inspectBoltFile(rebuilt); v.kind != boltHealthy {
		_ = os.Remove(rebuilt)
		return "", errors.New("rebuilt file failed its own check: " + v.reason)
	}
	return rebuilt, nil
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
