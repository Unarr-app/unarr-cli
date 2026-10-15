// Package metrics is the daemon's unified view of "what is moving right now":
// one session model for HLS streams, mounts and downloads, a stall monitor that
// emits ONE event per stall, and an opt-in Prometheus endpoint over the same
// data.
//
// The package owns no session state of its own. Sources (registered by the
// daemon) read the engine's existing registries on demand; the only thing kept
// here is what a snapshot cannot recompute — first-seen times, the previous
// byte counter (for a rate), and the stall episodes.
//
// The JSON shape of Snapshot is a published contract (GET /sessions on the
// control plane, `unarr status --sessions --json`, the web view). Additive
// changes keep SchemaVersion; anything else bumps it.
package metrics

import "time"

// SchemaVersion is the version of the Snapshot JSON schema.
const SchemaVersion = 1

// Kind is the session type. A closed set: it is also a Prometheus label value,
// so it must stay low-cardinality.
type Kind string

const (
	KindHLS      Kind = "hls"      // in-browser HLS playback (transcode / copy)
	KindStream   Kind = "stream"   // direct single-file stream being read
	KindMount    Kind = "mount"    // WebDAV/rclone mount serving reads
	KindDownload Kind = "download" // a download task the manager is running
)

// Kinds lists every Kind in a stable order (exposition order, UI order).
var Kinds = []Kind{KindDownload, KindHLS, KindStream, KindMount}

// Health is the normalised verdict of a session.
type Health string

const (
	HealthOK       Health = "ok"
	HealthDegraded Health = "degraded" // impaired, still inside the grace window
	HealthStalled  Health = "stalled"  // impaired for longer than the grace window
)

// Session is one row of the unified view.
type Session struct {
	// ID is the task id (download, stream), the HLS session id, or "mount".
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Title string `json:"title"`
	// State is kind-specific: download = the manager status ("resolving",
	// "downloading", "verifying", …); hls = "starting"|"active"|"idle";
	// stream/mount = "active".
	State  string `json:"state"`
	Health Health `json:"health"`
	// StartedAt is when the session began (first time the daemon saw it when the
	// source does not know); ActiveSec is time since StartedAt.
	StartedAt time.Time `json:"startedAt"`
	ActiveSec int64     `json:"activeSec"`
	// Bytes moved by this session (downloaded, or served to the viewer). 0 when
	// the source does not count them.
	Bytes int64 `json:"bytes"`
	// RateBps is the current rate in bytes/s; AvgRateBps is Bytes/ActiveSec.
	RateBps    int64 `json:"rateBps"`
	AvgRateBps int64 `json:"avgRateBps"`
	// SpeedX is the encoder speed in ×realtime (1.0 = exactly realtime). Set
	// only for transcoding HLS sessions that already have telemetry.
	SpeedX *float64 `json:"speedX,omitempty"`
	// Progress is 0-100, downloads only.
	Progress *int `json:"progress,omitempty"`
	// Stall is the buffering/stall history of the session.
	Stall Stall `json:"stall"`
}

// Stall summarises a session's stalls. Count is one per stall EPISODE, not per
// sampling tick (see Monitor).
type Stall struct {
	Count int `json:"count"`
	// Active is true while the session is in a stall right now.
	Active bool `json:"active"`
	// LastAt/LastReason describe the most recent stall; omitted before the first.
	LastAt     *time.Time `json:"lastAt,omitempty"`
	LastReason string     `json:"lastReason,omitempty"`
}

// VPN is the managed-VPN state.
type VPN struct {
	Enabled  bool `json:"enabled"`
	Required bool `json:"required"`
	// Up is true when the tunnel is healthy (handshake fresh).
	Up bool `json:"up"`
}

// Snapshot is the body of GET /sessions.
type Snapshot struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Agent         Agent     `json:"agent"`
	Sessions      []Session `json:"sessions"`
	// Summary is derived from Sessions/sources; it exists so a consumer need not
	// re-aggregate to draw a header.
	Summary Summary `json:"summary"`
}

// Agent identifies the daemon the snapshot came from.
type Agent struct {
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptimeSec"`
}

// Summary aggregates a snapshot. Maps are keyed by Kind / download state and are
// always present (possibly empty), never null.
type Summary struct {
	// Active counts sessions per kind (every kind present, 0 included).
	Active map[Kind]int `json:"active"`
	// Stalled counts sessions whose health is "stalled" right now.
	Stalled int `json:"stalled"`
	// BytesTotal / StallsTotal are monotonic since daemon start, per kind —
	// they keep counting after a session ends.
	BytesTotal  map[Kind]int64 `json:"bytesTotal"`
	StallsTotal map[Kind]int64 `json:"stallsTotal"`
	// RateBps sums the current rate per kind.
	RateBps map[Kind]int64 `json:"rateBps"`
	// DownloadsByState counts every known download (running or only persisted)
	// by its state.
	DownloadsByState map[string]int `json:"downloadsByState"`
	VPN              VPN            `json:"vpn"`
}

// Reading is what a Source reports for one session at one instant. The Hub
// turns it into a Session by adding what only it can know.
type Reading struct {
	Session
	// RateKnown: Session.RateBps is authoritative (e.g. a torrent's own speed).
	// Otherwise the Hub derives the rate from the Bytes delta between polls.
	RateKnown bool
	// Impaired is the source's own verdict (e.g. an encoder below realtime).
	Impaired bool
	// ExpectFlow: bytes should be moving; zero rate then counts as impaired.
	ExpectFlow bool
	// Reason names why the session is impaired, for the stall event.
	Reason string
}

// Source reports the sessions of one subsystem. Called on every poll, so it
// must be cheap and must not block.
type Source func() []Reading
