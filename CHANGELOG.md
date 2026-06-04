# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.4-beta] - 2026-06-04


### Fixed

- **stream**: self-heal host→container path skew in HLS + sidecar handlers
## [1.0.3-beta] - 2026-06-04


### Fixed

- **trickplay**: stop scan-time sprite generation from saturating the host

### Other

- **release**: 1.0.3-beta
## [1.0.2-beta] - 2026-06-03


### Added

- **stream**: debrid passthrough for mode=stream tasks (external players)
- **trickplay**: scan-time montage sprite for the web scrubber

### Fixed

- **release**: keep prerelease suffix in docker smoke-check version compare
## [1.0.1-beta] - 2026-06-03


### Added

- **agent**: report isDocker so the web shows a docker pull command
- **release**: sign release checksums (ed25519), enforce + bake pubkey

### Fixed

- **stream**: retry thumbnail extraction with output-seek on seek-index failure
- **stream**: clamp out-of-range audio-track index to 0:a:0

### Other

- **release**: 1.0.1-beta
## [1.0.0-beta] - 2026-06-03


### Added

- **agent**: event-driven uplink — sync on every state transition
- **agent**: hybrid SSE downlink with long-poll fallback
- **agent**: give the public API client mirror failover
- **agent**: auto-resume interrupted downloads after a daemon restart
- **docker**: glibc base with nvenc ffmpeg + par2/7z extractors
- **downloads**: pre-flight free-disk guard before each download (hueco medio)
- **library**: content fingerprint + path-resilient sync + stream self-heal
- **library**: detect corrupt/incomplete files during scan
- **seeding**: wire seed ratio/time lifecycle into the torrent daemon
- **stream**: enable GPU libplacebo in prod image + gate to real GPU
- **stream**: benchmark software encode ceiling at startup
- **stream**: GPU HDR tonemap via libplacebo
- **stream**: /speedtest endpoint for agent-path bandwidth probing
- **stream**: cache scan-time thumbnail frames to the .unarr sidecar
- **stream**: cache extracted subtitles to a hidden .unarr sidecar
- **stream**: serve embedded text subtitles as on-demand WebVTT
- **stream**: optional per-agent HTTPS listener with hot-reloadable cert
- **stream**: burn bitmap (PGS/DVB) subtitles into the video via overlay
- **stream**: bitrate-sized readahead for play-while-download
- **stream**: on-demand frame thumbnails via /thumbnail (hueco medio)
- **stream**: refresh expired debrid links mid-stream (hueco #2/2c)
- **stream**: transcode debrid sources to HLS from a URL (hueco #2/2b)
- **stream**: serve /stream from a debrid HTTPS link (hueco #2/2a)
- **stream**: device-aware remux (HEVC/AV1 + non-aac audio) + TTFF timers
- **stream**: progressive fMP4 remux source for /stream (hueco #3 / 3b-i)
- **stream**: direct-play passthrough for browser-native files
- **stream**: authenticate /stream and /hls with signed tokens
- **transcode**: tonemap HDR sources to SDR (zscale-gated)

### Documentation

- **docker**: add docker-compose.yml for one-command setup
- **roadmap**: close the realtime hueco + mark Tailscale-Funnel note stale
- **roadmap**: mark unarr localized-route 404 fixed
- **roadmap**: mark hueco #2 closed (2a+2b+2c)
- **roadmap**: mark hueco #2/2b (HLS-from-URL) closed
- **roadmap**: hueco #3 fully closed — 3d resolved as 3d-lite auto-downshift
- **roadmap**: hueco #3 3c closed (capability negotiation) + TTFF diagnosis
- **roadmap**: hueco #3 phase 3b closed (progressive fMP4 remux) + smoke
- **roadmap**: 3b approach = progressive fMP4 remux via /stream
- **roadmap**: hueco #3 3a smoke e2e passed + brand-isolation fix noted
- **roadmap**: add hueco #4 (pre-transcode on download) design
- **roadmap**: hueco #3 phase 3a closed (direct-play)
- **roadmap**: design hueco #3 (device-profile + direct-play + ABR)
- **roadmap**: design hueco #2 (debrid in the streaming path)

### Fixed

- **agent**: surface par2/install/NFS failures instead of degrading silently
- **stream**: don't cache transient libplacebo probe timeouts
- **stream**: functional libplacebo probe + benchmark hardening
- **stream**: clean HLS segments — no B-frames, no scene-cut, CFR
- **stream**: report stream failures via StreamError + retry transient stat
- **stream**: honor client network-caching in the M3U playlist
- **stream**: /critico review fixes for the sidecar cache
- **stream**: derive H.264 level from frame macroblocks, not height
- **stream**: derive H.264 level from frame macroblocks, not height
- **stream**: allow unarr.app origins for /stream + /hls CORS

### Other

- **release**: 1.0.0-beta
- **release**: 1.0.0-beta
- bump version to 0.10.0 (direct-play floor; local build only, no publish)

### Performance

- **stream**: run the subtitle/thumbnail prewarm at idle I/O priority
- **stream**: extract all text subtitles of a file in one ffmpeg pass
## [0.9.19] - 2026-05-30


### Fixed

- **docker**: three streaming/reliability bugs found in live docker test

### Other

- **release**: 0.9.19
## [0.9.18] - 2026-05-29


### Fixed

- **stream**: make completed torrent files readable (mmap creates 0000)

### Other

- **release**: 0.9.18
## [0.9.17] - 2026-05-27


### Added

- **scripts**: prune Forgejo releases >90 days in ship.sh

### Fixed

- **hls**: drop nvenc -tune ll — kills hls segmentation, bump 0.9.17

### Other

- **release**: 0.9.17
## [0.9.15] - 2026-05-27


### Added

- **sentry**: enhance error handling by skipping user input errors in CaptureError

### Changed

- **ci**: point Forgejo URLs at torrentclaw org (post-transfer)
- **sentry**: decouple agent import via string-match, rename predicate

### Documentation

- **positioning**: reframe unarr around download/stream/transcode, drop misleading search-first wording

### Fixed

- **ci**: unset GITHUB_TOKEN so goreleaser uses GITEA_TOKEN
- **sentry**: skip "daemon not running" stop/reload errors

### Other

- **release**: 0.9.15
- **scripts**: harden release.sh against double-release and inline version bumps
- untrack .claude/ (private local config)
## [0.9.14] - 2026-05-27


### Added

- **vaapi**: hybrid CPU-scale + hwupload encode path (QW2, 0.9.14)

### CI/CD

- port workflows from .github/ to .forgejo/ (Forgejo Actions)

### Fixed

- **daemon**: defensive IsClosed check in watchSessionReady poll loop
- **daemon**: use parent ctx for MarkSessionReady so cancel propagates
- **release**: move gitea_urls to top-level (goreleaser v2 schema)
## [0.9.13] - 2026-05-27


### Added

- **agent**: session-ready webhook for SSE-driven player handshake (0.9.13)
- **agent**: send full transcoder diagnostic in register payload (0.9.12)

### Fixed

- **daemon**: defer probeCancel so a panic mid-diagnostic still releases ctx

### Other

- **release**: add ship.sh end-to-end pipeline as GH Actions backup
- **skills**: add /publish slash command + allow .claude/ in git
## [0.9.11] - 2026-05-27


### Added

- **hls**: pre-segmentación delantada — 2 s segments + async session start (0.9.10)
- **hls**: faster first-start — probe cache + tighter encoder presets (0.9.9)

### Changed

- **hls**: critico-driven hardening of fase 3.2

### Fixed

- **cors**: allow play from .to / staging / onion mirrors
- **library**: classify resolution by width + height, not height alone
- **transcode**: make preset libx264-only + restore quality opt-in

### Other

- **release**: 0.9.11
## [0.9.8] - 2026-05-27


### Fixed

- **upgrade**: break auto-apply restart loop (0.9.8)
## [0.9.7] - 2026-05-26


### Added

- **hls**: persistent fMP4 segment cache + integrity + stats (0.9.7)
## [0.9.6] - 2026-05-26


### Added

- **daemon**: auto-apply upgrades when server signals (0.9.6)
## [0.9.5] - 2026-05-26


### Added

- **funnel**: cloudflare quick tunnel embedded subprocess (0.9.5)
## [0.9.4] - 2026-05-26


### Added

- **stream**: retire WebRTC, HLS-only, bump 0.9.4 (**BREAKING**)
## [0.9.3] - 2026-05-26


### Added

- **usenet**: warn at startup when par2 or extractor is missing

### Fixed

- **engine**: truncate errorMessage before reporting status
- **hls**: clamp ffmpeg bitrate to the level we derive from outputHeight
## [0.9.2] - 2026-05-22


### Added

- **vpn**: unarr vpn command + report/arbitrate the WireGuard slot
## [0.9.1] - 2026-05-21


### Added

- **mirror**: update fallback URLs to use IPFS and remove GitHub Pages

### Fixed

- **security**: bump golang.org/x deps and add container CVE scan gate

### Other

- **release**: 0.9.1
## [0.9.0] - 2026-05-21


### Added

- **agent**: add mirror failover, agent client refactor, status 401 detection
- **vpn**: local config_file for self-hosted/personal VPN testing
- **vpn**: split-tunnel torrent traffic through managed WireGuard

### CI/CD

- deploy install scripts to GitHub Pages

### Documentation

- **docker**: refresh Docker Hub README + sync description in CI

### Fixed

- **security**: CORS allowlist, URL scheme guard, state perms, ZIP slip, mirror docs
- **security**: UPnP opt-in, bounded SSE reader, signed self-update
- **security**: harden HLS session IDs, /health disclosure, archive password handling
- **upgrade**: fetch releases from TorrentClaw app, not GitHub

### Other

- **pages**: add .nojekyll to disable Jekyll processing
- **pages**: set custom domain unarr.torrentclaw.com
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
[1.0.4-beta]: https://github.com/torrentclaw/unarr/compare/v1.0.3-beta...v1.0.4-beta
[1.0.3-beta]: https://github.com/torrentclaw/unarr/compare/v1.0.2-beta...v1.0.3-beta
[1.0.2-beta]: https://github.com/torrentclaw/unarr/compare/v1.0.1-beta...v1.0.2-beta
[1.0.1-beta]: https://github.com/torrentclaw/unarr/compare/v1.0.0-beta...v1.0.1-beta
[1.0.0-beta]: https://github.com/torrentclaw/unarr/compare/v0.9.19...v1.0.0-beta
[0.9.19]: https://github.com/torrentclaw/unarr/compare/v0.9.18...v0.9.19
[0.9.18]: https://github.com/torrentclaw/unarr/compare/v0.9.17...v0.9.18
[0.9.17]: https://github.com/torrentclaw/unarr/compare/v0.9.15...v0.9.17
[0.9.15]: https://github.com/torrentclaw/unarr/compare/v0.9.14...v0.9.15
[0.9.14]: https://github.com/torrentclaw/unarr/compare/v0.9.13...v0.9.14
[0.9.13]: https://github.com/torrentclaw/unarr/compare/v0.9.11...v0.9.13
[0.9.11]: https://github.com/torrentclaw/unarr/compare/v0.9.8...v0.9.11
[0.9.8]: https://github.com/torrentclaw/unarr/compare/v0.9.7...v0.9.8
[0.9.7]: https://github.com/torrentclaw/unarr/compare/v0.9.6...v0.9.7
[0.9.6]: https://github.com/torrentclaw/unarr/compare/v0.9.5...v0.9.6
[0.9.5]: https://github.com/torrentclaw/unarr/compare/v0.9.4...v0.9.5
[0.9.4]: https://github.com/torrentclaw/unarr/compare/v0.9.3...v0.9.4
[0.9.3]: https://github.com/torrentclaw/unarr/compare/v0.9.2...v0.9.3
[0.9.2]: https://github.com/torrentclaw/unarr/compare/v0.9.1...v0.9.2
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

