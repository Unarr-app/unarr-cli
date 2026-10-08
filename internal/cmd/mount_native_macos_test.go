//go:build darwin

package cmd

import "testing"

// Run this guard before any dependency/controller fixture in the Mac runner.
// It checks both real labels/domains and both real-home plist filenames.
func TestMountNativeMacFixtureGuard(t *testing.T) {
	nativeMountOptIn(t, false)
	assertNoInstalledLaunchdDaemon(t)
	t.Log("canonical and legacy real Mac labels/plists absent")
}
