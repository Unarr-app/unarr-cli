//go:build darwin

package main

// macOS autostart backend: the artifact is the per-user launchd agent
// ~/Library/LaunchAgents/app.unarr.desktop.plist with RunAtLoad, which
// launchd starts at login. No launchctl load/unload needed: launchd picks the
// plist up at next login, matching the "enabled == artifact exists" contract.

import "os"

func autostartEnabled() (bool, error) {
	path, err := launchAgentPath()
	if err != nil {
		return false, err
	}
	return artifactExists(path)
}

func setAutostart(enable bool) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if !enable {
		return removeArtifact(path)
	}
	// ProgramArguments needs the running binary's own absolute path — launchd
	// does not consult PATH.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return writeArtifact(path, launchAgentPlist(exe))
}
