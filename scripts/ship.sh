#!/usr/bin/env bash
#
# ship.sh — End-to-end CLI release pipeline.
#
# Standalone backup for when GitHub Actions is unavailable (org shadow-ban,
# CI outage, etc). Mirrors what release.yml + docker job in CI would do.
#
# Pre-requisites:
#   - scripts/release.sh already ran → version.go bumped + tag created locally
#   - SENTRY_DSN exported (Sentry disabled in build if missing)
#   - docker logged in to docker.io as the org user
#   - SSH key for Hetzner publishing (see publish-cli-release.sh)
#
# Pipeline:
#   1. Sanity: clean tree, tag at HEAD, version.go matches
#   2. goreleaser build (skip GH publish — produces dist/*)
#   3. Rsync to Hetzner via web/scripts/publish-cli-release.sh
#   4. Multi-arch Docker build + push (amd64 + arm64) to Docker Hub
#   5. Smoke checks (torrentclaw.com/version + docker run image version)
#   6. Prune Forgejo releases older than FORGEJO_PRUNE_DAYS (default 90)
#   7. Optional `git push --follow-tags`
#
# Usage:
#   scripts/ship.sh                  Detect version from internal/cmd/version.go
#   scripts/ship.sh 0.9.12           Explicit version
#   scripts/ship.sh --dry-run        Preview steps, no side effects
#   scripts/ship.sh --push 0.9.12    Also git-push tag to GH afterwards
#
# Env knobs:
#   SENTRY_DSN              telemetry DSN injected at build time
#   RELEASE_SIGNING_PUBKEY  ed25519 pubkey (base64) for self-update signature check
#   DOCKER_IMAGE            default torrentclaw/unarr
#   PUBLISH_SCRIPT          default ../torrentclaw-web/scripts/publish-cli-release.sh
#   SKIP_DOCKER=1           skip Docker build/push
#   SKIP_HETZNER=1          skip Hetzner publish
#   SKIP_SMOKE=1            skip smoke checks
#   SKIP_FORGEJO_PRUNE=1    skip Forgejo retention prune
#   FORGEJO_TOKEN           PAT with write:repository for prune (no token = skip + warn)
#   FORGEJO_PRUNE_DAYS      retention window, default 90 days
#   FORGEJO_REPO            default torrentclaw/unarr
#
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

DOCKER_IMAGE="${DOCKER_IMAGE:-torrentclaw/unarr}"
PUBLISH_SCRIPT="${PUBLISH_SCRIPT:-$REPO_DIR/../torrentclaw-web/scripts/publish-cli-release.sh}"
SKIP_DOCKER="${SKIP_DOCKER:-0}"
SKIP_HETZNER="${SKIP_HETZNER:-0}"
SKIP_SMOKE="${SKIP_SMOKE:-0}"
SKIP_FORGEJO_PRUNE="${SKIP_FORGEJO_PRUNE:-0}"
FORGEJO_PRUNE_DAYS="${FORGEJO_PRUNE_DAYS:-90}"
FORGEJO_REPO="${FORGEJO_REPO:-torrentclaw/unarr}"
FORGEJO_BASE="${FORGEJO_BASE:-https://git.torrentclaw.com}"

DRY_RUN=false
PUSH_TAG=false
VERSION=""

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}▸${NC} $*"; }
ok()    { echo -e "${GREEN}✓${NC} $*"; }
warn()  { echo -e "${YELLOW}⚠${NC} $*"; }
die()   { echo -e "${RED}✗${NC} $*" >&2; exit 1; }

for a in "$@"; do
  case "$a" in
    --dry-run) DRY_RUN=true ;;
    --push)    PUSH_TAG=true ;;
    -h|--help)
      sed -n '2,/^set /p' "$0" | sed 's/^#\s\?//;$d'
      exit 0 ;;
    [0-9]*)    VERSION="$a" ;;
    *)         die "unknown arg: $a (use --help)" ;;
  esac
done

read_version_go() {
  grep 'var Version' internal/cmd/version.go | sed 's/.*"\(.*\)".*/\1/'
}

REPO_VERSION="$(read_version_go)"
[ -z "$VERSION" ] && VERSION="$REPO_VERSION"
[ -n "$VERSION" ] || die "cannot detect version (pass explicit X.Y.Z)"
TAG="v$VERSION"
MINOR="${VERSION%.*}"

echo ""
echo -e "  ${BOLD}Ship Plan${NC}"
echo -e "  ─────────────────────────────"
echo -e "  Version:        ${GREEN}$TAG${NC}"
echo -e "  Docker image:   $DOCKER_IMAGE:{$VERSION,$MINOR,latest}"
echo -e "  Skip Hetzner:   $SKIP_HETZNER"
echo -e "  Skip Docker:    $SKIP_DOCKER"
echo -e "  Push to GH:     $PUSH_TAG"
echo -e "  Dry run:        $DRY_RUN"
echo ""

# Sanity
[ "$REPO_VERSION" = "$VERSION" ] || die "version.go=$REPO_VERSION ≠ requested $VERSION (bump with make release-* first)"

if [ "$DRY_RUN" = false ]; then
  [ -z "$(git status --porcelain)" ] || die "working tree dirty"
  git rev-parse "$TAG" >/dev/null 2>&1 || die "tag $TAG missing — run scripts/release.sh first"

  HEAD_SHA="$(git rev-parse HEAD)"
  TAG_SHA="$(git rev-parse "$TAG^{commit}")"
  [ "$HEAD_SHA" = "$TAG_SHA" ] || die "HEAD ($HEAD_SHA) ≠ tag commit ($TAG_SHA) — checkout $TAG first"

  command -v goreleaser >/dev/null || die "goreleaser not installed"
  [ "$SKIP_DOCKER" = "1" ] || command -v docker >/dev/null || die "docker not installed"
  [ "$SKIP_HETZNER" = "1" ] || [ -x "$PUBLISH_SCRIPT" ] || die "publish script missing or not executable: $PUBLISH_SCRIPT"

  if [ -z "${SENTRY_DSN:-}" ]; then
    warn "SENTRY_DSN unset — built binaries will have Sentry disabled"
  fi
fi

if [ "$DRY_RUN" = true ]; then
  ok "Dry run complete — no changes made"
  exit 0
fi

# 1. Build
info "goreleaser build ($TAG)"
SENTRY_DSN="${SENTRY_DSN:-}" RELEASE_SIGNING_PUBKEY="${RELEASE_SIGNING_PUBKEY:-}" \
  goreleaser release --clean --skip=publish
ok "dist/ ready"

# 2. Hetzner
if [ "$SKIP_HETZNER" != "1" ]; then
  info "publishing to Hetzner releases volume"
  "$PUBLISH_SCRIPT" "$VERSION"
  ok "Hetzner version.txt flipped to $VERSION"
fi

# 3. Docker
if [ "$SKIP_DOCKER" != "1" ]; then
  info "docker buildx multi-arch push ($DOCKER_IMAGE:$VERSION, :$MINOR, :latest)"
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --build-arg VERSION="$TAG" \
    -t "$DOCKER_IMAGE:$VERSION" \
    -t "$DOCKER_IMAGE:$MINOR" \
    -t "$DOCKER_IMAGE:latest" \
    --push .
  ok "Docker Hub: $DOCKER_IMAGE:{$VERSION,$MINOR,latest}"
fi

# 4. Smoke
if [ "$SKIP_SMOKE" != "1" ]; then
  info "smoke checks"
  if [ "$SKIP_HETZNER" != "1" ]; then
    LIVE_VERSION="$(curl -fsSL https://torrentclaw.com/version 2>/dev/null | tr -d '[:space:]' || echo '')"
    if [ "$LIVE_VERSION" = "$VERSION" ]; then
      ok "torrentclaw.com/version = $LIVE_VERSION"
    else
      warn "torrentclaw.com/version = '$LIVE_VERSION' (expected $VERSION)"
    fi
  fi

  if [ "$SKIP_DOCKER" != "1" ]; then
    DOCKER_VERSION="$(docker run --rm "$DOCKER_IMAGE:$VERSION" version 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)"
    if [ "$DOCKER_VERSION" = "$TAG" ]; then
      ok "docker image $DOCKER_IMAGE:$VERSION reports $DOCKER_VERSION"
    else
      warn "docker image reports '$DOCKER_VERSION' (expected $TAG)"
    fi
  fi
fi

# 6. Forgejo retention prune
if [ "$SKIP_FORGEJO_PRUNE" != "1" ]; then
  if [ -z "${FORGEJO_TOKEN:-}" ]; then
    warn "FORGEJO_TOKEN not set — skipping Forgejo prune (set it to enable >${FORGEJO_PRUNE_DAYS}-day cleanup)"
  else
    info "pruning Forgejo releases older than $FORGEJO_PRUNE_DAYS days"
    FORGEJO_API="$FORGEJO_BASE/api/v1/repos/$FORGEJO_REPO/releases"
    RELEASES_JSON="$(curl -fsSL -H "Authorization: token $FORGEJO_TOKEN" "$FORGEJO_API?limit=50" || echo '[]')"
    PRUNE_IDS="$(echo "$RELEASES_JSON" | python3 -c "
import json, sys
from datetime import datetime, timedelta, timezone
days = int('${FORGEJO_PRUNE_DAYS}')
cutoff = datetime.now(timezone.utc) - timedelta(days=days)
for r in json.load(sys.stdin):
    created = datetime.fromisoformat(r['created_at'].replace('Z', '+00:00'))
    if created < cutoff:
        print(f\"{r['id']}\t{r['tag_name']}\t{r['created_at']}\")
" 2>/dev/null || true)"
    DELETED=0
    FAILED=0
    if [ -n "$PRUNE_IDS" ]; then
      while IFS=$'\t' read -r REL_ID REL_TAG REL_CREATED; do
        [ -z "$REL_ID" ] && continue
        CODE="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: token $FORGEJO_TOKEN" "$FORGEJO_API/$REL_ID")"
        if [ "$CODE" = "204" ]; then
          echo "    deleted $REL_TAG (created $REL_CREATED)"
          DELETED=$((DELETED + 1))
        else
          warn "    failed to delete $REL_TAG (id=$REL_ID, http=$CODE)"
          FAILED=$((FAILED + 1))
        fi
      done <<< "$PRUNE_IDS"
    fi
    if [ "$FAILED" -gt 0 ]; then
      warn "Forgejo prune: $DELETED removed, $FAILED failed"
    else
      ok "Forgejo prune: $DELETED release(s) removed (>${FORGEJO_PRUNE_DAYS} days old)"
    fi
  fi
fi

# 7. Optional push
if [ "$PUSH_TAG" = true ]; then
  info "git push origin main --follow-tags"
  git push origin main --follow-tags
  ok "tag $TAG pushed to GitHub"
fi

echo ""
ok "${BOLD}$TAG shipped${NC}"
