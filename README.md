# unarr

[![CI](https://github.com/torrentclaw/unarr/actions/workflows/ci.yml/badge.svg)](https://github.com/torrentclaw/unarr/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/torrentclaw/unarr)](https://goreportcard.com/report/github.com/torrentclaw/unarr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/torrentclaw/unarr)](go.mod)

Powerful terminal tool for torrent search and management.

Search 30+ torrent sources, inspect torrent quality, discover popular content, find streaming providers, and manage your media collection — all from your terminal.

<!-- GIF demo placeholder -->
<!-- ![unarr Demo](docs/demo.gif) -->

## Installation

### Quick install (Linux/macOS)

```bash
curl -fsSL https://torrentclaw.com/install.sh | sh
```

### PowerShell (Windows)

```powershell
irm https://torrentclaw.com/install.ps1 | iex
```

### Homebrew (macOS/Linux)

```bash
brew install torrentclaw/tap/unarr
```

### Go install

```bash
go install github.com/torrentclaw/unarr/cmd/unarr@latest
```

### GitHub Releases

Download prebuilt binaries for Linux, macOS, and Windows from [GitHub Releases](https://github.com/torrentclaw/unarr/releases).

### Build from source

```bash
git clone https://github.com/torrentclaw/unarr.git
cd unarr
make build
```

## Quick Start

```bash
# 1. Run the init wizard (opens browser for API key)
unarr init

# 2. Search for content
unarr search "breaking bad" --type show --quality 1080p

# 3. Start the download daemon
unarr start
```

## Commands

### Getting Started

| Command | Description |
|---------|-------------|
| `unarr init` | First-time configuration wizard (API key, download dir, daemon) |
| `unarr config` | Edit all settings interactively (speed, organization, etc.) |
| `unarr migrate` | Import settings and wanted list from Sonarr/Radarr/Prowlarr [pre-beta] |

### Search & Discovery

| Command | Description |
|---------|-------------|
| `unarr search <query>` | Search for movies and TV shows with advanced filters |
| `unarr inspect <magnet\|hash\|name>` | TrueSpec analysis — quality, codec, seed health |
| `unarr popular` | Show popular movies and TV shows |
| `unarr recent` | Show recently added content |
| `unarr watch <query>` | Find where to watch — streaming + torrents |

### Downloads & Streaming

| Command | Description |
|---------|-------------|
| `unarr download <hash\|magnet>` | One-shot download (no daemon needed) |
| `unarr stream <hash\|magnet>` | Stream a torrent directly to mpv/vlc/browser |

### Daemon Management

| Command | Description |
|---------|-------------|
| `unarr start` | Start the download daemon (foreground) |
| `unarr stop` | How to stop the running daemon |
| `unarr status` | Show daemon status and active downloads |
| `unarr daemon install` | Install as system service (systemd/launchd) |
| `unarr daemon uninstall` | Remove the system service |

### System & Diagnostics

| Command | Description |
|---------|-------------|
| `unarr stats` | Show catalog statistics |
| `unarr doctor` | Diagnose configuration and connectivity |
| `unarr clean` | Remove temporary files, logs, and cached data |
| `unarr self-update` | Update unarr to the latest version |
| `unarr version` | Show version info |
| `unarr completion <shell>` | Generate shell completion scripts |

---

## Search

Search the catalog with advanced filters. Results include quality scores, seed health, and metadata from 30+ sources.

```bash
unarr search "inception" --sort seeders --min-rating 7 --lang es
unarr search "breaking bad" --type show --quality 1080p
unarr search "matrix" --json | jq '.results[].title'
```

**Filters:**

| Flag | Description | Values |
|------|-------------|--------|
| `--type` | Content type | `movie`, `show` |
| `--quality` | Video quality | `480p`, `720p`, `1080p`, `2160p` |
| `--lang` | Audio language (ISO 639) | `es`, `en`, `fr`, `de`, ... |
| `--genre` | Genre | `Action`, `Comedy`, `Drama`, `Horror`, ... |
| `--year-min` | Minimum release year | `2020` |
| `--year-max` | Maximum release year | `2026` |
| `--min-rating` | Minimum IMDb/TMDb rating | `0`-`10` |
| `--sort` | Sort order | `relevance`, `seeders`, `year`, `rating`, `added` |
| `--limit` | Results per page | `1`-`50` |
| `--page` | Page number | `1`, `2`, ... |
| `--country` | Country for streaming info | `US`, `ES`, `GB`, ... |

## Inspect

TrueSpec analysis — parse a torrent and show detailed quality specs.

```bash
unarr inspect "Oppenheimer.2023.1080p.BluRay.x265"
unarr inspect abc123def456abc123def456abc123def456abc1
unarr inspect "magnet:?xt=urn:btih:ABC123&dn=Movie.2023.1080p"
```

Accepts magnet URIs, 40-character info hashes, or torrent file names. Shows quality, codec, size, seeds, languages, source, quality score, health, and alternatives.

## Watch

Find where to watch — streaming services alongside torrent options.

```bash
unarr watch "oppenheimer" --country ES
unarr watch "breaking bad" --json
```

Shows legal streaming options first (subscription, free, rent, buy), then torrent alternatives.

## Stream

Stream a torrent directly to a media player without waiting for the full download.

```bash
unarr stream abc123def456abc123def456abc123def456abc1
unarr stream "magnet:?xt=urn:btih:..." --port 8080
unarr stream <hash> --player mpv
unarr stream <hash> --no-open   # just print the URL
```

Downloads pieces sequentially and serves the video over a local HTTP server. Auto-detects mpv, vlc, or your default browser.

## Download

One-shot download by info hash or magnet link (no daemon required).

```bash
unarr download abc123def456abc123def456abc123def456abc1
unarr download "magnet:?xt=urn:btih:..." --method torrent
```

## Daemon

The daemon receives download tasks from the web dashboard and executes them automatically.

```bash
# Start in foreground (Ctrl+C to stop)
unarr start

# Or install as a system service (auto-starts on boot)
unarr daemon install

# Check status
unarr status

# Uninstall the service
unarr daemon uninstall
```

The daemon connects via WebSocket for instant task delivery, with automatic HTTP fallback. It supports torrent, debrid, and usenet downloads concurrently, reports progress to the web dashboard, and handles graceful shutdown.

**Service locations:**
- Linux: `~/.config/systemd/user/unarr.service` (systemd)
- macOS: `~/Library/LaunchAgents/com.torrentclaw.unarr.plist` (launchd)

## Diagnostics

```bash
# Run all diagnostic checks
unarr doctor

# Update to the latest version
unarr self-update
unarr self-update --force   # reinstall even if up to date
```

`unarr doctor` checks: config file, API key, server connectivity (with latency), agent registration, download directory, disk space, and version.

## Clean

Remove temporary files, logs, resume data, and other artifacts generated by unarr. Shows what will be removed and asks for confirmation before deleting.

```bash
unarr clean            # Show files and confirm before removing
unarr clean --dry-run  # Show what would be removed (no prompt)
unarr clean --yes      # Skip confirmation
unarr clean --all      # Also remove the data directory
```

**Cleans:** log files, daemon state, stale usenet resume files (> 7 days), stream temp data, upgrade temp files, and stale atomic-write temps. Recent resume files are kept to preserve download progress for paused or interrupted downloads. Never removes your config file, downloaded media, or partial torrent/debrid downloads.

## Alias (optional)

Create a shell alias for shorter commands:

```bash
# Add to ~/.bashrc or ~/.zshrc
alias un=unarr

# Then use:
un search "breaking bad" --type show
un popular --limit 5
un start
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--json` | Output as JSON (for piping to `jq`, scripts) |
| `--no-color` | Disable colored output |
| `--api-key` | API key (overrides config file and env) |
| `--config` | Custom config file path |

## JSON Output

All query commands support `--json` for scripting:

```bash
# Pipe to jq
unarr search "matrix" --json | jq '.results[].title'

# Save to file
unarr popular --json > popular.json

# Use in scripts
SEEDS=$(unarr search "inception" --json | jq '.results[0].torrents[0].seeders')
```

## Configuration

### Config file

Location: `~/.config/unarr/config.toml`

```toml
[auth]
api_key = "tc_your_api_key_here"
api_url = "https://torrentclaw.com"

[agent]
id = "auto-generated-uuid"
name = "My PC"

[downloads]
dir = "~/Media"
preferred_method = "auto"        # auto | torrent | debrid | usenet
max_concurrent = 3
max_download_speed = "0"         # e.g. "10MB", "500KB", "0" = unlimited
max_upload_speed = "0"

[organize]
enabled = true
movies_dir = "~/Media/Movies"
tv_shows_dir = "~/Media/TV Shows"

[daemon]
poll_interval = "30s"
heartbeat_interval = "30s"

[notifications]
enabled = true

[general]
country = "US"
```

### Environment variables

Environment variables override config file values:

```bash
export UNARR_API_KEY=tc_your_api_key
export UNARR_API_URL=https://torrentclaw.com
export UNARR_COUNTRY=ES
export UNARR_DOWNLOAD_DIR=~/Media
```

### Speed limits

Speed limits use human-readable format:

```toml
max_download_speed = "10MB"    # 10 megabytes/sec
max_upload_speed = "1MB"       # 1 megabyte/sec
max_download_speed = "500KB"   # 500 kilobytes/sec
max_download_speed = "0"       # unlimited (default)
```

## Shell Completion

Generate tab-completion scripts for your shell:

```bash
# Bash — add to ~/.bashrc
eval "$(unarr completion bash)"

# Zsh — add to ~/.zshrc
eval "$(unarr completion zsh)"

# Fish
unarr completion fish > ~/.config/fish/completions/unarr.fish

# PowerShell — add to $PROFILE
unarr completion powershell >> $PROFILE
```

Completions provide tab-completion for commands, flags, and flag values (e.g. `--type <Tab>` shows `movie` and `show`).

## Coming Soon

These commands are planned for future releases:

| Command | Description |
|---------|-------------|
| `unarr upgrade` | Find a better version of a torrent |
| `unarr moreseed` | Find same quality with more seeders |
| `unarr compare` | Compare two torrents side by side |
| `unarr scan` | Scan your media library for upgrades |
| `unarr add` | Search and add torrents to your client |
| `unarr monitor` | Watch for new episodes of a series |
| `unarr open` | Open content in the browser |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, code style, and guidelines.

## License

MIT License — see [LICENSE](LICENSE) for details.
