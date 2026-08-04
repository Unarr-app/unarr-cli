package logging

import (
	"fmt"
	"io"
	"os"
)

// stagingSuffix names the scratch file a rotation moves the live log into
// before the ring is touched. It sits next to the log — same directory, same
// filesystem — so placing it in slot 1 afterwards is a rename and never a
// second copy. `unarr clean` already sweeps it: its `unarr.log.*` glob matches
// unarr.log.rotating as well as unarr.log.1.
const stagingSuffix = ".rotating"

// liveFileOp is the ONE part of a rotation that can fail: it moves the contents
// of the live log into staging and leaves the live file empty (renaming it
// aside, or copying it and truncating in place). It must NOT touch the ring.
//
// Contract on failure: return an error and leave the ring — and, as far as it
// can, the live file — exactly as it found them.
type liveFileOp func(live, staging string) error

// rotateThroughStaging is the single rotation primitive, shared by the owner's
// rename (Writer.rotateLocked) and the outsider's copy-truncate (RotateNow).
//
// THE INVARIANT IT EXISTS TO ENFORCE: the ring is never mutated before the
// operation on the LIVE file has succeeded. Three separate bugs came from doing
// it the other way round — shift the ring, then discover the truncate or the
// rename cannot run — and each one destroyed the history while producing
// nothing in exchange: on Windows, a log held by `unarr logs -f` or by cmd.exe
// drained all three slots in three rotations while the live file kept growing.
//
// So the order here is fixed, and the type system keeps it that way: the ring
// shift lives on `staged`, and the only way to obtain a `staged` is to get past
// the op below. There is no exported — and no free — function that shifts the
// ring, so no future caller can reintroduce the pattern by accident.
func rotateThroughStaging(path string, keep int, op liveFileOp) error {
	staging := path + stagingSuffix
	// A staging file can only be left behind by a process that died mid
	// rotation; it is not history and nothing reads it.
	_ = os.Remove(staging)

	if err := op(path, staging); err != nil {
		_ = os.Remove(staging) // a half-copied log is not history either
		return err
	}
	return staged{path: path, staging: staging, keep: keep}.commit()
}

// staged is the proof that a liveFileOp succeeded: the live log's contents are
// now in `staging` and the live path is free. Only rotateThroughStaging builds
// one, and it only builds one after the op returned nil — which is what makes
// "shift the ring before knowing the rotation works" unrepresentable.
type staged struct {
	path    string
	staging string
	keep    int
}

// commit drops the oldest slot, shifts every other slot down and moves the
// staged contents into slot 1.
//
// Only reachable through rotateThroughStaging. The per-slot renames stay
// best-effort — a slot held by a reader must not stop a daemon logging — but
// the final placement is reported, because a staging file left outside the ring
// is content the reader will never show.
func (s staged) commit() error {
	_ = os.Remove(RotatedPath(s.path, s.keep))
	for i := s.keep - 1; i >= 1; i-- {
		_ = os.Rename(RotatedPath(s.path, i), RotatedPath(s.path, i+1))
	}
	if err := os.Rename(s.staging, RotatedPath(s.path, 1)); err != nil {
		return fmt.Errorf("place rotated log: %w", err)
	}
	return nil
}

// renameLive is the liveFileOp for a log THIS process owns: move the live file
// aside in one atomic step. The caller must have closed its descriptor first —
// Go opens files without FILE_SHARE_DELETE, so Windows refuses to rename a file
// this process still holds.
func renameLive(live, staging string) error {
	return os.Rename(live, staging)
}

// copyThenTruncate is the liveFileOp for a log held by SOMEONE ELSE (launchd,
// cmd.exe's `>>`, an inherited descriptor): the holder keeps appending to the
// inode, so a rename would shrink nothing. Copy the contents aside, then
// truncate in place.
//
// The copy comes first and the truncate second, and the ring is only touched
// after both worked. A truncate that fails therefore costs one discarded
// staging file and nothing else.
func copyThenTruncate(live, staging string) error {
	if err := snapshot(live, staging); err != nil {
		return err
	}
	if err := os.Truncate(live, 0); err != nil {
		return fmt.Errorf("truncate log file: %w", err)
	}
	return nil
}

// snapshot copies src over dst, replacing whatever was there.
func snapshot(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("read log file: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, logFileMode)
	if err != nil {
		return fmt.Errorf("write rotated log: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("write rotated log: %w", err)
	}
	return out.Close()
}
