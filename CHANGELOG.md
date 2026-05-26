# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.6] - 2026-05-26

### Added

- **auto-upgrade**: when the web flags the agent for upgrade
  (`POST /api/internal/agent/upgrade` or the "Force update now" button),
  the daemon now downloads and replaces the binary in-place, then exits so
  the service supervisor (`systemd Restart=always` on Linux, the equivalent
  on macOS/Windows) respawns on the new version. No `unarr update` step
  required from the user. Still opt-in — only fires when the server sends
  the upgrade signal.

### Changed

- The `OnUpgrade` daemon callback no longer just logs `run unarr self-update`;
  it now triggers the actual upgrade in a background goroutine.

## [0.9.5] - 2026-05-26

### Added

- **funnel**: optional CloudFlare Quick Tunnel subprocess. `unarr funnel on`
  spawns `cloudflared` as a child process and registers an anonymous
  `https://<random>.trycloudflare.com` hostname tunnelled to the daemon's
  HLS server. The hostname is reported back to the web on every sync so the
  in-browser player picks it up automatically — cross-network playback now
  works on torrentclaw.com without Tailscale or port forwarding. Bytes
  proxy through CloudFlare; TorrentClaw still doesn't relay content.
- **funnel**: on by default for fresh installs (NAS/Docker get cross-network
  HTTPS automatically); existing configs that pre-date the feature stay
  off until the operator runs `unarr funnel on`.
- **funnel**: auto-downloads cloudflared to the unarr data dir when not on
  PATH (Linux amd64/arm64/armhf/386). ELF magic + size sanity check on the
  download; `O_EXCL` partial-write so concurrent daemons don't clobber
  each other.
- **funnel**: subprocess supervisor keeps the tunnel up across cloudflared
  crashes + CF's ~6h Quick Tunnel rotation. Exponential backoff (2 s → 5 min)
  on persistent failures. The web's reported URL is cleared the moment
  cloudflared exits so an outdated hostname doesn't keep handing out 502s.
- **funnel**: `unarr funnel status` shows the live URL once registered.
  See README §`[downloads.funnel]` for the throughput / latency caveats of
  CF's free Quick Tunnels.
- **docker**: the official `torrentclaw/unarr` image now bundles
  `cloudflared` so the funnel works the moment the container starts — no
  first-run download.

### Fixed

- **hls/libx264**: bump the H.264 level we hint to libx264 by one tier so
  anamorphic (>16:9) sources stop emitting unplayable streams. 720p at
  level 3.1 silently rejected 1728×720 cinemascope frames with
  `frame MB size > level limit`; 720p now ships at level 4.0, 1080p at 4.1.
  Decoder compatibility is unaffected — every device that handles 1080p
  already handles ≥ 4.1.

## [0.9.4] - 2026-05-26

### Removed

- **streaming**: retire the custom WebRTC DataChannel pipeline. The daemon no
  longer ships pion/webrtc, the WSS signaling client, or the wire framing
  package — every in-browser session now uses HLS over HTTP from the daemon
  (Tailscale / LAN / UPnP). Browser P2P (WebTorrent) bytes never re-enabled.
- **config**: `[downloads.webrtc]` block removed from the TOML schema; existing
  config files with the section parse cleanly because go-toml ignores unknown
  sections.
- **seed_file**: `mode=seed_file` task handler + `engine.SeedFile` helper
  dropped — the last in-browser caller was retired with the WebRTC player.
- **wstracker-probe**: standalone probe binary removed.

### Changed

- **agent wire**: `SyncResponse.WebRTCSessions` (JSON: `webrtcSessions`) renamed
  to `StreamSessions` (JSON: `streamSessions`). The Go type `agent.WebRTCSession`
  is now `agent.StreamSession`. Wire-incompatible with web < 2026-05-26.
- **torrent**: `buildMagnet` no longer accepts an `extraTrackers` variadic —
  the default tracker list is the only set used.

### Fixed

- **hls**: clamp the ffmpeg `-b:v` to the bitrate cap derived from the EFFECTIVE
  output height instead of the requested quality. Previously asking for "2160p"
  on a 1080p source overshot the H.264 level we resolved from the effective
  height (4.0, max 20 Mbps) and made libx264 abort with
  `VBV bitrate > level limit`.

## [0.9.2] - 2026-05-21

### Added

- **vpn**: `unarr vpn` command (`status`, `enable`, `disable`) to manage the managed
  WireGuard split-tunnel, with `vpn status --check` to verify provisioning.
- **vpn**: report split-tunnel state (active, exit server) to the web on register
  + every sync, so the dashboard shows which agent holds the single WireGuard slot.
- **vpn**: send the agent id when fetching the VPN config so the web can arbitrate
  the single WireGuard slot — the first agent claims it; the rest are told to run
  OpenVPN on their own host (1 agent on WireGuard + up to 9 on OpenVPN).

## [0.9.1] - 2026-05-21


### Added

- **mirror**: update fallback URLs to use IPFS and remove GitHub Pages

### Fixed

- **security**: bump golang.org/x deps and add container CVE scan gate
## [0.9.0] - 2026-05-21


### Added

- **agent**: add mirror failover, agent client refactor, status 401 detection
- **vpn**: local config_file for self-hosted/personal VPN testing
- **vpn**: split-tunnel torrent traffic through managed WireGuard

### Documentation

- **docker**: refresh Docker Hub README + sync description in CI

### Fixed

- **security**: CORS allowlist, URL scheme guard, state perms, ZIP slip, mirror docs
- **security**: UPnP opt-in, bounded SSE reader, signed self-update
- **security**: harden HLS session IDs, /health disclosure, archive password handling
- **upgrade**: fetch releases from TorrentClaw app, not GitHub

### Other

- **release**: 0.9.0
## [0.8.1] - 2026-05-08


### Added

- **config**: set default values for WebRTC and transcoding in minimal TOML config
- **transcode**: dynamic H.264 level + HW probe + capability reporting

### Changed

- **streaming**: improve signal handling and remove unused components

### Fixed

- **self-update**: auto-restart live daemon after upgrade
- **streaming**: allow HLS sessions when webrtc disabled

### Other

- **gitignore**: add dist-ffbinaries to ignored files
- **release**: 0.8.1
## [0.8.0] - 2026-05-08


### Added

- **mediainfo**: ResolveFFmpeg + DownloadFFmpeg mirroring ffprobe pattern
- **release**: bundle ffmpeg + ffprobe in tarballs and Docker image
- **seed-file**: unarr-side handler for browser-on-demand seeding (Fase 4.7.c)
- **stream**: per-session quality cap from web
- **stream**: real-time transcoding for non-browser-decodable codecs
- **stream**: pion-based WebRTC byte streamer for browser playback
- **streaming**: seek-restart, single-session, idle sweeper, probe.json
- **streaming**: add HLS transport pipeline (daemon side)
- **streaming**: ffmpeg transcoding pipeline (direct play / fMP4 / HW accel)
- **torrent**: act as WebTorrent peer for browser ↔ unarr P2P streaming
- **wstracker-probe**: -seed FILE mode for browser ↔ unarr e2e validation

### Fixed

- **streaming**: bounded ffmpeg auto-restart + tmpdir gc + probe/stderr safety
- **transcoder**: force aac stereo 48khz + frag_duration for mse compat
- **transcoder**: force main profile + setparams Rec.709 + serveRange wait
- **transcoder**: correct scale filter + always force yuv420p

### Other

- **release**: 0.8.0
- **streaming**: post-review fixes — race lock, dead branch, stderr cap
- **torrent**: bump anacrolix log level Critical → Warning for visibility
## [0.7.0] - 2026-04-10


### Added

- **daemon**: enhance service management with start, stop, restart, and status commands for Windows

### Other

- **release**: 0.7.0
## [0.6.8] - 2026-04-10


### Added

- **library**: add server-driven file deletion with allow_delete config

### Other

- **release**: 0.6.8
## [0.6.7] - 2026-04-10


### Added

- **scan**: always scan downloads + organize dirs, deduplicate child paths

### Other

- **release**: 0.6.7
## [0.6.6] - 2026-04-09


### Fixed

- **docker**: switch ffprobe download from johnvansickle.com to BtbN/FFmpeg-Builds
- **stream**: fix black screen on remote/Tailscale streaming

### Other

- **release**: 0.6.6
## [0.6.5] - 2026-04-09


### Fixed

- **upgrade**: retry download on transient network errors with user feedback

### Other

- **release**: 0.6.5
## [0.6.4] - 2026-04-09


### Fixed

- **daemon**: report error status when stream path is rejected

### Other

- **release**: 0.6.4
## [0.6.3] - 2026-04-09


### Fixed

- **library**: use native arm64 ffprobe on Apple Silicon (osx-arm-64)

### Other

- **release**: 0.6.3
## [0.6.2] - 2026-04-09


### Added

- **library**: resilient scan for large libraries and better ffprobe errors

### Other

- **release**: 0.6.2
- ignore local config/ directory
## [0.6.1] - 2026-04-08


### Added

- **wake**: long-poll wake listener for instant CLI sync

### Fixed

- resolve deadlock, data races and path traversal vulnerabilities
## [0.6.0] - 2026-04-08


### Added

- **sync**: replace WS+DO transport with unified HTTP sync

### Fixed

- **ws**: add ping/pong keepalive and read deadline to detect zombie connections

### Other

- **release**: 0.6.0
## [0.5.5] - 2026-04-07


### Added

- **agent**: send stream port and IPs in register request
- **stream**: report duration and position in watch progress
- **stream**: trackingReader with byte-based progress and rate limiting

### Fixed

- **daemon**: cancel watch reporter on stream switch and re-notify ready

### Other

- **release**: 0.5.5
## [0.5.4] - 2026-04-07


### Fixed

- **stream**: use platform-specific socket options for Windows cross-compilation

### Other

- **release**: 0.5.4
## [0.5.3] - 2026-04-07


### Added

- **stream**: persistent stream server with file swapping

### Other

- **release**: 0.5.3
## [0.5.2] - 2026-04-07


### Added

- **stream**: report multi-network URLs for smart resolution

### Other

- **release**: 0.5.2
## [0.5.1] - 2026-04-07


### Added

- **daemon**: add on-demand library scan via heartbeat and WebSocket

### Fixed

- **agent**: add retry with backoff and WebSocket connect for daemon registration
- **daemon**: report failed status on stream request errors
- **daemon**: use correct systemd user target and isolate test cache
- **stream**: prevent duplicate events from killing active stream server

### Other

- **release**: 0.5.1
## [0.5.0] - 2026-04-06


### Added

- **organize**: use server metadata for file organization and subtitle handling
- **stream**: add NAT-PMP port mapping for remote downloads

### Other

- **release**: 0.5.0
- **release**: add changelog generation and release automation
## [0.4.1] - 2026-04-01


### Added

- **cli**: add login command and refactor shared helpers
- **stream**: report watch progress to API via HTTP Range tracking

### Fixed

- **ci**: fix lint errors and pin CI to Go 1.25
- **lint**: remove unused newStubCmd function

### Other

- **cli**: remove moreseed stub command
- **cli**: remove redundant stub commands (monitor, open, add, compare)
## [0.4.0] - 2026-03-31


### Added

- **cli**: upgrade command, rich status, and version cache

### Fixed

- **progress**: always report status transitions and poll for control signals
## [0.3.7] - 2026-03-31


### CI/CD

- **docker**: remove dockerhub-description sync step
## [0.3.6] - 2026-03-31


### CI/CD

- **deps**: bump docker/metadata-action from 5 to 6
- **deps**: bump docker/setup-qemu-action from 3 to 4
- **deps**: bump docker/login-action from 3 to 4
- **deps**: bump docker/build-push-action from 6 to 7
- **deps**: bump codecov/codecov-action from 5 to 6
- **docker**: add Docker Hub description sync and DOCKERHUB.md

### Fixed

- **ci**: upgrade golangci-lint to v2.11.3 for Go 1.25 support
- **docker**: upgrade alpine packages to patch CVE-2025-60876 and CVE-2026-27171
- **lint**: use default:none to disable errcheck, fix all gofmt and exhaustive
- **lint**: disable errcheck, tune gosec/exclusions for codebase state
- **lint**: configure linters for codebase maturity, fix gofmt and ineffassign
- **lint**: exclude common fire-and-forget patterns from errcheck
- **lint**: resolve errcheck and bodyclose warnings for golangci-lint v2
## [0.3.5] - 2026-03-30


### Changed

- migrate lint config to v2, remove daemon auto-upgrade, add trust badges
## [0.3.3] - 2026-03-30


### Fixed

- **ci**: remove go-client checkout steps
## [0.3.2] - 2026-03-30


### Added

- **init**: add 60s countdown, skip key, and cancel detection to browser auth

### CI/CD

- **release**: add Docker Hub publish and VirusTotal scan jobs

### Documentation

- add beta notice, fix install URLs to get.torrentclaw.com

### Fixed

- **ci**: fix virustotal job condition syntax
- **docker**: simplify Dockerfile for CI builds (no local go-client)
- **release**: disable homebrew tap (needs PAT, not GITHUB_TOKEN)

### Other

- re-enable homebrew tap in goreleaser
## [0.3.1] - 2026-03-30


### Fixed

- **build**: unused variable in Windows process check
- **release**: disable homebrew tap until repo is created

### Other

- rename module from torrentclaw-cli to unarr

### Build

- remove UPX compression (antivirus false positives, startup penalty)
## [0.3.0] - 2026-03-29


### Added

- **agent**: add WebSocket transport with HTTP fallback
- **auth**: browser-based CLI authentication (like Claude Code)
- **daemon**: add auto-scan, force start, and stall timeout default
- **debrid**: add HTTPS downloader for debrid direct URLs
- **stream**: UPnP port forwarding for remote video playback
- **usenet**: implement full NNTP download pipeline
- add migrate command, media server detection, and debrid auto-config
- replace setup with init wizard + interactive config menu
- add clean command to remove temp files, logs, and cached data
- add Sentry error reporting
- improve daemon resilience, streaming, and usenet downloads
- initial commit — unarr CLI

### Changed

- extract BuildSyncItems to library package, remove duplication

### Documentation

- improve CLI help, shell completion, and README

### Fixed

- **torrent**: expand tracker list, add DHT persistence and configurable timeouts
- force-start tasks bypass HasCapacity check in dispatch loop
- add panic recovery to auto-scan, cap DHT nodes at 200
- harden usenet/debrid downloaders from critico review

### Build

- add -s -w -trimpath to Makefile, add build-small target with UPX
[0.9.1]: https://github.com/torrentclaw/unarr/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/torrentclaw/unarr/compare/v0.8.1...v0.9.0
[0.8.1]: https://github.com/torrentclaw/unarr/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/torrentclaw/unarr/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/torrentclaw/unarr/compare/v0.6.8...v0.7.0
[0.6.8]: https://github.com/torrentclaw/unarr/compare/v0.6.7...v0.6.8
[0.6.7]: https://github.com/torrentclaw/unarr/compare/v0.6.6...v0.6.7
[0.6.6]: https://github.com/torrentclaw/unarr/compare/v0.6.5...v0.6.6
[0.6.5]: https://github.com/torrentclaw/unarr/compare/v0.6.4...v0.6.5
[0.6.4]: https://github.com/torrentclaw/unarr/compare/v0.6.3...v0.6.4
[0.6.3]: https://github.com/torrentclaw/unarr/compare/v0.6.2...v0.6.3
[0.6.2]: https://github.com/torrentclaw/unarr/compare/v0.6.1...v0.6.2
[0.6.1]: https://github.com/torrentclaw/unarr/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/torrentclaw/unarr/compare/v0.5.5...v0.6.0
[0.5.5]: https://github.com/torrentclaw/unarr/compare/v0.5.4...v0.5.5
[0.5.4]: https://github.com/torrentclaw/unarr/compare/v0.5.3...v0.5.4
[0.5.3]: https://github.com/torrentclaw/unarr/compare/v0.5.2...v0.5.3
[0.5.2]: https://github.com/torrentclaw/unarr/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/torrentclaw/unarr/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/torrentclaw/unarr/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/torrentclaw/unarr/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/torrentclaw/unarr/compare/v0.3.7...v0.4.0
[0.3.7]: https://github.com/torrentclaw/unarr/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/torrentclaw/unarr/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/torrentclaw/unarr/compare/v0.3.3...v0.3.5
[0.3.3]: https://github.com/torrentclaw/unarr/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/torrentclaw/unarr/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/torrentclaw/unarr/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/torrentclaw/unarr/releases/tag/v0.3.0

