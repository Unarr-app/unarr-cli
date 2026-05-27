# unarr

> **⚠️ Beta** — unarr is under active development. Features may change, and bugs are expected. [Report issues here](https://github.com/torrentclaw/unarr/issues).

[![CI](https://github.com/torrentclaw/unarr/actions/workflows/ci.yml/badge.svg)](https://github.com/torrentclaw/unarr/actions/workflows/ci.yml)
[![Latest Release](https://img.shields.io/github/v/release/torrentclaw/unarr)](https://github.com/torrentclaw/unarr/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/torrentclaw/unarr)](https://goreportcard.com/report/github.com/torrentclaw/unarr)
[![Coverage](https://img.shields.io/codecov/c/github/torrentclaw/unarr)](https://codecov.io/gh/torrentclaw/unarr)
[![VirusTotal](https://img.shields.io/badge/VirusTotal-scanned-brightgreen?logo=virustotal)](https://github.com/torrentclaw/unarr/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/torrentclaw/unarr)](https://hub.docker.com/r/torrentclaw/unarr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/torrentclaw/unarr)](go.mod)

The single-binary terminal client for torrent, debrid, and usenet downloads. **Free and open source.**

Built-in torrent engine, debrid (Real-Debrid / AllDebrid), and NZB support. Stream to mpv/vlc, transcode on the fly with hardware acceleration, and manage your library — one binary or a headless daemon with WireGuard split-tunnel and Cloudflare Funnel remote access.

<!-- GIF demo placeholder -->
<!-- ![unarr Demo](docs/demo.gif) -->

## Installation

### Quick install (Linux/macOS)

```bash
curl -fsSL https://get.torrentclaw.com/install.sh | sh
```

### PowerShell (Windows)

```powershell
irm https://get.torrentclaw.com/install.ps1 | iex
```

### Homebrew (macOS/Linux)

```bash
brew install torrentclaw/tap/unarr
```

### Docker

```bash
docker run -d --name unarr \
  --restart unless-stopped \
  --network host \
  --read-only --memory 512m \
  -v ~/.config/torrentclaw:/config \
  -v ~/Media:/downloads \
  torrentclaw/unarr
```

Run setup first to configure your API key:

```bash
docker run -it --rm \
  -v ~/.config/torrentclaw:/config \
  torrentclaw/unarr setup
```

### Docker Compose

```bash
mkdir -p torrentclaw && cd torrentclaw
curl -fsSL https://raw.githubusercontent.com/torrentclaw/unarr/main/docker-compose.yml -o docker-compose.yml
docker compose up -d
```

<details>
<summary>docker-compose.yml</summary>

```yaml
services:
  unarr:
    image: torrentclaw/unarr:latest
    container_name: unarr
    restart: unless-stopped
    user: "1000:1000"
    read_only: true
    tmpfs:
      - /tmp:size=64m,mode=1777
    volumes:
      - ./config:/config
      - ~/Media:/downloads
      - unarr-data:/data
    environment:
      - TZ=${TZ:-UTC}
      # - UNARR_API_KEY=tc_your_key_here
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: "2.0"
    # Host network for full P2P performance
    network_mode: host
    # Or use bridge with ports:
    # ports:
    #   - "6881-6889:6881-6889/tcp"
    #   - "6881-6889:6881-6889/udp"

volumes:
  unarr-data:
```

</details>

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
| `unarr vpn status` | Show managed-VPN config and live tunnel state |
| `unarr vpn enable` | Turn the managed VPN on |
| `unarr vpn disable` | Turn the managed VPN off |

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

## VPN

unarr can route your **downloads** through a managed WireGuard VPN, so peers and
trackers see the VPN server's IP instead of yours. It runs entirely in userspace
(wireguard-go + a gVisor netstack) — **no root, no `wg-quick`, no changes to your
OS routing table**.

Requires a **PRO+ plan with the VPN add-on**. Set it up at
[torrentclaw.com/vpn](https://torrentclaw.com/vpn).

```bash
# Turn it on (writes [downloads.vpn] enabled = true to your config)
unarr vpn enable

# Restart the daemon so it brings the tunnel up at startup
unarr daemon restart        # or: unarr start (if not installed as a service)

# Check it's working — shows the exit server when the tunnel is up
unarr vpn status

# Verify your account is provisioned (queries the API)
unarr vpn status --check

# Turn it off again
unarr vpn disable
```

**Split-tunnel — read this:** only the torrent client's traffic goes through the
VPN. Your browser, `curl`, and every other app keep using your **real IP** — that
is by design. To check the VPN is working, look at `unarr vpn status` (or the
peer/announce IP), **not** your browser's "what's my IP". To protect your other
devices (phone, laptop), use the **OpenVPN credentials** from your profile — those
support ~10 concurrent devices and do **not** share the agent's WireGuard slot.

**When does it fetch the config?** Once, at daemon startup. There's no periodic
refresh — after changing your exit server in the web panel or re-provisioning,
restart the daemon to pick it up. If the fetch fails the daemon logs a `[vpn]`
line and downloads in the clear (never refuses to run).

**Self-hosted / personal VPN:** instead of the managed config, point unarr at a
local WireGuard `.conf`:

```toml
[downloads.vpn]
config_file = "/path/to/wg.conf"   # takes precedence over `enabled`
```

## Diagnostics

```bash
# Run all diagnostic checks
unarr doctor

# Update to the latest version
unarr self-update
unarr self-update --force   # reinstall even if up to date
```

`unarr doctor` checks: config file, API key, server connectivity (with latency), agent registration, download directory, disk space, and version.

### Updating unarr

unarr supports three update paths. Pick whichever fits your workflow.

**1. Manual self-update (always available).**

```bash
unarr self-update                # interactive update to latest
unarr self-update --force        # reinstall same version
unarr self-update --allow-unsigned # accept releases without checksum signature
```

The CLI downloads the new release archive over HTTPS (from
`torrentclaw.com/releases/download/v<ver>/`), verifies SHA-256, swaps the
binary in place (`.backup` kept next to it), and restarts the systemd
user unit if the daemon is running.

**2. Auto-apply on server signal (default, since 0.9.6).**

When you press **"Force update now"** on the web (Settings → Agent → Force
update), the server sets a flag your daemon polls every sync (~3 s). On
the next sync the daemon downloads the new binary, replaces itself, and
exits — `systemd Restart=always` respawns on the new version. No SSH, no
terminal access required. Works headless on NAS / Docker.

The button shows an amber warning if your agent is below 0.9.6 (older
daemons see the signal but only log "run unarr update" — the operator
must run the command manually that one time).

**Opt out of auto-apply.** Some users prefer reviewing CHANGELOG before
applying. Disable in `config.toml`:

```toml
[daemon]
auto_upgrade = false
```

With `auto_upgrade = false`, pressing the web button still flags your
agent (so the daemon logs the new version on next sync), but the daemon
will not download / replace anything — you run `unarr self-update` when
you're ready.

**3. Docker auto-restart with a new tag.**

```bash
docker pull torrentclaw/unarr:latest
docker compose up -d
```

Tags published: `latest`, `0.9`, `0.9.7`, ... — pin to a minor (`0.9`)
for opt-in patch updates without surprises.

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
auto_upgrade = true   # apply server-flagged upgrades in-place (since 0.9.6)

[notifications]
enabled = true

[general]
country = "US"
```

### Streaming reference

The in-browser player on torrentclaw.com streams from the daemon over HLS
(HTTP fragments + ffmpeg transcode for codecs the browser can't decode
natively). Enabled by default — a fresh install "just works" without editing
the TOML.

```toml
[downloads.transcode]
enabled        = true        # master switch
hw_accel       = "auto"      # auto | none | nvenc | qsv | vaapi | videotoolbox
preset         = "veryfast"  # libx264 preset
video_bitrate  = ""          # e.g. "5M" caps -b:v; empty = engine fallback (5M)
audio_bitrate  = "192k"      # e.g. "128k", "192k", "256k"
max_height     = 0           # 0 = no cap; e.g. 720 forces 720p max
max_concurrent = 2           # max simultaneous ffmpeg processes
```

#### `[downloads.transcode]`

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `enabled` | bool | `true` | Real-time HLS transcoding when source codec is browser-incompatible (HEVC, AV1, AC3, DTS). Requires `ffmpeg` + `ffprobe` on PATH. |
| `hw_accel` | string | `"auto"` | Hardware accel: `"auto"`, `"none"`, `"nvenc"` (NVIDIA), `"qsv"` (Intel), `"vaapi"` (Linux), `"videotoolbox"` (macOS). |
| `preset` | string | `"veryfast"` | libx264 preset. Slower preset = smaller files but higher CPU. Options: `ultrafast`, `superfast`, `veryfast`, `faster`, `fast`, `medium`, `slow`, `slower`, `veryslow`. |
| `video_bitrate` | string | `""` | E.g. `"5M"` caps `-b:v`. Empty falls back to the engine default (`5M`). |
| `audio_bitrate` | string | `"192k"` | E.g. `"128k"`, `"256k"`. |
| `max_height` | int | `0` | `0` = no cap. E.g. `720` forces 720p max — useful on weak GPUs. |
| `max_concurrent` | int | `2` | Max simultaneous ffmpeg processes. Increase if hosting multiple users on a beefy box. |

If `transcode.enabled = true` but `ffmpeg` / `ffprobe` aren't on PATH, the
daemon logs a warning at startup and HLS sessions are rejected at runtime
with a clear error — install ffmpeg or set `enabled = false`.

#### `[downloads.hls_cache]` — persistent HLS segment cache

```toml
[downloads.hls_cache]
enabled = true   # on by default
size_gb = 5      # disk budget; LRU eviction once exceeded
dir     = ""     # custom path; empty = ~/.cache/unarr/hls-cache
```

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `enabled` | bool | `true` | Persists finished HLS encodes per `(source, quality, audio_index)`. A second play of the same file at the same quality reuses the segments — no ffmpeg, near-zero CPU, instant playback. Set to `false` to delete segments on session close (original behavior). |
| `size_gb` | int | `5` | Cache budget in gigabytes. When exceeded the LRU sweeper evicts the least-recently-used cached encodes hourly. Minimum 1 GB (smaller values are clamped up). |
| `dir` | string | `""` | Custom storage path. Empty defaults to `~/.cache/unarr/hls-cache` (Linux/macOS) or the user cache dir (Windows). |

**What it does.** First play encodes normally (ffmpeg writes segments).
On session close, if every segment is on disk and ffmpeg exited cleanly,
the directory is sealed with a `.complete` marker and kept. Next time the
same source + quality combo is requested, the daemon serves segments
straight from disk — no transcode, no warm-up, no CPU cost.

**Why per (source, quality, audio).** Renaming the file or switching
quality invalidates the entry: the segments are tied to the exact source
bytes and the exact ffmpeg parameters. Re-encoding generates a new key.

**Eviction.** A background goroutine wakes every hour. If total cache size
exceeds `size_gb`, it deletes the oldest entries (by mtime) until under
budget. Active sessions are pinned — they never get evicted mid-play.

**Disable.** Either edit the TOML to set `enabled = false`, or remove the
cache directory manually (it'll be recreated as needed). Disabling does
not delete existing cached segments — drop `dir` (or `~/.cache/unarr/hls-cache`)
to reclaim the space.

#### `[downloads.vpn]`

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `enabled` | bool | `false` | Managed VPN: at startup the daemon fetches a WireGuard config from your account and split-tunnels torrent traffic through it. Needs a PRO+ plan with the VPN add-on. Toggle with `unarr vpn enable` / `disable`. |
| `config_file` | string | `""` | Self-hosted / personal VPN: path to a local WireGuard `.conf`. **Takes precedence over `enabled`** — when set, the daemon uses this file and never calls the API. |

See the [VPN](#vpn) section above for how it works (split-tunnel, no root) and
how to protect your other devices.

#### `[downloads.funnel]` — public HTTPS hostname for the daemon (CloudFlare Quick Tunnel)

```toml
[downloads.funnel]
enabled = false   # off by default
```

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `enabled` | bool | `false` | Spawns `cloudflared tunnel --url http://localhost:<stream_port>` as a child process at daemon startup. Toggle with `unarr funnel on` / `off`. Requires `cloudflared` on PATH. |

**What it does.** Without a tunnel, the daemon is reachable on `localhost`,
your LAN, and (if installed) Tailscale. That covers the same-machine and
Tailscale-connected cases, but the **browser-based player on torrentclaw.com
fails on any other network** because HTTPS pages can't fetch HTTP resources
("mixed content"). Enabling the funnel gives the daemon a public
`https://<random>.trycloudflare.com` hostname so the web player picks it up
and playback works from anywhere — phone on cellular, friend's laptop on a
foreign Wi-Fi, anywhere. The Stremio addon already works cross-network
(native mpv/VLC players ignore CORS), so this is strictly a web-player fix.

**Privacy posture.** Bytes pass through CloudFlare's edge — TorrentClaw never
relays content (we don't see your traffic), CloudFlare does. Quick Tunnels
are **anonymous** (no CF account required); the registration is unauthenticated
and the hostname is a random label, but CF logs request metadata like any CDN
would. If you want zero third-party byte access, use Tailscale instead.

**Limitations (free Quick Tunnels).**
| Aspect | Limit |
|--------|-------|
| Session lifetime | ~6 hours, then the hostname rotates. cloudflared re-registers automatically; the web picks up the new URL on the next sync. In-flight HLS sessions break across the rotation (browser retries). |
| Bandwidth | No documented hard cap, but CF reserves the right to throttle. 1080p HLS (~6 Mbps) is fine; 4K HEVC at 25 Mbps may hit throttling. |
| Latency | +20–80 ms vs direct LAN/Tailscale (extra hop browser → CF edge → tunnel). HLS player buffer absorbs it. |
| Concurrency | One tunnel serves N viewers. CF rate-limits ~200 req/s, plenty for HLS segments. |
| TOS | CloudFlare flags Quick Tunnels as "not for production traffic". They can decommission an abusive tunnel without notice. |

For heavy / high-throughput / persistent-URL use cases, switch to a CloudFlare
Named Tunnel (free, needs a CF account) or run your own reverse proxy — both
out of scope for the bundled command.

**Disable.** `unarr funnel off` flips `enabled` to `false` in the TOML and
prompts you to restart the daemon. You can also edit `config.toml` directly:

```toml
[downloads.funnel]
enabled = false
```

**Install cloudflared.**
- Linux: `apt install cloudflared` (after adding CF's apt repo) — see
  <https://pkg.cloudflare.com>. Or pull the static binary from
  <https://github.com/cloudflare/cloudflared/releases>.
- macOS: `brew install cloudflared`.
- Windows: `winget install --id Cloudflare.cloudflared`.

If `cloudflared` is not on PATH the daemon logs a warning at startup and
falls back to LAN/Tailscale-only reachability.

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
