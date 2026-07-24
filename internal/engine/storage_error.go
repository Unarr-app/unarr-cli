package engine

import (
	"errors"
	"fmt"
)

// StorageError marks a download whose BYTES were fine but could not be PERSISTED
// to the target directory: an fsync/close that returned an I/O error, a stalled
// or dropped network mount (NFS/SMB soft-mount timing out), a read-only or
// disconnected volume. It is DELIBERATELY distinct from IntegrityError:
//
//   - IntegrityError = the bytes are wrong (truncated / short / checksum / par2).
//     Re-downloading the SAME source usually lands intact, so the manager retries
//     it several times before giving up.
//   - StorageError = the destination failed. Re-downloading is pointless — the
//     next attempt writes to the same broken mount — and burns the user's
//     bandwidth (a debrid re-fetch re-pulls the whole file) only to end in a
//     misleading "corrupt download". So the manager retries ONCE (a mount can be
//     momentarily unresponsive and recover), then PAUSES the task as resumable
//     and surfaces a storage-specific message the web renders as "check your
//     download folder / NAS", not "corrupt".
//
// The split exists because ~any user pointing their download dir at a network
// share (the prod dir is an NFS mount at /mnt/nas/peliculas; users self-host the
// same way) hits a transient mount stall as a write-back error at fsync time —
// which the old code classified as flush_failed integrity corruption and looped
// on. Incident 2026-07-24: a debrid download of a healthy file re-downloaded in
// a loop because the NFS server (soft-mount, timeo=30) timed out on the final
// fsync of a large .mkv.
type StorageError struct {
	// Reason is a stable short code surfaced to the web / logs:
	// "mkdir_failed", "open_failed", "flush_failed" (fsync error),
	// "close_failed" (close error), "stat_failed" (read-back stat faulted).
	Reason  string
	Dir     string // target directory, when known — the actionable part for the user
	Message string // human-readable detail (includes the underlying OS error)
}

func (e *StorageError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "storage write failed: " + e.Reason
}

// IsStorage reports whether err is (or wraps) a StorageError.
func IsStorage(err error) bool {
	var se *StorageError
	return errors.As(err, &se)
}

// storageErr builds a StorageError with a printf-style message. Dir is optional
// (pass "" when the caller has no path handy); it just enriches the surfaced text.
func storageErr(reason, dir, format string, args ...any) *StorageError {
	return &StorageError{Reason: reason, Dir: dir, Message: fmt.Sprintf(format, args...)}
}
