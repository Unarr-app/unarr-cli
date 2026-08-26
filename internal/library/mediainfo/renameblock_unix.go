//go:build !windows

package mediainfo

// isTransientRenameBlock is always false outside Windows.
//
// POSIX rename(2) replaces the destination atomically no matter who has it
// open, so there is no transient failure to wait out — which is exactly why
// this whole race is invisible on Linux and macOS.
func isTransientRenameBlock(error) bool { return false }
