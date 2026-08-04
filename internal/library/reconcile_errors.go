package library

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// fileFinding builds a file Finding, recording BOTH the real on-disk usage
// (Bytes, from diskUsage — allocated blocks) and the apparent size (info.Size()).
// The two diverge for sparse files (a preallocated/corrupt 1 GiB .part backed by
// ~0 blocks): Bytes reports what removal actually frees, Apparent the logical size.
func fileFinding(path string, info os.FileInfo, kind FindingKind, reason string) *Finding {
	return &Finding{
		Path: path, Kind: kind, Reason: reason,
		Bytes:    diskUsage(info),
		Apparent: info.Size(),
		IsDir:    false,
	}
}

// dirFinding builds a directory Finding, summing its children's real on-disk usage
// (Bytes — what removing the dir frees) and apparent sizes (Apparent). A dir full
// of sparse stubs can be huge on Apparent but near-zero on Bytes. Best-effort: a
// walk failure just yields 0.
func dirFinding(path string, kind FindingKind, reason string) Finding {
	var bytes, apparent int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			bytes += diskUsage(info)
			apparent += info.Size()
		}
		return nil
	})
	return Finding{Path: path, Kind: kind, Reason: reason, Bytes: bytes, Apparent: apparent, IsDir: true}
}

// dropFindingsUnderDirs removes any finding whose path lives inside a directory
// finding (which is deleted whole via RemoveAll). Dir findings themselves and
// findings outside every flagged dir are kept.
//
// Without it a per-file finding INSIDE an empty_dir/media_named_dir (an orphan
// .part, a stub, a sidecar) is double-counted — its bytes tallied once as the
// file finding and again inside its dir finding (a 25 GB .part → an impossible
// "83 GB" on a 68 GB library) — and applyFindings would then try to os.Remove a
// path RemoveAll already took. Dropping the covered children keeps the report,
// the freed-bytes total, and the apply pass honest. O(n·d) with d = number of dir
// findings, which is tiny in practice.
func dropFindingsUnderDirs(findings []Finding) []Finding {
	var dirs []string
	for _, f := range findings {
		if f.IsDir {
			dirs = append(dirs, filepath.Clean(f.Path)+string(os.PathSeparator))
		}
	}
	if len(dirs) == 0 {
		return findings
	}
	out := findings[:0:0]
	for _, f := range findings {
		if !f.IsDir && coveredByDir(filepath.Clean(f.Path), dirs) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// coveredByDir reports whether the (cleaned) file path sits inside any of the
// dir prefixes (each already terminated with a path separator).
func coveredByDir(clean string, dirs []string) bool {
	withSep := clean + string(os.PathSeparator)
	for _, d := range dirs {
		if strings.HasPrefix(withSep, d) {
			return true
		}
	}
	return false
}

// RemoveOutcome classifies why a single removal did not go through cleanly. It
// drives the actionable, user-facing guidance the CLI surfaces at the end of a
// sweep — mirroring engine/storage_error.go's philosophy: say WHAT happened and
// HOW to fix it, not just "failed".
type RemoveOutcome int

const (
	// OutcomeRemoved: the path was removed (or was already gone — idempotent).
	OutcomeRemoved RemoveOutcome = iota
	// OutcomePermission: EACCES/EPERM — the process may not own the file, or the
	// parent directory is not writable.
	OutcomePermission
	// OutcomeReadOnly: EROFS — the mount is read-only.
	OutcomeReadOnly
	// OutcomeUnreachable: ENOENT-on-parent / EIO / ESTALE — the drive/NAS is
	// disconnected or the mount went stale.
	OutcomeUnreachable
	// OutcomeDiskFull: ENOSPC — no space left to complete the operation (e.g. a
	// rename/replace that RemoveAll performs internally).
	OutcomeDiskFull
	// OutcomeOther: anything else — surfaced verbatim.
	OutcomeOther
)

// RemoveFailure is one path that could not be removed, with the classified
// outcome and a ready-to-print, actionable guidance line.
type RemoveFailure struct {
	Path     string
	Kind     FindingKind
	Outcome  RemoveOutcome
	Err      error
	Guidance string // WHAT happened + HOW to fix it (English, matches storageErr tone)
}

// RemoveSummary is the aggregate result of applyFindings: how many items were
// removed, the bytes freed, and every failure with its guidance. A single
// failing file NEVER aborts the rest of the sweep — the summary lets the caller
// (the `library clean` command, the daemon) report a complete picture.
type RemoveSummary struct {
	Removed  int
	Freed    int64
	Failures []RemoveFailure
}

// classifyRemoveError maps an os.Remove/os.RemoveAll error to an outcome + a
// human-actionable guidance string. Portable: it unwraps to syscall.Errno where
// the platform exposes it (POSIX), and falls back to the fs sentinel errors
// (os.ErrPermission) so Windows — where ERROR_ACCESS_DENIED maps to
// os.ErrPermission but not to a POSIX EACCES — still gets the permission guidance.
func classifyRemoveError(path string, err error) (RemoveOutcome, string) {
	// Read-only filesystem — check first: EROFS also satisfies ErrPermission on
	// some platforms, and its fix (remount rw) is different from a perms fix.
	if errors.Is(err, syscall.EROFS) {
		return OutcomeReadOnly, "could not delete " + path +
			": the filesystem is mounted read-only — remount it read-write (or free it from a read-only NAS export) and run the sweep again"
	}
	if errors.Is(err, syscall.ENOSPC) {
		return OutcomeDiskFull, "could not delete " + path +
			": no space left on device — the delete needs a little scratch space to complete; free some room on the target drive and retry"
	}
	// Stale/disconnected mount or lower-level I/O fault — the drive/NAS is not
	// reachable right now. ENOENT here (surfaced by RemoveAll traversing a dir
	// whose mount vanished mid-walk) is NOT the idempotent already-gone case,
	// which applyFindings handles before ever calling classify.
	if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE) || errors.Is(err, syscall.ENOTCONN) {
		return OutcomeUnreachable, "could not access " + path +
			": the drive or NAS appears disconnected or the mount went stale — check that it is connected and mounted, then run the sweep again"
	}
	// Permission denied. errors.Is(os.ErrPermission) catches both POSIX
	// EACCES/EPERM and Windows ERROR_ACCESS_DENIED.
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return OutcomePermission, "could not delete " + path +
			": permission denied — make sure this process owns the file and its parent directory is writable, or run unarr with the right permissions"
	}
	// Directory not empty is transient inside a deepest-first sweep (a flagged
	// child failed, so the parent still has contents); still actionable.
	if errors.Is(err, syscall.ENOTEMPTY) {
		return OutcomeOther, "could not delete directory " + path +
			": it is not empty — a file inside it could not be removed (see the failures above); fix those and retry"
	}
	return OutcomeOther, "could not delete " + path + ": " + err.Error()
}

// removeOne removes a single flagged path (file or dir), returning the outcome.
// An already-gone path is an idempotent success (the sweep may run twice, or the
// daemon may have moved the file). Everything else is classified for guidance.
func removeOne(f Finding) (RemoveOutcome, error) {
	clean := filepath.Clean(f.Path)
	var err error
	if f.IsDir {
		err = os.RemoveAll(clean)
	} else {
		err = os.Remove(clean)
	}
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return OutcomeRemoved, nil
	}
	return OutcomeOther, err // outcome refined by caller via classifyRemoveError
}

// applyFindings executes the removals, confined to roots, deepest-path-first so a
// flagged dir's flagged children are gone before the parent. It returns a
// RemoveSummary: a per-file failure is recorded WITH actionable guidance and the
// sweep CONTINUES — one undeletable file never strands the rest.
// isConfiguredRoot reports whether clean (already filepath.Clean'd) is exactly one
// of the configured roots — the dirs reconcile must never delete.
func isConfiguredRoot(clean string, roots []string) bool {
	for _, r := range roots {
		if clean == filepath.Clean(r) {
			return true
		}
	}
	return false
}

// confinedForRemoval is the belt-and-braces gate every path clears before removal.
// It rejects (returns false, logging why):
//   - a path outside the configured roots,
//   - a path that IS a configured root (download/movies/tv organize target),
//   - a path whose SYMLINK-RESOLVED target escapes the roots — parity with
//     delete.go's deleteOne, which resolves symlinks before confinement so a root
//     (or a parent) that is a symlink into shared space cannot be used to delete
//     outside the library. A not-yet-existing / already-gone path skips the
//     resolve (removeOne treats a missing path as an idempotent success).
func confinedForRemoval(clean string, roots []string) bool {
	if !isWithinScanPaths(clean, roots) {
		log.Printf("reconcile: refusing to remove %s - outside configured roots", clean)
		return false
	}
	// Never delete a configured dir itself: removing an empty Movies/ or TV Shows/
	// on a fresh or momentarily-empty library would break the next organize.
	if isConfiguredRoot(clean, roots) {
		log.Printf("reconcile: refusing to remove %s - it is a configured library dir", clean)
		return false
	}
	// Resolve symlinks and re-validate confinement against the RESOLVED roots. A
	// missing path is fine (idempotent removal); only a RESOLVED-but-escaping path
	// is refused. Roots are resolved too so a legitimately symlinked mount point
	// (e.g. macOS /tmp → /private/tmp) is not a false positive — it is the ESCAPE
	// that must be caught, not the mount indirection.
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return true // gone or unresolvable — removeOne handles a missing path idempotently
	}
	if !isWithinScanPaths(real, resolvedRoots(roots)) {
		log.Printf("reconcile: refusing to remove %s - resolves to %s outside configured roots", clean, real)
		return false
	}
	return true
}

// resolvedRoots returns the roots with symlinks resolved, so confinement checks on
// a resolved target compare like-for-like. A root that can't be resolved falls
// back to its cleaned form.
func resolvedRoots(roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			out[i] = real
		} else {
			out[i] = filepath.Clean(r)
		}
	}
	return out
}

func applyFindings(findings []Finding, roots []string) RemoveSummary {
	sorted := make([]Finding, len(findings))
	copy(sorted, findings)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].Path) > len(sorted[j].Path)
	})

	var summary RemoveSummary
	for _, f := range sorted {
		clean := filepath.Clean(f.Path)
		if !confinedForRemoval(clean, roots) {
			continue
		}
		outcome, err := removeOne(f)
		if err != nil {
			refined, guidance := classifyRemoveError(clean, err)
			log.Printf("reconcile: %s", guidance)
			summary.Failures = append(summary.Failures, RemoveFailure{
				Path: clean, Kind: f.Kind, Outcome: refined, Err: err, Guidance: guidance,
			})
			continue
		}
		_ = outcome
		summary.Removed++
		summary.Freed += f.Bytes
		log.Printf("reconcile: removed %s (%s: %s)", clean, f.Kind, f.Reason)
	}
	return summary
}
