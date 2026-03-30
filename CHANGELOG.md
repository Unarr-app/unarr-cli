# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Init wizard with daemon install step (`unarr init`, replaces `unarr setup`)
- Interactive config menu with 7 categories (`unarr config [category]`)
- Migration wizard from Sonarr/Radarr/Prowlarr (`unarr migrate`) [pre-beta]
  - Auto-detect instances via Docker, config files, port scan, Prowlarr
  - Import download history and blocklist to avoid re-downloading
  - Detect Plex/Jellyfin/Emby media servers and library paths
  - Extract debrid tokens from *arr download clients
  - JSON export with `--dry-run --json`
- Media server detection in `unarr init` (suggests library paths as download directory)
- `preferred_quality` setting in config (2160p/1080p/720p)
- Clean command to remove temp files, logs, and cached data (`unarr clean`)
- Daemon mode with background download management (`unarr start`)
- One-shot download command (`unarr download`)
- Stream to media player (`unarr stream`)
- Doctor command for diagnostics (`unarr doctor`)
- Status command for daemon monitoring (`unarr status`)
- Download engine with torrent support (debrid and usenet coming soon)
- File organization (Movies/TV Shows directory structure)
- Post-download verification
- Desktop notifications (Linux, macOS)
- Docker support with multi-stage build
- Cross-platform install scripts (shell, PowerShell)
- Dependabot for automated dependency updates
- golangci-lint configuration with gosec

### Changed
- Renamed `internal/commands/` to `internal/cmd/`

## [0.1.0] - 2025-02-14

### Added
- Initial release
- Search across 30+ torrent sources with advanced filters
- TrueSpec torrent inspection (quality, codec, seeds, score)
- Watch command (streaming providers + torrent alternatives)
- Popular and recent content browsing
- System statistics
- Interactive configuration
- JSON output mode (`--json`) for scripting
- Colored terminal output with `--no-color` support
- Homebrew tap distribution
- GoReleaser with UPX compression
- CI pipeline (test, build, lint, vet)
- Lefthook git hooks (gofmt, go vet, conventional commits)

[Unreleased]: https://github.com/torrentclaw/unarr/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/torrentclaw/unarr/releases/tag/v0.1.0
