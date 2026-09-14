package fsx

import (
	"errors"

	"golang.org/x/sys/windows"
)

// IsTransientRenameBlock reports whether err is Windows refusing a rename
// because somebody else momentarily holds one of the two paths.
//
// os.Rename onto an existing destination is MoveFileEx(MOVEFILE_REPLACE_EXISTING),
// and Windows fails it while any other handle is open on either path — even a
// handle that is only reading. ERROR_SHARING_VIOLATION is the direct form;
// ERROR_ACCESS_DENIED is what a replace-existing collision reports, which is
// the one measured on the CI runner ("rename ...unarr.new ...unarr: Access is
// denied.") while a second goroutine did nothing but os.Stat the destination.
//
// The holders in the field are the same shape and just as brief: an antivirus
// scanning the executable that was written a millisecond ago, Explorer building
// a thumbnail, or unarr-desktop stat-ing the binary it polls on a timer.
func IsTransientRenameBlock(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// IsSharingViolation reports whether err is Windows refusing an open because
// another handle holds the file without sharing the requested access — and
// ONLY that: unlike IsTransientRenameBlock it does not accept
// ERROR_ACCESS_DENIED, which is also what a plain permissions problem reports.
func IsSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
