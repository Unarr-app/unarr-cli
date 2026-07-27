//go:build !windows

package winproc

import "os/exec"

// HideWindow is a no-op off Windows: only Windows allocates a console window
// for a console child spawned by a windowless parent. Kept so call sites are
// cross-platform without build tags of their own.
func HideWindow(_ *exec.Cmd) {}
