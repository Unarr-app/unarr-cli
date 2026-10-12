package cmd

import (
	"log"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// persistUsenetStreamingMarker writes the config back once when Load had to
// migrate `downloads.usenet_streaming` in memory (see
// config.Config.UsenetMarkerPending).
//
// Without it the marker only reaches disk on an unrelated Save (login, the
// one-off agent-hash mint…), which a stable, already-registered agent — typically
// a Docker/NAS one — never does. Until then a user's own
// `usenet_streaming = false` would be migrated back to true on every start.
//
// Best effort: a read-only or bind-mounted config just keeps the in-memory
// migration and logs it; it must never stop the daemon.
func persistUsenetStreamingMarker(cfg config.Config, path string) {
	if !cfg.UsenetMarkerPending() {
		return
	}
	if err := config.Save(cfg, path); err != nil {
		log.Printf("[config] usenet_streaming defaulted to ON but %s could not be updated (%v) - a manual `usenet_streaming = false` needs `usenet_streaming_default_applied = true` beside it to stick", path, err)
		return
	}
	log.Printf("[config] usenet_streaming now defaults to ON (on-the-fly Usenet playback); to opt out set usenet_streaming = false under [downloads] - recorded in %s", path)
}
