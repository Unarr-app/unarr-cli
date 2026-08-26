package agent

// Managed-VPN split-tunnel + P2P kill-switch state accessors for the daemon.
// Split out of daemon.go (size budget). The backing fields (vpnMu, vpnActive,
// vpnRequired, vpnMode, vpnServer) are declared on Daemon in daemon.go.

// SetVPNState records the managed-VPN split-tunnel state so it's reflected in the
// daemon state file (read by `unarr vpn status` + `unarr doctor`). Safe to call at
// startup (before Run) AND live from the reconnect supervisor when the tunnel goes
// down/up. VPNBlocking = required && !active (kill-switch on but no healthy tunnel
// → P2P currently disabled — safe, not a leak). Persists immediately so status
// reflects a live change without waiting for the next heartbeat.
func (d *Daemon) SetVPNState(active, required bool, mode, server string) {
	d.vpnMu.Lock()
	d.vpnActive = active
	d.vpnRequired = required
	d.vpnMode = mode
	d.vpnServer = server
	d.vpnMu.Unlock()
	// Mirrored into State under stateMu, AFTER vpnMu is released — the sync
	// loop writes the same struct from its own goroutine, and the two locks
	// must never nest. See the stateMu comment on Daemon.
	d.mutateState(func(st *DaemonState) {
		st.VPNActive = active
		st.VPNRequired = required
		st.VPNMode = mode
		st.VPNServer = server
		st.VPNBlocking = required && !active
	})
}

// vpnSnapshot returns the current VPN state under the lock, for the register /
// heartbeat builders that read these fields off the sync goroutine.
func (d *Daemon) vpnSnapshot() (active, required bool, mode, server string) {
	d.vpnMu.Lock()
	defer d.vpnMu.Unlock()
	return d.vpnActive, d.vpnRequired, d.vpnMode, d.vpnServer
}
