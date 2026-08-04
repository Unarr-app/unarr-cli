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
      - PUID=1000               # your host user id — see PUID/PGID below (NAS ≠ 1000)
      - PGID=1000
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
| `PUID`               | User id the agent runs as    | `1000`              |
| `PGID`               | Group id the agent runs as   | `1000`              |

Any config value can be overridden by its matching `UNARR_*` environment variable.

### PUID / PGID (NAS users: read this)

The container runs as `1000:1000` unless you say otherwise, which matches a
typical Linux desktop but **not** a NAS. If `/config` or `/downloads` is owned
by a different user on the host, the agent gets *permission denied* on its
first write. Pass your own ids:

```bash
docker run -d --name unarr \
  -e PUID=1026 -e PGID=100 \
  -v /volume1/docker/unarr:/config \
  -v /volume1/media:/downloads \
  unarr/cli
```

| Platform    | How to find your ids                                  |
|-------------|-------------------------------------------------------|
| Synology    | SSH in, run `id <your-user>` (uids usually start 1024) |
| unRAID      | `99` / `100` (the standard `nobody:users`)             |
| QNAP        | SSH in, run `id admin`                                 |
| Linux/macOS | `id -u` / `id -g` (often already `1000`)               |

**What the entrypoint actually does with the mounts.** It starts as root, then:

- `/config`, `/data` and the home directory are chowned **recursively** to
  `PUID:PGID` — they are small and entirely owned by the agent.
- `/downloads` gets **only its mount point** chowned. Your library can be
  terabytes and a recursive chown on every start would stall the container for
  minutes.
- It then drops to `PUID:PGID` and everything unarr **creates** from that point
  is owned by `PUID:PGID`.

So **files that were already in your library keep their original owner**. If
they belong to a different uid, unarr can still fail to rename, move or delete
*those* files. That is fixed once, on the host, not by the container:

```bash
chown -R 1026:100 /volume1/media     # your PUID:PGID
```

**Running as root** is opt-in: set `PUID=0` explicitly. Anything else keeps the
privilege drop, and if privileges cannot be dropped the container refuses to
start rather than writing root-owned files onto your shares.

**Kubernetes.** The image has no baked `USER`, so a pod with
`runAsNonRoot: true` must also set `runAsUser` (and `runAsGroup`):

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1026
  runAsGroup: 100
  supplementalGroups: [100]
```

With `runAsUser` set, the container is already unprivileged: the entrypoint
skips the chown and the privilege drop entirely and execs the agent directly, so
the mount permissions must be right on the host (or via `fsGroup`).

**Supplementary groups.** In the default (root → drop) path the agent ends up in
exactly one group, `PGID` — extra groups added with `--group-add` are not
carried over. If a share depends on a secondary group, start the container
unprivileged instead, which preserves the runtime's full group list:

```bash
docker run -d --name unarr \
  --user 1026:100 --group-add 65539 \
  -v /volume1/docker/unarr:/config \
  -v /volume1/media:/downloads \
  unarr/cli
```

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
docker exec unarr unarr doctor         # diagnose config / connectivity
docker exec unarr unarr support-bundle # one redacted file to attach to an issue
```

---

## Health check

The image ships a `HEALTHCHECK`, so a container whose daemon has died shows as
`unhealthy` instead of `running`:

```bash
docker ps                                    # STATUS column shows (healthy)
docker inspect --format '{{.State.Health.Status}}' unarr
docker compose up -d --wait                  # blocks until the daemon is really up
```

It runs `unarr doctor --quick`, which checks only local things — config
readable, download dir writable, disk space, and whether the daemon process is
actually alive — and **makes no network call**. That is deliberate: a probe
that reached the API would flip the container unhealthy on any internet blip,
and Docker's response to unhealthy is to restart. Warnings never mark the
container unhealthy for the same reason.

Run it yourself to see what it sees:

```bash
docker exec unarr unarr doctor --quick
docker exec unarr unarr doctor --quick --json   # exit 1 when unhealthy
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

- **User:** starts as root only to prepare the mounts, then drops to
  `PUID:PGID` (default `1000:1000`) — the agent itself runs as **non-root**.
  Kubernetes `runAsNonRoot` needs an explicit `runAsUser` (see PUID/PGID above)
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
