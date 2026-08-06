package support

// configFields classifies EVERY leaf field of config.Config.
//
// This map is the contract, and it is enforced, not documented:
// TestEveryConfigFieldIsClassified walks config.Config by reflection and fails
// the build when a field here is missing or stale. That is the whole point —
// the failure mode we are defending against is "someone adds
// `[usenet] password` in six months and nobody remembers this file exists".
// Adding a field to config.Config breaks the build until its verdict is
// written down here, and the safe verdict costs one line.
//
// Two consumers read it:
//
//   - redact_config.go publishes only what it can describe safely. Note that
//     Publishable is NOT "print the value": a Publishable free-form string
//     (a path, an agent name, a custom URL) is published as its SHAPE —
//     "set"/"unset", a vocabulary match, a count — because the bundle is meant
//     to explain a configuration, not to carry the user's directory layout or
//     hostnames into a public issue tracker.
//   - scrub.go takes the VALUES of the Secret fields and erases them from every
//     free-text section (daemon log, doctor messages, task titles). The config
//     file is not the only place a credential can surface: the daemon logs
//     URLs, and a doctor check prints the first bytes of the API key.
//
// When in doubt, mark it Secret. A missing setting costs a follow-up question;
// a leaked one cannot be un-leaked.
var configFields = map[string]Sensitivity{
	// ── Credentials ────────────────────────────────────────────────────────
	// The four values that must never leave the machine. WebDAVUsername is here
	// with the password because half a credential pair is still a credential:
	// the share is reachable from the LAN (and from the WAN when
	// webdav_allow_wan is on), so publishing the account name narrows a
	// brute-force to one unknown.
	"Auth.APIKey":             Secret,
	"Agent.Hash":              Secret,
	"Download.WebDAVPassword": Secret,
	"Download.WebDAVUsername": Secret,

	// ── Identity ───────────────────────────────────────────────────────────
	// Not credentials, but they name the user's machine and account. Published
	// as presence only; the agent ID a support engineer needs to look the
	// install up server-side is already in the doctor report the user ran.
	"Agent.ID":   Publishable,
	"Agent.Name": Publishable,

	// ── Server endpoints ───────────────────────────────────────────────────
	// "is it pointing at the stock endpoint or at something custom" is the
	// answer support needs; the custom hostname itself is not.
	"Auth.APIURL":  Publishable,
	"Auth.Mirrors": Publishable,

	// ── Downloads ──────────────────────────────────────────────────────────
	"Download.Dir":                Publishable,
	"Download.PreferredMethod":    Publishable,
	"Download.PreferredMethods":   Publishable,
	"Download.PreferredQuality":   Publishable,
	"Download.MaxConcurrent":      Publishable,
	"Download.MinFreeDiskMB":      Publishable,
	"Download.MaxDownloadSpeed":   Publishable,
	"Download.MaxUploadSpeed":     Publishable,
	"Download.SeedEnabled":        Publishable,
	"Download.SeedRatio":          Publishable,
	"Download.SeedTime":           Publishable,
	"Download.MetadataTimeout":    Publishable,
	"Download.StallTimeout":       Publishable,
	"Download.ListenPort":         Publishable,
	"Download.StreamPort":         Publishable,
	"Download.HTTPSStreamPort":    Publishable,
	"Download.EnableUPnP":         Publishable,
	"Download.AutoHTTPSUpnp":      Publishable,
	"Download.MaxStreamSessions":  Publishable,
	"Download.UsenetStreaming":    Publishable,
	"Download.RequireStreamToken": Publishable,
	"Download.CORSExtraOrigins":   Publishable,
	"Download.WebDAVEnabled":      Publishable,
	"Download.WebDAVAllowWAN":     Publishable,

	// ── Streaming / transcode ──────────────────────────────────────────────
	"Download.Transcode.Enabled":       Publishable,
	"Download.Transcode.HWAccel":       Publishable,
	"Download.Transcode.Preset":        Publishable,
	"Download.Transcode.VideoBitrate":  Publishable,
	"Download.Transcode.AudioBitrate":  Publishable,
	"Download.Transcode.MaxHeight":     Publishable,
	"Download.Transcode.MaxConcurrent": Publishable,
	"Download.HLSCache.Enabled":        Publishable,
	"Download.HLSCache.SizeGB":         Publishable,
	"Download.HLSCache.Dir":            Publishable,

	// ── VPN / funnel ───────────────────────────────────────────────────────
	// ConfigFile is a PATH to a WireGuard .conf, not the key inside it — the
	// bundle never opens that file. Presence still answers the only question
	// that matters ("self-hosted or managed?").
	"Download.VPN.Enabled":    Publishable,
	"Download.VPN.Required":   Publishable,
	"Download.VPN.ConfigFile": Publishable,
	"Download.Funnel.Enabled": Publishable,

	// ── Organize ───────────────────────────────────────────────────────────
	"Organize.Enabled":    Publishable,
	"Organize.MoviesDir":  Publishable,
	"Organize.TVShowsDir": Publishable,

	// ── Daemon ─────────────────────────────────────────────────────────────
	"Daemon.StatusInterval": Publishable,
	"Daemon.AutoUpgrade":    Publishable,
	"Daemon.Downlink":       Publishable,
	"Daemon.LogMaxSizeMB":   Publishable,
	"Daemon.LogMaxFiles":    Publishable,
	"Daemon.LogLevel":       Publishable,
	"Daemon.LogFormat":      Publishable,

	// ── Misc ───────────────────────────────────────────────────────────────
	"Notifications.Enabled": Publishable,
	"General.Country":       Publishable,
	"General.Locale":        Publishable,
	"General.NoColor":       Publishable,
	"Telemetry.Enabled":     Publishable,

	// ── Desktop companion ──────────────────────────────────────────────────
	// PlayerCommand is an arbitrary command line the user wrote. It is not a
	// credential by design, but it is free text we did not author, so it is
	// published as presence rather than verbatim.
	"Desktop.Player":        Publishable,
	"Desktop.PlayerCommand": Publishable,

	// ── Library ────────────────────────────────────────────────────────────
	"Library.ScanPath":            Publishable,
	"Library.Workers":             Publishable,
	"Library.FFprobePath":         Publishable,
	"Library.FFmpegPath":          Publishable,
	"Library.BackupDir":           Publishable,
	"Library.AutoScan":            Publishable,
	"Library.ScanInterval":        Publishable,
	"Library.AllowDelete":         Publishable,
	"Library.CacheSubtitles":      Publishable,
	"Library.CacheThumbnails":     Publishable,
	"Library.CacheKeyframes":      Publishable,
	"Library.SkipDetect":          Publishable,
	"Library.PrewarmMaxLoadRatio": Publishable,
	"Library.Trickplay.Enabled":   Publishable,
	"Library.Trickplay.Interval":  Publishable,
	"Library.Trickplay.Width":     Publishable,
	"Library.Subtitles.AutoFetch": Publishable,
	"Library.Subtitles.Languages": Publishable,

	"Library.Cleanup.Enabled":               Publishable,
	"Library.Cleanup.MinVideoBytes":         Publishable,
	"Library.Cleanup.RemoveStubs":           Publishable,
	"Library.Cleanup.RemoveOrphanPartials":  Publishable,
	"Library.Cleanup.DedupExact":            Publishable,
	"Library.Cleanup.RemoveOrphanSubtitles": Publishable,
	"Library.Cleanup.PruneEmptyDirs":        Publishable,
	"Library.Cleanup.RemoveCorruptVideos":   Publishable,
}
