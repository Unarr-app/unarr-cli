//go:build !windows

package cmd

// stopSupervisor is a no-op off Windows.
//
// Linux and macOS have a real service manager, and `unarr stop` already routes
// through it (see runStop → service.Respawns): systemctl and launchctl stop the
// unit, which stops whatever the unit is running. Only Windows lacks that — its
// supervisor is the VBScript launcher shim, which no service manager knows how
// to address, so stopping it needs the scheduled-task escape hatch.
func stopSupervisor() {}
