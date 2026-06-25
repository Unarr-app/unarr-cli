# Releasing unarr

Distribution moved off the self-hosted Forgejo/Hetzner pipeline to **GitHub
Releases** (`github.com/Unarr-app/unarr-cli`). Hetzner stays as a **permanent
backup** — the self-updater fails over to it if GitHub is ever unreachable
(e.g. an account takedown). That backup is only useful if it stays current, so
**every release publishes to BOTH GitHub and Hetzner**.

## The release ritual

```bash
# 1. Bump version.go + CHANGELOG + tag (no publish yet)
make release V=1.2.2-beta

# 2. Push → GitHub Actions builds, signs, and publishes the GitHub Release
#    (.github/workflows/release.yml: goreleaser + ed25519 sign + Docker Hub)
git push github main --follow-tags

# 3. Mirror the SAME build to the Hetzner backup (Docker already done by Actions)
SKIP_DOCKER=1 make ship
```

- **GitHub Release + Docker Hub** come from `release.yml` on the `v*` tag push.
  Requires repo secrets `RELEASE_SIGNING_KEY`, `DOCKERHUB_USERNAME`,
  `DOCKERHUB_TOKEN` (and optionally `SENTRY_DSN`).
- **Hetzner backup** comes from `make ship` (`scripts/ship.sh`): it runs
  `goreleaser release --skip=publish` (build only) + `publish-cli-release.sh`
  (rsync to `/opt/torrentclaw/releases` over Tailscale + flip `version.txt`).
  GitHub-hosted runners can't reach the Tailscale-only Hetzner box, so this step
  is local. Use `SKIP_DOCKER=1` because Actions already pushed the image.

> If you skip step 3, the GitHub release still works, but the Hetzner backup
> goes stale — and the updater's failover would hand users an old version on a
> GitHub outage.

## How the self-updater finds releases

- **Primary:** `github.com/Unarr-app/unarr-cli/releases/download/v{ver}/...`
- **Fallback:** the agent's API host (`cfg.Auth.APIURL`, → Hetzner) for the
  archive, `checksums.txt`, `.sig`, and the version check. `UNARR_UPDATE_BASE`
  overrides the primary (staging/tests).
- **Latest version:** read from `GET /releases?per_page=100`, picking the
  **highest semver client-side** — the GitHub list endpoint is NOT semver-
  ordered (it returned an old tag as `[0]` after a backfill), so never trust
  `releases[0]`.
- Signature: ed25519 over `checksums.txt`; the public key is compiled in
  (`internal/upgrade/signature.go`), the private key is `RELEASE_SIGNING_KEY`.
  `checksums.txt` + `.sig` are byte-identical across mirrors (one goreleaser
  build), so a signature from either host verifies the other's checksums.

## CI

- `.github/workflows/ci.yml` — test (race) / vet / golangci-lint / build matrix
  / coverage floor, on push + PR.
- `.github/workflows/docker-rebuild.yml` — weekly refresh of `:latest` so base
  image patches land between tagged releases.
