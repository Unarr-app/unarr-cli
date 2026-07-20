# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| latest  | :white_check_mark: |
| < latest | :x:               |

Only the latest release receives security updates.

## Reporting a Vulnerability

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, report them via **GitHub Security Advisories**:

1. Go to [Security Advisories](https://github.com/Unarr-app/unarr-cli/security/advisories)
2. Click **"Report a vulnerability"**
3. Fill in the details

Alternatively, email **security@unarr.app** with:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

## Response Timeline

- **Acknowledgment**: within 48 hours
- **Initial assessment**: within 5 business days
- **Fix and disclosure**: coordinated with reporter, typically within 30 days

## Scope

The following are in scope:

- Command injection or arbitrary code execution
- Path traversal or file access outside intended directories
- Authentication bypass or credential exposure
- Denial of service in the daemon
- Dependency vulnerabilities with exploitable impact

The following are out of scope:

- Vulnerabilities in torrent protocol itself (BitTorrent DHT, peer exchange)
- Issues requiring physical access to the machine
- Social engineering attacks

## Security Practices

This project follows these security practices:

- **No hardcoded credentials** — API keys stored in config files with 0600 permissions
- **Path traversal protection** — All file operations validated through `safePath()`
- **HTTPS by default** — All API communication uses TLS
- **Response size limits** — API responses capped at 1MB
- **Non-root Docker** — Container runs as unprivileged user (UID 1000)
- **Dependency scanning** — Automated via Dependabot

## What the agent exposes to the network

The daemon serves two listeners on your machine. They have different exposure
rules because they have different authentication.

| Listener | Reaches the internet? | Protected by |
|---|---|---|
| Cleartext stream port | Only if you set `enable_upnp = true` | Nothing — hence opt-in |
| HTTPS stream port (direct-TLS) | Yes, by default (`auto_https_upnp = true`) | TLS + a per-request token the web mints |

`auto_https_upnp` asks your router (UPnP/NAT-PMP) to forward the HTTPS port so
remote playback reaches a stable address with a valid certificate, rather than a
CloudFlare quick tunnel that changes hostname on every restart. It is on by
default because that listener is TLS-only and rejects any request without a
valid token. The lease is renewed while the daemon runs and removed on shutdown;
if the daemon is killed it expires on its own within 2 hours.

Set `auto_https_upnp = false` if you would rather the agent never touch your
router. Remote playback then falls back to the tunnel.

**The WebDAV mount is not part of this.** `/dav/` shares the same mux, but it
answers only callers on your local network (loopback, RFC1918, link-local, and
Tailscale's `100.64.0.0/10`) — anything else gets a 404, not an auth challenge,
so a port scan cannot even tell the mount is there. Publishing the stream port
exposes playback, never your library.

`webdav_allow_wan = true` lifts that restriction. Think before setting it: the
only thing left protecting the mount is HTTP Basic auth with **no rate
limiting**, which makes it an unthrottled online password-guessing target. The
daemon logs a warning at startup whenever that flag is on and the port is
published.

## Container Image Vulnerability Scanning

The Docker image (`unarr/cli`) is scanned by Docker Scout on Docker Hub and
by a CVE gate in CI (see `.github/workflows/`). Two things matter when reading the
Docker Hub vulnerability count:

- **Scanner database differs.** Docker Hub (Scout) matches `package@version` against
  NVD/GHSA. Trivy/Alpine `secdb` only lists CVEs Alpine has acknowledged and patched.
  A high Scout count with a clean Trivy report is expected, not a contradiction.
- **The bulk comes from the bundled `ffmpeg` codec stack.** Alpine's `ffmpeg`
  package pulls ~40 codec/parser libraries (`x264`, `x265`, `libvpx`, `aom`,
  `dav1d`, `libtheora`, `libvorbis`, `libwebp`, `libbluray`, `libopenmpt`, …).
  Each carries a long NVD history that Alpine does not backport. ffmpeg is a
  **functional dependency** — the HLS transcode pipeline shells out to
  `ffmpeg`/`ffprobe` to decode untrusted media and re-encode to H.264 + AAC.

### Accepted risk and policy

- **Fixable** CRITICAL/HIGH findings **block** a release (CI CVE gate, `only-fixed`).
- **Unfixed-upstream** codec CVEs are tracked but **accepted**: there is no patched
  Alpine package to move to, and dropping codecs would break playback of common
  formats. They are mitigated by the hardening below rather than eliminated.
- Images are **rebuilt and re-pushed weekly** (scheduled workflow) so any newly
  *fixed* base/ffmpeg/Go patch lands between tagged releases.

### Mitigations (run the container hardened)

Crafted media (torrents are untrusted input) is the realistic attack vector against
ffmpeg's parsers. The shipped `docker-compose.yml` already applies:

- **Non-root** user (UID 1000), **read-only** root filesystem, writable `tmpfs` only.
- **Resource limits** (memory/CPU) to bound a runaway decode.

Recommended additions for exposed deployments:

```yaml
    cap_drop: ["ALL"]
    security_opt:
      - no-new-privileges:true
```

If you do not need HLS transcoding, you can run with transcoding disabled to
avoid feeding untrusted media to ffmpeg at all.

## Disclosure Policy

We follow coordinated disclosure. We will credit reporters in the release notes unless they prefer to remain anonymous.
