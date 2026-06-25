# unarr

**The single binary that replaces your whole *arr stack.** Built-in torrent,
debrid, and usenet engines. Stream, transcode, and organize your library from
one terminal — or run it as a headless daemon with a web dashboard, WireGuard
split-tunnel, and Cloudflare Funnel remote access.

**[Website & docs](https://unarr.app)** · **[Install guide](https://unarr.app/cli)** · **[Get an API key](https://unarr.app)**

> unarr unifies multiple torrent, debrid, and usenet sources, enriched with
> TMDB metadata and a 0–100 quality score per release.

---

## Quick start

### 1. First-time setup (interactive wizard)

```bash
docker run -it --rm \
  -v ~/.config/unarr:/config \
  unarr/cli setup
```

The wizard asks for your unarr API key (free at
[unarr.app](https://unarr.app)) and your download directory.

### 2. Run the daemon

```bash
docker run -d --name unarr \
  --restart unless-stopped \
  --network host \
  --read-only --memory 512m \
  -v ~/.config/unarr:/config \
  -v ~/Media:/downloads \
  unarr/cli
```

That's it — `unarr` now runs headless, watching for jobs and managing downloads.

---

## Docker Compose

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
      - TZ=UTC
      # - UNARR_API_KEY=tc_your_key_here
    network_mode: host        # recommended for full P2P performance
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: "2.0"

volumes:
  unarr-data:
```

```bash
docker compose run --rm unarr setup   # one-time wizard
docker compose up -d                   # start the daemon
```

---

## Volumes

| Path         | Purpose                                          |
|--------------|--------------------------------------------------|
| `/config`    | Configuration file (`config.toml`)               |
| `/downloads` | Finished media downloads                         |
| `/data`      | Internal state: torrent metadata, cache          |

## Environment variables

| Variable               | Description                          | Default                   |
|------------------------|--------------------------------------|---------------------------|
| `UNARR_API_KEY`        | TorrentClaw API key                  | from config               |
| `UNARR_API_URL`        | API endpoint                         | `https://unarr.app` |
| `UNARR_DOWNLOAD_DIR`   | Download directory                   | `/downloads`              |
| `UNARR_CONFIG_DIR`     | Config directory                     | `/config`                 |
| `UNARR_COUNTRY`        | Country code (ISO 3166)              | `US`                      |
| `TZ`                   | Timezone                             | `UTC`                     |

Any config value can be overridden by its matching `UNARR_*` environment variable.

## Networking

**Host mode (recommended)** — full P2P performance, no port mapping:

```yaml
network_mode: host
```

**Bridge mode** — more isolated, but you must expose the BitTorrent ports:

```yaml
ports:
  - "6881-6889:6881-6889/tcp"
  - "6881-6889:6881-6889/udp"
```

## Running commands

Use `docker exec` for one-off commands while the daemon is running:

```bash
docker exec unarr unarr search "inception" --quality 1080p
docker exec unarr unarr popular --limit 10
docker exec unarr unarr status
docker exec unarr unarr doctor      # diagnose config / connectivity
```

---

## Tags

| Tag      | Description                                      |
|----------|--------------------------------------------------|
| `latest` | Latest stable release                            |
| `X.Y.Z`  | Exact version (e.g. `0.9.0`)                      |
| `X.Y`    | Latest patch within a minor (e.g. `0.9`)         |

Pin a tag in production (`unarr/cli:0.9.0`) for reproducible deploys.

## Supported architectures

Multi-arch image — Docker pulls the right one automatically:

- `linux/amd64`
- `linux/arm64` (Apple Silicon, Raspberry Pi 4/5, ARM servers)

## Image details

- **Base:** Alpine 3.22 (minimal, regularly patched)
- **User:** `unarr` (UID 1000, GID 1000) — runs as **non-root**
- **Entrypoint:** `unarr start` (daemon mode)
- **Read-only rootfs** — only mounted volumes are writable
- **Bundled `ffmpeg` / `ffprobe`** for media inspection — nothing else to install
- **Self-contained updates** — binaries are served from TorrentClaw's own
  infrastructure, no third-party registry dependency

---

## Other install methods

Not using Docker? Install the native binary instead:

```bash
# Linux / macOS
curl -fsSL https://unarr.app/install.sh | sh

# Windows (PowerShell)
irm https://unarr.app/install.ps1 | iex

# Go toolchain
go install github.com/Unarr-app/unarr-cli/cmd/unarr@latest
```

## Mirrors

The installer and release binaries are served from every TorrentClaw mirror, so
you can install even if one domain is blocked in your region. Each mirror is
self-contained (it serves its own binaries — no cross-domain dependency):

| Mirror | Install command |
|--------|-----------------|
| `unarr.app` (primary) | `curl -fsSL https://unarr.app/install.sh \| sh` |
| Tor (`.onion`) | `torsocks sh -c "$(curl http://torrentf3aifidcsaaanmnmuhv2s53r6hqsl3zkmfidiaxainkeqk5id.onion/install.sh)"` |

The Tor address routes everything (install script + binaries) through the hidden
service, so no clearnet exit is needed.

## Links

- **Website & docs:** https://unarr.app
- **CLI install guide:** https://unarr.app/cli
- **API & account:** https://unarr.app
- **Mirror status:** https://unarr.app/mirrors

## License

MIT.
