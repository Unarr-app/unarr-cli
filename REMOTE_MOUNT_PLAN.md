# Remote library implementation plan

## Project assessment

The Go CLI and desktop share configuration, daemon orchestration and download
engines. `internal/agent` is the remote control plane; `internal/control` is the
local task API. `internal/engine` handles torrent, HTTPS debrid downloads, NNTP
downloads and media playback/transcoding. `internal/library` scans and organizes
downloaded files; `internal/davfs` exposes only that on-disk library. The existing
Usenet streaming stack already implements NNTP pooling, yEnc, shared bounded
article caching, random access and uncompressed RAR4/RAR5 streaming.

Other packages cover configuration/validation, diagnostics/support and logging,
platform service management, updates, VPN/tunnel/TLS, desktop integration,
media-server discovery and Arr migration. They must retain their behavior.
Provider account enumeration was absent from the CLI: debrid download URLs are
already resolved by the web service. The mount reuses web-owned credentials and
provider clients, while keeping its durable metadata catalog on the device.

## Scope and architecture

- Add an independent `[mount]` opt-in, false when absent. Disabled operation must
  create no listener, watcher, cache or provider traffic.
- Read-only WebDAV on a loopback listener, consumed by `rclone mount`. Keep the
  existing downloaded-library `/dav/` and download/stream modes compatible.
- Web account adapters for Real-Debrid and AllDebrid enumerate completed releases;
  read-time resolution renews expiring URLs. No account deletion or repair that
  re-adds torrents implicitly.
- Include TorBox (torrent/Usenet/web libraries) and Torrin (native job cursor
  pagination). Accounts and AES-256-GCM credential storage remain exclusively in
  the existing web services. `unarr config mount` edits device-local settings.
  Remove the experimental local provider adapters, keys, credential forms and
  encryption machinery instead of maintaining two sources of configuration.
- Authenticated web endpoints expose account revisions, paginated metadata and
  user-bound signed file references. Resolve validates the current account and
  exact file; media flows straight to the CLI. Shared managed account libraries
  are excluded to prevent cross-user disclosure.
- Watch an operator-configured NZB directory. Reuse the existing NNTP/yEnc/RAR
  readers. Reject unsupported encrypted/compressed archives explicitly; never
  disguise archive bytes as a video or download a whole release implicitly.
  Fetch NNTP credentials lazily through the existing web endpoint, in memory.
- Immutable indexed filesystem snapshots: no remote I/O for Stat, HEAD or
  PROPFIND. Background refresh retains the last successful per-source snapshot
  on failure. Persist metadata for restart/outage continuity.
- Lazy, independently seekable readers; verify upstream byte ranges and lengths,
  bound retries, coalesce URL renewal, and propagate cancellation.
- CLI `mount serve` and `mount <directory>` plus configuration documentation.
  Reuse rclone's OS integration and VFS caching rather than adding a second FUSE
  implementation. The explicit command owns its processes and shutdown.

## Verification

Baseline existing WebDAV/Usenet/config/engine/CLI tests; then deterministic fake
provider/CDN/NNTP tests, DAV protocol/range/auth tests, cancellation and race
tests, persistent-catalog restart/failure tests and an actual rclone/FUSE smoke
test where the host permits it. Benchmark metadata at 1k/100k files, parallel
lookup, sequential/range reads and Usenet cached seeks. Record environment,
allocations and limitations. Run repository correctness/architecture checks and
cross-compile supported CLI platforms.

Zurg references: https://notes.debridmediamanager.com/,
https://api.real-debrid.com/, https://docs.alldebrid.com/,
https://rclone.org/commands/rclone_mount/ . A claim of faster performance than
Zurg requires both binaries against the same account/content/network/workload;
synthetic results alone do not establish that claim.
