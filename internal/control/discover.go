package control

import (
	"fmt"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// Discover builds a client for the daemon running on this machine, reading the
// endpoint and token from the daemon state file.
//
// Shared by the CLI (`unarr downloads`) and the desktop tray so both agree on
// what "there is no daemon to talk to" means: no state file, or a daemon old
// enough to predate the control plane. Both cases come back as ErrNoDaemon, and
// both callers then fall back to acting on the on-disk resume queue.
func Discover() (*Client, error) {
	state, err := agent.LoadState()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	if state.ControlPort == 0 || state.ControlToken == "" {
		return nil, fmt.Errorf("%w: the running daemon has no control plane (update it with `unarr update`)", ErrNoDaemon)
	}
	return NewClient(state.ControlPort, state.ControlToken), nil
}
