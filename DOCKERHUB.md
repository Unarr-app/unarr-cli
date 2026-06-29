# unarr

**Your self-hosted media agent in a single binary.** Organize your library,
stream and transcode to any device on your network, and connect your debrid
account — all from one headless daemon with a web dashboard, WireGuard
split-tunnel, and Cloudflare Funnel remote access.

**[Website & docs](https://unarr.app)** · **[Install guide](https://unarr.app/cli)** · **[Get an API key](https://unarr.app)**

> Pairs with the **[unarr.app](https://unarr.app)** web app: rich metadata from
> TMDB, a 0–100 quality score per release, and one-tap play to your TV, phone, or
> browser.

---

## Quick start

### 1. First-time setup (interactive wizard)

```bash
docker run -it --rm \
  -v ~/.config/unarr:/config \
  unarr/cli setup
```

The wizard asks for your unarr API key (free at [unarr.app](https://unarr.app),
under **Profile → API keys**) and your media directory.

### 2. Run the daemon

```bash
docker run -d --name unarr \
  --restart unless-stopped \
  --network host \
  -v ~/.config/unarr:/config \
  -v ~/Media:/downloads \
  -v unarr-data:/data \
  unarr/cli
```

That's it — `unarr` now runs headless, ready to stream and manage your library.
`--network host` lets it reach your TV, phone, and Chromecast on the LAN.

---

## Docker Compose

```yaml
services:
  unarr:
    image: unarr/cli:latest
    pull_policy: always
    container_name: unarr
    restart: unless-stopped
    network_mode: host        # recommended — reaches devices on your LAN
    environment:
      - TZ=UTC
      # - UNARR_API_KEY=your_key_here
    volumes:
      - ~/.config/unarr:/config
      - ~/Media:/downloads
      - unarr-data:/data

volumes:
  unarr-data:
```

```bash
docker compose run --rm unarr setup   # one-time wizard
docker compose up -d                   # start the daemon
```

---

## Volumes

| Path         | Purpose                                   |
|--------------|-------------------------------------------|
| `/config`    | Configuration file (`config.toml`)        |
| `/downloads` | Your media library                        |
| `/data`      | Internal state & cache                     |

## Environment variables

| Variable             | Description                  | Default             |
|----------------------|------------------------------|---------------------|
| `UNARR_API_KEY`      | unarr API key                | from config         |
| `UNARR_API_URL`      | API endpoint                 | `https://unarr.app` |
| `UNARR_DOWNLOAD_DIR` | Media directory              | `/downloads`        |
| `UNARR_CONFIG_DIR`   | Config directory             | `/config`           |
| `UNARR_COUNTRY`      | Country code (ISO 3166)      | `US`                |
| `TZ`                 | Timezone                     | `UTC`               |

Any config value can be overridden by its matching `UNARR_*` environment variable.

## Networking

**Host mode (recommended)** — `--network host` / `network_mode: host`. Lets the
agent reach your TV, phone, and Chromecast directly on the LAN for local
streaming, with no port mapping.

**Bridge mode** — more isolated; map the agent's stream/control ports yourself:

```yaml
ports:
  - "11818:11818"
  - "11819:11819"
```

## Hardware transcode

The image ships the NVIDIA runtime env, so GPU transcode works out of the box:

- **NVIDIA:** add `--gpus all`
- **Intel QSV / VA-API:** pass `--device /dev/dri`

## Running commands

Use `docker exec` for one-off commands while the daemon is running:

```bash
docker exec unarr unarr status
docker exec unarr unarr doctor      # diagnose config / connectivity
```

---

## Tags

| Tag        | Description                                  |
|------------|----------------------------------------------|
| `latest`   | Latest release                               |
| `X.Y.Z`    | Exact version (e.g. `1.3.0-beta`)            |
| `X.Y`      | Latest patch within a minor (e.g. `1.3`)     |

Pin a tag in production (`unarr/cli:1.3`) for reproducible deploys.

## Supported architectures

Multi-arch image — Docker pulls the right one automatically:

- `linux/amd64`
- `linux/arm64` (Apple Silicon, Raspberry Pi 4/5, ARM servers)

## Image details

- **User:** `unarr` (UID 1000, GID 1000) — runs as **non-root**
- **Entrypoint:** `unarr start` (daemon mode)
- **Bundled `ffmpeg` / `ffprobe`** for transcode & inspection — nothing else to install
- **Signed releases** — binaries are published as **[GitHub Releases](https://github.com/Unarr-app/unarr-cli/releases)**;
  `checksums.txt` is ed25519-signed and the self-updater verifies it before applying

---

## Other install methods

Not using Docker? Install the native binary instead:

```bash
# Linux / macOS
curl -fsSL https://unarr.app/install.sh | sh

# macOS (Homebrew)
brew install unarr-app/tap/unarr

# Windows (PowerShell)
irm https://unarr.app/install.ps1 | iex

# Go toolchain
go install github.com/Unarr-app/unarr-cli/cmd/unarr@latest
```

## Links

- **Website & docs:** https://unarr.app
- **CLI install guide:** https://unarr.app/cli
- **Source:** https://github.com/Unarr-app/unarr-cli
- **Releases:** https://github.com/Unarr-app/unarr-cli/releases

## License

MIT.
