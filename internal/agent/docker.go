package agent

import (
	"os"
	"path/filepath"

	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

// RunningInDocker reports whether the agent process is running inside a Docker
// (or compatible OCI) container. The web uses this to swap the in-app "force
// update" button — which drives the binary self-update path that hard-stops
// inside a container (see internal/upgrade) — for a copy-paste `docker pull`
// command instead.
//
// Detection order:
//  1. UNARR_DOCKER env truthy — baked into the official image's Dockerfile, so
//     it also covers podman/containerd running our image (which don't create
//     /.dockerenv).
//  2. /.dockerenv exists — the standard marker Docker writes into every
//     container, covering images that didn't set the env.
func RunningInDocker() bool {
	switch os.Getenv("UNARR_DOCKER") {
	case "1", "true", "yes":
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// DetectInstallType classifies how this agent was installed, for the web's
// CLI-vs-desktop adoption stats. It is NOT a separate build: "desktop" simply
// means the unarr-desktop tray companion the installers drop NEXT TO the
// daemon is present on disk — the same distinction `unarr update` uses to
// decide whether to refresh a sibling (upgrade.FindDesktopSibling).
//
// Order matters: a containerised agent is reported as "docker" even in the
// unlikely event a desktop binary sits alongside it, because the update path
// (and thus the UI the web shows) is driven by the container, not the tray.
//
//	"docker"  → running inside a container
//	"desktop" → unarr-desktop present in the daemon's own directory
//	"cli"     → bare daemon, no tray companion
func DetectInstallType() string {
	if RunningInDocker() {
		return "docker"
	}
	// Same-dir only, like FindDesktopSibling itself: a PATH hit could belong to
	// an unrelated install. os.Executable can fail on exotic platforms — treat
	// an unresolved path as "no sibling" → "cli" rather than guessing.
	if exe, err := os.Executable(); err == nil {
		if _, ok := upgrade.FindDesktopSibling(filepath.Dir(exe)); ok {
			return "desktop"
		}
	}
	return "cli"
}
