# unarr

> **⚠️ Active development** — unarr is under active development. Features may change, and bugs are expected. [Report issues here](https://github.com/Unarr-app/unarr-cli/issues).

[![CI](https://github.com/Unarr-app/unarr-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/Unarr-app/unarr-cli/actions/workflows/ci.yml)
[![Latest Release](https://img.shields.io/github/v/release/Unarr-app/unarr-cli)](https://github.com/Unarr-app/unarr-cli/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/Unarr-app/unarr-cli)](https://goreportcard.com/report/github.com/Unarr-app/unarr-cli)
[![Coverage](https://img.shields.io/codecov/c/github/Unarr-app/unarr-cli)](https://codecov.io/gh/Unarr-app/unarr-cli)
[![VirusTotal](https://img.shields.io/badge/VirusTotal-scanned-brightgreen?logo=virustotal)](https://github.com/Unarr-app/unarr-cli/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/unarr/cli)](https://hub.docker.com/r/unarr/cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/Unarr-app/unarr-cli)](go.mod)

The single-binary terminal client for torrent, debrid, and usenet downloads. **Free and open source.**

Built-in torrent engine, debrid (Real-Debrid / AllDebrid), and NZB support. Stream to mpv/vlc, transcode on the fly with hardware acceleration, and manage your library — one binary or a headless daemon with WireGuard split-tunnel and Cloudflare Funnel remote access.

<!-- GIF demo placeholder -->
<!-- ![unarr Demo](docs/demo.gif) -->

## Installation

### Quick install (Linux/macOS)

```bash
curl -fsSL https://unarr.app/install.sh | sh
```

### PowerShell (Windows)

```powershell
irm https://unarr.app/install.ps1 | iex
```

### Homebrew (macOS/Linux) — coming soon

```bash
brew install unarr-app/tap/unarr
```

### Debian/Ubuntu (.deb) and Fedora/RHEL (.rpm)

Published on every release for `amd64` and `arm64`, and covered by the same
signed `checksums.txt` as the tarballs:

```bash
# Debian/Ubuntu — replace <version> and <arch> (amd64 | arm64)
curl -fsSLO https://github.com/Unarr-app/unarr-cli/releases/latest/download/unarr_<version>_linux_<arch>.deb
sudo apt install ./unarr_<version>_linux_<arch>.deb

# Fedora/RHEL
sudo dnf install https://github.com/Unarr-app/unarr-cli/releases/latest/download/unarr_<version>_linux_<arch>.rpm
```

The package installs the `unarr` binary only. Run `unarr init` afterwards to
link your account, pick a download directory and install the background
service — the same step the other install methods end with. `ffmpeg` is a
recommended dependency; without it the agent downloads its own static copy.

### Docker

```bash
docker run -d --name unarr \
  --restart unless-stopped \
  --network host \
  --read-only --memory 512m \
  -v ~/.config/unarr:/config \
  -v ~/Media:/downloads \
  unarr/cli
```

Run setup first to configure your API key:

```bash
docker run -it --rm \
  -v ~/.config/unarr:/config \
  unarr/cli setup
```

### Docker Compose

```bash
mkdir -p unarr && cd unarr
curl -fsSL https://raw.githubusercontent.com/Unarr-app/unarr-cli/main/docker-compose.yml -o docker-compose.yml
docker compose up -d
```

<details>
<summary>docker-compose.yml</summary>

```yaml
services:
  unarr:
    image: unarr/cli:latest
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
go install github.com/Unarr-app/unarr-cli/cmd/unarr@latest
```

### GitHub Releases

Download prebuilt binaries for Linux, macOS, and Windows from [GitHub Releases](https://github.com/Unarr-app/unarr-cli/releases).

### Build from source

```bash
git clone https://github.com/Unarr-app/unarr-cli.git
cd unarr-cli
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
| `unarr login` | Authenticate with your account (opens browser) |
| `unarr config` | Edit all settings interactively (speed, organization, etc.) |
| `unarr config check` | Validate `config.toml` — unknown keys (with suggestions) and out-of-range values; exits non-zero when anything is reported |
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

### Library

| Command | Description |
|---------|-------------|
| `unarr scan <path>` | Scan a folder, analyze video files with ffprobe, sync quality data |
| `unarr library clean` | Sweep download & library dirs for orphaned files (dry-run by default) |
| `unarr library stats` | Report library health, composition & quality — read-only (`--json` for scripts) |

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
| `unarr mirrors` | Manage mirror failover list (list / update / test) |
| `unarr logs` | Read the daemon log — `-f`, `--since`, `--level`, `--grep`, `-n` (alias: `unarr daemon logs`) |
| `unarr clean` | Remove temporary files, logs, and cached data |
| `unarr upgrade` | Update unarr to the latest version (alias: `unarr self-update`) |
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

**Subtitles.** When the source file contains embedded text subtitles (SRT, ASS, PGS in an MKV), the daemon extracts them as WebVTT sidecars during the same transcode pass and serves them alongside the HLS stream. The web player lists available subtitle tracks automatically — no separate subtitle download needed.

**Seek-anywhere (copy-VOD).** For sources with browser-compatible codecs (H.264 + AAC), the daemon uses a copy pass instead of re-encoding. This enables full random-seek across the entire duration from the first play, at near-zero CPU cost.

**Audio tracks.** Multi-audio MKVs expose all tracks (e.g. `fr`, `en`, `es`) in the player's audio menu. Switching tracks starts a new session from the current position.

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
GitHub Releases at `github.com/Unarr-app/unarr-cli/releases/download/v<ver>/`,
falling back to the web origin if GitHub is unreachable), verifies SHA-256,
swaps the binary in place (`.backup` kept next to it), and restarts the systemd
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
docker pull unarr/cli:latest
docker compose up -d
```

Tags published: `latest`, `1.2`, `1.2.2`, ... — pin to a minor (`1.2`)
for opt-in patch updates without surprises.

## Logs

```bash
unarr logs                                  # last 50 lines
unarr logs -f                               # follow
unarr logs -n 200 --level warn              # only warnings and errors
unarr logs --since 2h --grep 'usenet|nzb'   # last 2 hours, matching a regex
unarr logs --since "2025-01-20 09:00" --level error
unarr logs --boot                           # startup output — when it won't start
unarr logs rotate                           # trim now, if rotation is on and over budget
```

`unarr daemon logs` is the same command (kept as an alias, `-f` / `-n`
included). On Linux, where the systemd unit sends output to the journal, this
streams from `journalctl --user -u unarr` instead of a file; everywhere else it
reads `unarr.log` from the data directory and transparently continues into the
rotated copies when the live file holds fewer lines than you asked for.

Severity is inferred from the line itself (the daemon logs free-form text), so
`--level` is a reading aid, not a guarantee. `--grep` is a case-insensitive
regular expression. `--since` takes an age (`30m`, `2h`, `7d`, `1w`) or a stamp
(`2025-01-20`, `2025-01-20 09:00`, `09:00` for today).

**Two files.** A daemon started by a service launcher owns `unarr.log` and
writes everything it logs there. The launcher itself captures a second, much
smaller `unarr.boot.log`: the start banner, a fatal error from a start that
never got going, a crash dump — output that never reaches the daemon's own log.
`unarr logs --boot` reads it, and that is where to look when the daemon does not
come up at all. On a systemd install there is no such file: the startup output
is in the journal, so `unarr logs` already shows it and `--boot` says so.

**Rotation is OPT-IN, and off by default** (`log_max_size_mb = 0`). Out of the
box unarr never rotates anything: `unarr.log` and `unarr.boot.log` just grow.
Bound them with `unarr clean`, or with an external `logrotate` — and if you use
one, it must use `copytruncate`, since the daemon holds its descriptor for the
whole run and has no reopen signal.

Set `log_max_size_mb` to a number of MB to turn rotation on. `unarr.log` is then
rotated once it reaches that size, keeping `log_max_files` copies (default 3),
and `unarr.boot.log` gets its own fixed 2 MB cap with one rotated copy —
independent of the size you set, but switched on by the same key. A running
daemon rotates its own log by renaming it aside, so rotation does not wait for a
restart; `unarr logs rotate` is for the case where the daemon is stopped, and it
says so rather than doing nothing if a running daemon owns the file.

**What you accept by turning it on.** Rotation is not a solved problem here, and
this is why it does not ship on. Any second process holding the log can block
the rename or the truncate — an open `unarr logs -f`, an antivirus scanner,
OneDrive or Dropbox syncing the data directory, Windows Search. Windows is the
strict case: a holder there can deny write access outright, so a rotation that
cannot proceed is reported and the log keeps growing. Residual failure modes
that remain even when it does proceed (a reader holding a rotated slot, two
rotators racing over one staging file, a live-but-offline daemon losing its
ownership claim) are listed under "Deuda abierta" in
[`docs/plans/daemon-log-ownership.md`](docs/plans/daemon-log-ownership.md) —
read it before enabling this on an install you care about. On Windows only, the
boot log's trim is baked into the launcher script at install time, so re-run
`unarr daemon install` after enabling rotation for that one to take effect.

The global `--log-level` flag overrides `[daemon] log_level` for a single
command.

## Clean

Remove temporary files, logs, resume data, and other artifacts generated by unarr. Shows what will be removed and asks for confirmation before deleting.

```bash
unarr clean            # Show files and confirm before removing
unarr clean --dry-run  # Show what would be removed (no prompt)
unarr clean --yes      # Skip confirmation
unarr clean --all      # Also remove the data directory
```

**Cleans:** log files (including the rotated `unarr.log.1`, `.2`, … copies), daemon state, stale usenet resume files (> 7 days), stale replaced-file backups (> 7 days, kept by library upgrades), stream temp data, upgrade temp files, and stale atomic-write temps. Recent resume files are kept to preserve download progress for paused or interrupted downloads. Never removes your config file, downloaded media, or partial torrent/debrid downloads.

## Library clean (hygiene sweep)

`unarr library clean` reconciles your **download directory** and **Movies/TV library dirs**, reporting — and with `--apply`, removing — the deterministic junk that accretes there. It **never** removes a valid, playable video (>= the plausibility floor, default 1 MiB), and only removes duplicates after a full byte-for-byte compare confirms they are identical (a cheap fingerprint just finds the candidates). Everything acted upon is confined to the configured download/movies/tv directories.

```bash
unarr library clean               # report only (DRY-RUN — the default)
unarr library clean --apply       # actually remove the reported orphans
unarr library clean --dry-run     # force report-only even with --apply (explicit preview)
unarr library clean --dedup-only  # only collapse byte-identical duplicate videos
```

**Reports / removes:**

- **Download stubs** — video files below the floor (a CDN/expired-link stub, not media).
- **Orphaned partials** — `.part`, `.!qB`, `.aria2`, `.tmp`, `.partial` with no active download.
- **Duplicate videos** — byte-identical copies of the same file in a directory (keeps one canonical copy, verified by fingerprint = size + first/last 1 MiB). Distinct content (a real 2160p next to a 1080p) is never touched.
- **Orphaned subtitles/sidecars** — `.srt`/`.nfo`/`.jpg`/`.par2`/… with no owning video. Per-track sidecars in a `.unarr/` cache dir are checked against the **parent** release dir, so they are only removed when the release itself no longer holds the video.
- **Video-less directories** — empty or holding only junk/stubs.
- **Media-named directories** — a folder literally named `movie.mkv/` (a mis-created dir).
- **Suspect zero-content videos** *(opt-in, `remove_corrupt_videos`)* — a video of the right size (>= floor, so not a stub) whose **first and/or last 1 MiB is all zero bytes**: a corrupt/zero-filled download that is unplayable yet passes the floor and dodges the dedup (its fingerprint differs from the real copy). This is a **strong-but-not-absolute heuristic**, so it defaults **off** and is removed **only** by this manual command with the toggle on — never by the daemon's automatic sweep. `unarr library stats` always counts these (shown as **Suspect (zero-content)**) so you can see them even without enabling removal.

### Automatic cleanup (daemon)

The **same sweep runs automatically after every library auto-scan** in the daemon — it is the native, always-on replacement for an external cleanup script. It applies only the safe, deterministic categories, protects any partial modified in the last few minutes (a live download), logs every action, and reports the space freed. Configure it under `[library.cleanup]`:

```toml
[library.cleanup]
  enabled = true                   # run automatically after each auto-scan (manual command always works)
  min_video_bytes = "1MiB"         # anti-stub floor; a video smaller than this is a stub
  remove_stubs = true              # delete videos below min_video_bytes
  remove_orphan_partials = true    # delete .part/.!qB/.aria2/.tmp/.partial with no active task
  dedup_exact = true               # collapse byte-identical duplicate videos (keeps one canonical copy)
  remove_orphan_subtitles = true   # delete sidecars with no owning video (.unarr → checks parent dir)
  prune_empty_dirs = true          # remove video-less dirs and media-named dirs
  remove_corrupt_videos = false    # OPT-IN: flag/remove right-sized videos whose first/last 1 MiB is all zeros
```

Every category **except `remove_corrupt_videos` defaults on** (each of those is deterministic and non-destructive to valid media). `remove_corrupt_videos` defaults **off** because zero-content detection is a heuristic; enable it deliberately, and even then it is applied **only** by the manual `unarr library clean --apply` — the daemon's automatic sweep never removes a suspect video. Set `enabled = false` to disable the automatic post-scan sweep — the manual `unarr library clean` command still works regardless.

## Library stats (health & composition)

`unarr library stats` reports the health, composition and quality of your **Movies/TV library and download dir**. It is a **pure DRY-RUN — it only READS**, never modifies anything on disk. Confined to the configured download/movies/tv directories.

```bash
unarr library stats               # readable table (dry-run — reads only)
unarr library stats --json        # emit the full stats struct as JSON (for scripts)
unarr library stats --workers 4   # limit concurrent ffprobe workers for the quality pass
```

**Three blocks:**

- **Composition** — number of movies, shows, seasons and episodes, plus the **real on-disk space** (allocated blocks, like `du` — not apparent size) per category (Movies vs TV Shows vs Downloads) and the average size per title.
- **Health / reclaimable** — the same sweep `unarr library clean` performs, in dry-run: stubs, orphaned partials, duplicates, orphaned sidecars, empty dirs and media-named dirs — grouped by category with the total space reclaimable. Run `unarr library clean --apply` to actually free it.
- **Quality** — resolution (2160p/1080p/720p/480p/SD), video codec (h265/h264/av1/other) and HDR breakdown, extracted with ffprobe. A file ffprobe can't read is counted as `unknown` and never aborts the report. Probing a large library end-to-end can take a while (bounded by `--workers`, default 8).

`--json` emits the full `LibraryStats` struct (composition, health, quality) for piping into scripts.

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
api_url = "https://unarr.app"

[agent]
id = "auto-generated-uuid"
name = "My PC"

[downloads]
dir = "~/Media"
# Ordered download-method preference. The web honours this list, so anything NOT
# listed is disabled — e.g. ["debrid"] means debrid-only and never falls back to
# torrent; ["debrid","usenet"] tries debrid then usenet. Omit (or use ["auto"])
# to let the server decide (default: auto — if you have debrid configured, cached
# titles use debrid automatically, otherwise torrent). Debrid/usenet must be
# configured in your TorrentClaw account — the agent only fetches links the web
# resolves. Requires unarr >= 1.1.5-beta.
preferred_methods = ["auto"]     # e.g. ["debrid"], ["debrid","usenet"], or ["auto"]
# preferred_method = "auto"      # legacy single value (still supported; superseded by preferred_methods)
max_concurrent = 3
max_download_speed = "0"         # e.g. "10MB", "500KB", "0" = unlimited
max_upload_speed = "0"
# Read-only WebDAV export of your organized library (Movies / TV Shows) so
# Infuse, Kodi, VLC, etc. can browse and play it without the web player. Opt-in,
# off by default. Only GET/HEAD/PROPFIND are served — every write verb is 405'd.
# Basic auth: username defaults to "unarr"; password defaults to a stable value
# derived from your API key (shown by `unarr status`) unless you set one. The
# mount is served on the same port as streaming, at /dav/. Requires unarr >= 1.5.0-beta.
webdav_enabled = false
# webdav_username = "unarr"
# webdav_password = ""           # blank = derive from the API key
# The mount answers only your local network (LAN, Tailscale, loopback);
# everything else gets a 404, so it stays private even when the stream port is
# reachable from the internet. Turning this on removes that restriction, and the
# Basic auth left protecting it has NO rate limiting — an exposed mount is an
# unthrottled password-guessing target. Remote playback does not use WebDAV, so
# leaving it off costs you nothing there.
# webdav_allow_wan = false

# Publish the HTTPS streaming port to your router (UPnP/NAT-PMP) so remote
# playback uses your agent's stable direct-TLS address instead of a CloudFlare
# quick tunnel, which changes hostname on every restart and gets rate-limited.
# On by default: that listener is TLS-only and every request needs a token the
# web mints, so nothing unauthenticated is reachable. The lease is renewed
# automatically and removed on shutdown. Set to false to never touch your router.
# auto_https_upnp = true

[organize]
enabled = true
movies_dir = "~/Media/Movies"
tv_shows_dir = "~/Media/TV Shows"

# Library-hygiene sweep — runs automatically after each auto-scan, and manually
# via `unarr library clean`. Removes only deterministic junk; a valid video
# (>= min_video_bytes) is NEVER removed. See the "Library clean" section above.
[library.cleanup]
enabled = true                   # run automatically after each auto-scan
min_video_bytes = "1MiB"         # anti-stub floor; a video below this is a stub
remove_stubs = true              # delete videos below min_video_bytes
remove_orphan_partials = true    # delete .part/.!qB/.aria2/.tmp/.partial with no active task
dedup_exact = true               # collapse byte-identical duplicate videos (keeps one canonical copy)
remove_orphan_subtitles = true   # delete sidecars with no owning video (.unarr checks the parent dir)
prune_empty_dirs = true          # remove video-less dirs and media-named dirs

[daemon]
auto_upgrade = true      # apply server-flagged upgrades in-place (since 0.9.6)
status_interval = "3s"   # how often a running download reports progress

# Log file (unarr.log in the data dir). Rotation is OPT-IN and off by default:
# read the limitations in the "Logs" section above before turning it on. With 0
# the log grows until `unarr clean` or an external logrotate trims it.
log_max_size_mb = 0      # >0 = rotate unarr.log once it reaches this many MB; 0 = never rotate
log_max_files = 3        # rotated copies to keep (unarr.log.1 … .3)
log_level = "info"       # default minimum severity for `unarr logs`: debug|info|warn|error
log_format = "text"      # `unarr logs` output: "text" (verbatim) or "json" (json-lines)

[notifications]
enabled = true

# Read by the desktop companion (tray app), not the daemon: which player the
# web's Play button opens. Empty = autodetect. See [desktop] below.
[desktop]
player = ""
# player_command = "flatpak run org.videolan.VLC --start-time={start} -- {url}"

[general]
country = "US"
```

### Streaming reference

The in-browser player on unarr.app streams from the daemon over HLS
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
Tailscale-connected cases, but the **browser-based player on unarr.app
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

#### `[desktop]` — which player Play opens

Read by the **unarr desktop companion** (the tray app that registers the
`unarr://` handler), not by the daemon. Pressing Play on the web sends an
`unarr://play` link to it, and this section decides what opens.

```toml
[desktop]
# Empty = autodetect. Or: "mpv", "vlc", "iina" (macOS), "mpc" (Windows),
# "system" (whatever your OS opens video with).
player = ""

# An explicit command line, for players none of those names reach.
# Wins over `player`. See below.
# player_command = "flatpak run org.videolan.VLC --start-time={start} -- {url}"
```

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `player` | string | `""` | `""` = autodetect. `"mpv"` also resolves **Celluloid** (it embeds mpv and installs no `mpv` binary). `"system"` uses your OS default video app. A player that isn't installed falls back to autodetect **and notifies you** — the setting is never silently ignored. Watching in the browser is not a value here: that's the web's own "Web player" button. |
| `player_command` | string | `""` | Explicit command line with placeholders (below). Outranks `player`; while it is set, the tray's Player submenu is shown disabled. |

**How the player is chosen**, most specific first:

1. `player_command` — your own command line
2. `player` — a name from the table above
3. autodetect — mpv/Celluloid → VLC → IINA (macOS) / MPC-HC (Windows)
4. your OS default video application
5. the browser, as a last resort, so a click always plays *something*

Step 5 is a fallback, not a choice: if you want to watch in the browser, the
web has its own "Web player" button, and going through the desktop app to get
there would be a second route to the same place. Steps 1 and 4 exist because a
Flatpak/Snap/AppImage install exposes no binary on PATH, and mpv.net or
SMPlayer are their own spellings. Step 4 asks the system directly
(freedesktop MIME association on Linux, LaunchServices on macOS, the
registered file association on Windows), so it finds those — but it can only
pass a URL, so playback starts from the beginning. Steps 2 and 3 know the
player's flags and can pass resume position, title and language preferences.

**`player_command` placeholders.** The template is split into arguments here
and executed directly — never through a shell — so quoting is the only shell
syntax that applies. An argument whose placeholder has no value for a given
stream is dropped whole, so `--start={start}` simply disappears when there is
nothing to resume.

| Placeholder | Value |
|---|---|
| `{url}` | The stream URL. Appended automatically if the template never mentions it. |
| `{web}` | The unarr web player page for this stream (falls back to `{url}`). |
| `{start}` | Resume position in seconds. Absent when starting from the beginning. |
| `{title}` | Display title for the player window/OSD. |
| `{alang}` / `{slang}` | Preferred audio / subtitle languages, comma-separated. |
| `{subfile}` | URL of an external subtitle file to side-load. **First one only** — see the note below. |

> **`{subfile}` carries a single subtitle.** unarr can side-load several
> external subtitle files (AI translations and shared provider subtitles), but a
> placeholder always substitutes inside ONE argument, so a template cannot emit
> a repeatable flag more than once. `{subfile}` therefore expands to the first
> subtitle only, and the argument disappears when there is none. The built-in
> players (mpv, Celluloid, VLC) receive **all** of them — use one of those if
> you want the full subtitle menu.

```toml
# Flatpak VLC (no `vlc` on PATH)
player_command = "flatpak run org.videolan.VLC --start-time={start} -- {url}"

# mpv.net on Windows
player_command = 'C:\Program Files\mpv.net\mpvnet.exe --start={start} --force-media-title={title} -- {url}'

# SMPlayer (its own flag spellings)
player_command = "smplayer -media-title {title} -start {start} {url}"
```

### Environment variables

Environment variables override config file values:

```bash
export UNARR_API_KEY=tc_your_api_key
export UNARR_API_URL=https://unarr.app
export UNARR_COUNTRY=ES
export UNARR_DOWNLOAD_DIR=~/Media

# Desktop companion — one-off override without editing the TOML
export UNARR_DESKTOP_PLAYER=vlc
export UNARR_DESKTOP_PLAYER_COMMAND="flatpak run org.videolan.VLC -- {url}"
```

### Speed limits

Speed limits use human-readable format:

```toml
max_download_speed = "10MB"    # 10 megabytes/sec
max_upload_speed = "1MB"       # 1 megabyte/sec
max_download_speed = "500KB"   # 500 kilobytes/sec
max_download_speed = "0"       # unlimited (default)
```

### Telemetry

The agent reports a small set of **lifecycle events** to the server — enough to
see why an agent that connected never came back:

- **Onboarding:** login succeeded, first sync completed, or a start-up failure
  (missing config, a stream port already in use, a permission error).
- **Exit:** whether the daemon was stopped by you, crashed, or exited normally.

No file names, library contents, search terms, or download activity are sent —
only these lifecycle signals plus the agent's version and OS. It helps us fix the
setup problems that make agents fail on the first run.

**To turn it off completely:**

```toml
[telemetry]
enabled = false
```

or with an environment variable (wins over the config file — handy for
headless/Docker):

```bash
UNARR_TELEMETRY=off unarr start
```

When disabled the agent sends **nothing** — it still registers, syncs, and
downloads exactly the same. Telemetry is purely additive.

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

## Scan

Walk a folder recursively, analyze each video file with ffprobe, and sync quality data to your account.

```bash
unarr scan ~/Media              # scan default download dir
unarr scan /mnt/nas/Movies      # scan a specific path
unarr scan ~/Media --status     # show last scan results without re-scanning
unarr scan ~/Media --workers 4  # use 4 parallel ffprobe workers
unarr scan ~/Media --no-sync    # analyze locally without uploading results
```

The daemon also runs an automatic background scan when it detects new files in the download directory.

## Mirrors

Mirrors are alternate base URLs the agent falls back to when the primary domain is unreachable — useful for bypassing DNS blocks, ISP filters, or short-lived outages without restarting the agent.

```bash
unarr mirrors list     # show currently configured mirrors
unarr mirrors update   # refresh from the server's canonical list
unarr mirrors test     # probe every configured mirror for latency and reachability
```

## Coming Soon

These commands are planned for future releases:

| Command | Description |
|---------|-------------|
| `unarr moreseed` | Find same quality with more seeders |
| `unarr compare` | Compare two torrents side by side |
| `unarr monitor` | Watch for new episodes of a series |
| `unarr open` | Open content in the browser |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, code style, and guidelines.

## License

MIT License — see [LICENSE](LICENSE) for details.
