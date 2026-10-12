package config

import "github.com/BurntSushi/toml"

// migrateUsenetStreamingDefault turns on-the-fly Usenet streaming ON for configs
// written while it was opt-in.
//
// Why a migration and not just a new default: Save serializes every field, so any
// config the CLI ever rewrote carries an explicit `usenet_streaming = false` that
// nobody chose. Flipping Default() alone would leave exactly those users — the
// ones the web already offers "Play from Usenet" to — stuck on the batch download.
//
// The marker (`usenet_streaming_default_applied`) makes it one-shot: once a Save
// has persisted it, a later explicit `usenet_streaming = false` is a real choice
// and is respected. Default() already carries the marker, so a file that lacks it
// inherits `true` there; only the streaming value needs flipping here. The marker
// is persisted by the next Save — the daemon forces one at boot (see
// UsenetMarkerPending) so a hand-written opt-out is not re-migrated forever.
//
// The marker is tested by PRESENCE, not value: nothing in the CLI writes
// `usenet_streaming_default_applied = false`, and a hand-written one counts as
// "already applied".
func migrateUsenetStreamingDefault(cfg *Config, meta toml.MetaData) {
	if meta.IsDefined("downloads", "usenet_streaming_default_applied") {
		return
	}
	cfg.Download.UsenetStreaming = true
	cfg.usenetMarkerPending = true
}

// UsenetMarkerPending reports that Load migrated this config in memory and the
// marker is not on disk yet. Until it is saved, a user's own
// `usenet_streaming = false` would be migrated again on every start.
func (c *Config) UsenetMarkerPending() bool { return c.usenetMarkerPending }
