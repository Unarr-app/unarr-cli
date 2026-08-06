//go:build !windows

package postprocess

import "syscall"

// noFollowFlag makes an open() fail with ELOOP when the final path component is
// a symlink, so a link planted at the destination cannot redirect a write
// outside it. Not exposed by the os package, hence syscall.
const noFollowFlag = syscall.O_NOFOLLOW
