package mediainfo

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientRenameBlock reports whether err is Windows refusing a rename
// because somebody else momentarily holds one of the two paths.
//
// Same predicate, and the same reasoning, as internal/upgrade's copy: os.Rename
// onto an existing destination is MoveFileEx(MOVEFILE_REPLACE_EXISTING), which
// Windows fails while any other handle is open on either path — even a
// read-only one. ERROR_SHARING_VIOLATION is the direct form;
// ERROR_ACCESS_DENIED is what a replace-existing collision reports.
//
// It is duplicated rather than shared because the two packages are otherwise
// unrelated, and a mediainfo -> upgrade import to reach four lines of syscall
// constants would couple the tool downloader to the self-updater. If a third
// user appears, that is the point to lift it into its own package.
func isTransientRenameBlock(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
