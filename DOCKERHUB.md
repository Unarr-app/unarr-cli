# unarr

Powerful terminal tool for torrent search and management. Search 30+ sources, inspect quality, discover popular content, find streaming providers, and manage downloads — all from your terminal.

**[GitHub](https://github.com/torrentclaw/unarr)** | **[Documentation](https://github.com/torrentclaw/unarr#readme)** | **[Releases](https://github.com/torrentclaw/unarr/releases)**

## Quick Start

### 1. Setup (interactive wizard)

```bash
docker run -it --rm \
  -v ~/.config/unarr:/config \
  torrentclaw/unarr setup
```

### 2. Run the daemon

```bash
docker run -d --name unarr \
  --restart unless-stopped \
  --network host \
  --read-only --memory 512m \
  -v ~/.config/unarr:/config \
  -v ~/Media:/downloads \
  torrentclaw/unarr
```

## Docker Compose

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
      - TZ=UTC
      # - UNARR_API_KEY=tc_your_key_here
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: "2.0"
    network_mode: host

volumes:
  unarr-data:
```

## Volumes

| Path | Purpose |
|------|---------|
| `/config` | Configuration file (`config.toml`) |
| `/downloads` | Finished media downloads |
| `/data` | Internal state: torrent metadata, cache |

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `TZ` | Timezone | `UTC` |
| `UNARR_API_KEY` | TorrentClaw API key | from config |
| `UNARR_API_URL` | API endpoint | `https://torrentclaw.com` |
| `UNARR_DOWNLOAD_DIR` | Download directory | `/downloads` |
| `UNARR_CONFIG_DIR` | Config directory | `/config` |
| `UNARR_COUNTRY` | Country code (ISO 3166) | `US` |

## Networking

**Host mode** (recommended) gives full P2P performance with no port management:

```yaml
network_mode: host
```

**Bridge mode** — more isolated, but requires explicit ports:

```yaml
ports:
  - "6881-6889:6881-6889/tcp"
  - "6881-6889:6881-6889/udp"
```

## Running Commands

Use `docker exec` for one-off commands while the daemon is running:

```bash
docker exec unarr unarr search "inception" --quality 1080p
docker exec unarr unarr popular --limit 10
docker exec unarr unarr status
docker exec unarr unarr doctor
```

## Supported Architectures

| Architecture | Tag |
|-------------|-----|
| `linux/amd64` | `latest`, `0.3`, `0.3.5` |
| `linux/arm64` | `latest`, `0.3`, `0.3.5` |

## Tags

| Tag | Description |
|-----|-------------|
| `latest` | Latest stable release |
| `X.Y.Z` | Specific version (e.g. `0.3.5`) |
| `X.Y` | Latest patch for minor version (e.g. `0.3`) |

## Image Details

- **Base image:** Alpine 3.22
- **User:** `unarr` (UID 1000, GID 1000)
- **Entrypoint:** `unarr start` (daemon mode)
- **Read-only filesystem** — only mounted volumes are writable
- **No root required** — runs as non-root by default

## License

MIT License — see [LICENSE](https://github.com/torrentclaw/unarr/blob/main/LICENSE) for details.
