package engine

import (
	"errors"
	"fmt"
)

// IntegrityError marks a finished download whose bytes don't match what was
// expected: a truncated / short file, a checksum/par2 failure, or an on-disk
// size below the advertised length. It is DISTINCT from a transport failure
// (network drop, dead tracker) — on an IntegrityError the manager re-downloads
// the SAME source a bounded number of times (a fresh, clean-start attempt), and
// only after exhausting the retries surfaces the task as damaged. This is the
// cross-backend safety net that guarantees "completed" never means a corrupt
// file (incident: 2026-06-15 debrid NFS write-back truncation — a 20 MB stub of
// a 394 MB file was marked completed because nothing re-checked the on-disk size
// after the page-cache write-back silently dropped ~374 MB).
type IntegrityError struct {
	// Reason is a stable short code surfaced to the web / logs:
	// "truncated", "size_mismatch", "empty", "par2_failed", "flush_failed".
	Reason  string
	Message string // human-readable detail
}

func (e *IntegrityError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "integrity check failed: " + e.Reason
}

// IsIntegrity reports whether err is (or wraps) an IntegrityError.
func IsIntegrity(err error) bool {
	var ie *IntegrityError
	return errors.As(err, &ie)
}

// integrityErr builds an IntegrityError with a printf-style message.
func integrityErr(reason, format string, args ...any) *IntegrityError {
	return &IntegrityError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}
