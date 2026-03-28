# unarr

[![CI](https://github.com/torrentclaw/torrentclaw-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/torrentclaw/torrentclaw-cli/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/torrentclaw/torrentclaw-cli)](https://goreportcard.com/report/github.com/torrentclaw/torrentclaw-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/torrentclaw/torrentclaw-cli)](go.mod)

Powerful terminal tool for torrent search and management.

Search 30+ torrent sources, inspect torrent quality, discover popular content, find streaming providers, and manage your media collection — all from your terminal.

<!-- GIF demo placeholder -->
<!-- ![unarr Demo](docs/demo.gif) -->

## Installation

### Go install

```bash
go install github.com/torrentclaw/torrentclaw-cli/cmd/unarr@latest
```

### Homebrew (macOS/Linux)

```bash
brew install torrentclaw/tap/unarr
```

### GitHub Releases

Download prebuilt binaries for Linux, macOS, and Windows from [GitHub Releases](https://github.com/torrentclaw/torrentclaw-cli/releases).

### Build from source

```bash
git clone https://github.com/torrentclaw/torrentclaw-cli.git
cd torrentclaw-cli
make build
```

## Quick Start

```bash
# Configure (first time)
unarr config

# Search for content
unarr search "breaking bad" --type show --quality 1080p

# Inspect a torrent
unarr inspect "magnet:?xt=urn:btih:ABC123&dn=Movie.2023.1080p.BluRay.x265"

# Popular content
unarr popular --limit 20

# Recently added
unarr recent

# Where to watch (streaming + torrents)
unarr watch "oppenheimer" --country ES

# System statistics
unarr stats
```

## Commands

### Search

Search the unarr catalog with advanced filters.

```bash
unarr search "inception" --sort seeders --min-rating 7 --lang es
```

**Filters:**

| Flag | Description | Example |
|------|-------------|---------|
| `--type` | Content type | `movie`, `show` |
| `--quality` | Video quality | `480p`, `720p`, `1080p`, `2160p` |
| `--lang` | Audio language (ISO 639) | `es`, `en`, `fr` |
| `--genre` | Genre filter | `Action`, `Comedy`, `Drama` |
| `--year-min` | Minimum release year | `2020` |
| `--year-max` | Maximum release year | `2026` |
| `--min-rating` | Minimum IMDb/TMDb rating | `7` |
| `--sort` | Sort order | `relevance`, `seeders`, `year`, `rating`, `added` |
| `--limit` | Results per page (1-50) | `10` |
| `--page` | Page number | `2` |
| `--country` | Country for streaming info | `US`, `ES` |

### Inspect

TrueSpec analysis — parse a torrent and show detailed specs.

```bash
unarr inspect "Oppenheimer.2023.1080p.BluRay.x265"
```

Accepts magnet URIs, 40-character info hashes, or torrent names.

Output includes: quality, codec, size, seeds, languages, source, quality score, and health.

### Watch

Find where to watch — streaming services alongside torrent options.

```bash
unarr watch "oppenheimer" --country ES
```

Shows legal streaming options first (subscription, free, rent, buy), then torrent alternatives.

### Popular

Show trending content ranked by community engagement.

```bash
unarr popular --limit 20
```

### Recent

Show the most recently added content.

```bash
unarr recent --limit 20
```

### Stats

Display unarr system statistics.

```bash
unarr stats
```

### Config

Interactive configuration setup.

```bash
unarr config
```

Saves to `~/.config/unarr/config.yaml`.

## Alias

You can use `un` as a shorthand for `unarr`:

```bash
un search "breaking bad" --type show
un popular --limit 5
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--json` | Output as JSON (for piping to `jq`, scripts) |
| `--no-color` | Disable colored output |
| `--api-key` | API key (overrides config file and env) |
| `--config` | Custom config file path |

## JSON Output

All commands support `--json` for scripting:

```bash
# Pipe to jq
unarr search "matrix" --json | jq '.results[].title'

# Save to file
unarr popular --json > popular.json

# Use in scripts
SEEDS=$(unarr search "inception" --json | jq '.results[0].torrents[0].seeders')
```

## Configuration

Config file location: `~/.config/unarr/config.yaml`

```yaml
api_url: https://torrentclaw.com
api_key: tc_your_api_key_here
country: US
```

Environment variables (override config file):

```bash
export UNARR_API_URL=https://torrentclaw.com
export UNARR_API_KEY=tc_your_api_key
export UNARR_COUNTRY=ES
```

## Coming Soon

These commands are stubbed and will be available in future releases:

- `unarrupgrade` — Find a better version of a torrent
- `unarrmoreseed` — Find same quality with more seeders
- `unarrcompare` — Compare two torrents side by side
- `unarrscan` — Scan your media library for upgrades
- `unarradd` — Search and add torrents to your client
- `unarrmonitor` — Watch for new episodes of a series

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, code style, and guidelines.

## License

MIT License — see [LICENSE](LICENSE) for details.
