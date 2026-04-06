# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **organize**: use server metadata for file organization and subtitle handling
- **stream**: add NAT-PMP port mapping for remote downloads

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
[Unreleased]: https://github.com/torrentclaw/unarr/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/torrentclaw/unarr/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/torrentclaw/unarr/compare/v0.3.7...v0.4.0
[0.3.7]: https://github.com/torrentclaw/unarr/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/torrentclaw/unarr/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/torrentclaw/unarr/compare/v0.3.3...v0.3.5
[0.3.3]: https://github.com/torrentclaw/unarr/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/torrentclaw/unarr/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/torrentclaw/unarr/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/torrentclaw/unarr/releases/tag/v0.3.0

