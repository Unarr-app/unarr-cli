package cmd

import (
	"errors"
	"io/fs"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// dirFailureEvent classifies a failure to create a configured directory into
// the lifecycle event that describes it honestly.
//
// It exists because runDaemon used to report EVERY such failure as
// permission_denied. A Windows agent whose config held `downloads.dir = D:\D:\`
// therefore filed twelve permission_denied events for
//
//	mkdir D:\D:\: The filename, directory name, or volume label syntax is incorrect
//
// which is the one diagnosis that cannot be right: no amount of
// Run-as-Administrator creates a second drive letter mid-path. A malformed
// path, a drive that is not plugged in, a read-only volume — those are the
// user's configuration to fix (config_error). Only a real EACCES/EPERM is a
// permissions problem, and mislabelling the rest buries the genuine ones in the
// server-side event counts.
func dirFailureEvent(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return agent.EventPermissionDenied
	}
	return agent.EventConfigError
}
