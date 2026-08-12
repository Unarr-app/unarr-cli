//go:build !windows

package upgrade

// isTransientRenameBlock is always false outside Windows.
//
// POSIX rename(2) replaces the destination atomically no matter who has it
// open — an existing reader keeps its inode, it does not block the swap — so
// there is no transient failure to wait out, and retrying any error rename(2)
// does return (EXDEV, ENOENT, EACCES on the directory) would only delay a
// diagnosis that is not going to change.
func isTransientRenameBlock(error) bool { return false }
