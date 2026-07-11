# Releasing unarr

Distribution is **entirely on GitHub Actions** (`github.com/Unarr-app/unarr-cli`).
A `vX.Y.Z` tag push triggers `.github/workflows/release.yml`, which builds, signs,
and publishes the GitHub Release plus the multi-arch Docker Hub image. There is no
local pipeline and no self-hosted mirror step anymore.

> The old self-hosted Hetzner backup (`make ship` → `scripts/ship.sh` +
> `torrentclaw-web/scripts/publish-cli-release.sh` → `/opt/torrentclaw/releases` +
> `version.txt`) was **removed 2026-07-11**. `ship.sh` and the web `/version` &
> `/releases/download` routes no longer exist — GitHub Releases is the single source.

## The release ritual

```bash
# 1. Bump version.go + CHANGELOG + tag (no publish yet)
make release V=1.3.7-beta

# 2. Push → GitHub Actions builds, signs, and publishes everything
#    (release.yml: goreleaser cross-compile + ffmpeg bundle + ed25519 sign +
#     GitHub Release upload + multi-arch Docker Hub push)
git push github main --follow-tags
```

Requires repo secrets `RELEASE_SIGNING_KEY`, `DOCKERHUB_USERNAME`,
`DOCKERHUB_TOKEN` (and optionally `SENTRY_DSN`, `HOMEBREW_TAP_TOKEN`). Watch the
run: `gh run watch <id> -R Unarr-app/unarr-cli --exit-status`.

**Push only with the `lucaiarr` gh account** (`gh auth switch -u lucaiarr &&
gh auth setup-git`); it is the only account allowed to push to the repo.

## How the self-updater finds releases

- **Primary:** `github.com/Unarr-app/unarr-cli/releases/download/v{ver}/...`
- **Latest version:** `GET /releases?per_page=100`, picking the **highest semver
  client-side** — the list endpoint is NOT semver-ordered, so never trust `releases[0]`.
- Signature: ed25519 over `checksums.txt`; the public key is compiled in
  (`internal/upgrade/signature.go`), the private key is the `RELEASE_SIGNING_KEY`
  CI secret (+ Bitwarden backup).
- **Compiled-in `fallbackBaseURL` (`cfg.Auth.APIURL`, → the web origin) is now a
  DEAD endpoint** for already-deployed agents: the web mirror routes it pointed at
  (`/version`, `/releases/download`) were removed on 2026-07-11. A GitHub outage no
  longer has a failover (accepted tradeoff of going GitHub-only). Dropping the
  fallback from the binary is a future, separate release.

## CI

- `.github/workflows/ci.yml` — test (race) / vet / golangci-lint / build matrix /
  coverage floor, on push + PR.
- `.github/workflows/release.yml` — the release pipeline above, on `v*` tag push.
- `.github/workflows/docker-rebuild.yml` — weekly refresh of `:latest` so base
  image patches land between tagged releases.
