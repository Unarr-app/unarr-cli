package agent

import "os"

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
