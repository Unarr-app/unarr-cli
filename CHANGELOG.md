# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.2] - 2026-04-09


### Added

- **library**: resilient scan for large libraries and better ffprobe errors

### Other

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

- **agent**: add WebSocket transport with HTTP fallback
- **auth**: browser-based CLI authentication (like Claude Code)
- **cli**: add login command and refactor shared helpers
- **cli**: upgrade command, rich status, and version cache
- **daemon**: add auto-scan, force start, and stall timeout default
- **debrid**: add HTTPS downloader for debrid direct URLs
- **init**: add 60s countdown, skip key, and cancel detection to browser auth
- **stream**: report watch progress to API via HTTP Range tracking
- **stream**: UPnP port forwarding for remote video playback
- **usenet**: implement full NNTP download pipeline
- add migrate command, media server detection, and debrid auto-config
- replace setup with init wizard + interactive config menu
- add clean command to remove temp files, logs, and cached data
- add Sentry error reporting
- improve daemon resilience, streaming, and usenet downloads
- initial commit — unarr CLI

### CI/CD

- **deps**: bump docker/metadata-action from 5 to 6
- **deps**: bump docker/setup-qemu-action from 3 to 4
- **deps**: bump docker/login-action from 3 to 4
- **deps**: bump docker/build-push-action from 6 to 7
- **deps**: bump codecov/codecov-action from 5 to 6
- **docker**: remove dockerhub-description sync step
- **docker**: add Docker Hub description sync and DOCKERHUB.md
- **release**: add Docker Hub publish and VirusTotal scan jobs

### Changed

- migrate lint config to v2, remove daemon auto-upgrade, add trust badges
- extract BuildSyncItems to library package, remove duplication

### Documentation

- add beta notice, fix install URLs to get.torrentclaw.com
- improve CLI help, shell completion, and README

### Fixed

- **build**: unused variable in Windows process check
- **ci**: fix lint errors and pin CI to Go 1.25
- **ci**: upgrade golangci-lint to v2.11.3 for Go 1.25 support
- **ci**: remove go-client checkout steps
- **ci**: fix virustotal job condition syntax
- **docker**: upgrade alpine packages to patch CVE-2025-60876 and CVE-2026-27171
- **docker**: simplify Dockerfile for CI builds (no local go-client)
- **lint**: remove unused newStubCmd function
- **lint**: use default:none to disable errcheck, fix all gofmt and exhaustive
- **lint**: disable errcheck, tune gosec/exclusions for codebase state
- **lint**: configure linters for codebase maturity, fix gofmt and ineffassign
- **lint**: exclude common fire-and-forget patterns from errcheck
- **lint**: resolve errcheck and bodyclose warnings for golangci-lint v2
- **progress**: always report status transitions and poll for control signals
- **release**: disable homebrew tap (needs PAT, not GITHUB_TOKEN)
- **release**: disable homebrew tap until repo is created
- **torrent**: expand tracker list, add DHT persistence and configurable timeouts
- force-start tasks bypass HasCapacity check in dispatch loop
- add panic recovery to auto-scan, cap DHT nodes at 200
- harden usenet/debrid downloaders from critico review

### Other

- **cli**: remove moreseed stub command
- **cli**: remove redundant stub commands (monitor, open, add, compare)
- re-enable homebrew tap in goreleaser
- rename module from torrentclaw-cli to unarr

### Build

- remove UPX compression (antivirus false positives, startup penalty)
- add -s -w -trimpath to Makefile, add build-small target with UPX
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

