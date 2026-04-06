#!/usr/bin/env bash
#
# release.sh — Automate version bump, changelog generation, and tag creation.
#
# Usage:
#   ./scripts/release.sh patch|minor|major    Auto-bump from latest tag
#   ./scripts/release.sh 0.5.0               Explicit version
#   ./scripts/release.sh --dry-run patch      Preview without changes
#
set -euo pipefail

VERSION_FILE="internal/cmd/version.go"
CHANGELOG_FILE="CHANGELOG.md"

# ── Colors ──────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${CYAN}▸${NC} $*"; }
ok()    { echo -e "${GREEN}✓${NC} $*"; }
warn()  { echo -e "${YELLOW}⚠${NC} $*"; }
error() { echo -e "${RED}✗${NC} $*" >&2; }
die()   { error "$@"; exit 1; }

# ── Args ────────────────────────────────────────────────────────────
DRY_RUN=false
BUMP=""

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    patch|minor|major) BUMP="$arg" ;;
    [0-9]*) BUMP="$arg" ;;
    -h|--help)
      echo "Usage: $0 [--dry-run] <patch|minor|major|X.Y.Z>"
      exit 0
      ;;
    *) die "Unknown argument: $arg" ;;
  esac
done

[ -z "$BUMP" ] && die "Usage: $0 [--dry-run] <patch|minor|major|X.Y.Z>"

# ── Prerequisites ───────────────────────────────────────────────────
command -v git-cliff >/dev/null 2>&1 || die "git-cliff not found. Install: https://git-cliff.org/docs/installation"

if [ "$DRY_RUN" = false ]; then
  [ -n "$(git status --porcelain)" ] && die "Working tree is dirty. Commit or stash changes first."
fi

CURRENT_BRANCH=$(git branch --show-current)
[ "$CURRENT_BRANCH" = "main" ] || warn "Not on main branch (current: $CURRENT_BRANCH)"

# ── Resolve version ────────────────────────────────────────────────
LATEST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
LATEST_VERSION="${LATEST_TAG#v}"

bump_version() {
  local version="$1" part="$2"
  IFS='.' read -r major minor patch <<< "$version"
  case "$part" in
    major) echo "$((major + 1)).0.0" ;;
    minor) echo "$major.$((minor + 1)).0" ;;
    patch) echo "$major.$minor.$((patch + 1))" ;;
  esac
}

case "$BUMP" in
  patch|minor|major) NEXT_VERSION=$(bump_version "$LATEST_VERSION" "$BUMP") ;;
  *)                 NEXT_VERSION="$BUMP" ;;
esac

NEXT_TAG="v${NEXT_VERSION}"

echo ""
echo -e "${BOLD}  Release Plan${NC}"
echo -e "  ─────────────────────────────"
echo -e "  Current tag:  ${YELLOW}${LATEST_TAG}${NC}"
echo -e "  Next version: ${GREEN}${NEXT_TAG}${NC}"
echo -e "  Dry run:      ${DRY_RUN}"
echo ""

# ── Preview changelog ───────────────────────────────────────────────
info "Generating changelog for ${NEXT_TAG}..."
CHANGELOG_PREVIEW=$(git-cliff --tag "$NEXT_TAG" --unreleased --strip header)

if [ -z "$CHANGELOG_PREVIEW" ]; then
  die "No conventional commits found since ${LATEST_TAG}. Nothing to release."
fi

echo -e "${BOLD}  Changes in ${NEXT_TAG}:${NC}"
echo "$CHANGELOG_PREVIEW" | sed 's/^/  /'
echo ""

# ── Dry run stops here ─────────────────────────────────────────────
if [ "$DRY_RUN" = true ]; then
  ok "Dry run complete. No changes made."
  exit 0
fi

# ── Confirm ─────────────────────────────────────────────────────────
echo -ne "${YELLOW}Proceed with release ${NEXT_TAG}? [y/N]${NC} "
read -r CONFIRM
[[ "$CONFIRM" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }

# ── Update version.go ──────────────────────────────────────────────
info "Updating ${VERSION_FILE}..."
sed -i "s/var Version = \".*\"/var Version = \"${NEXT_VERSION}\"/" "$VERSION_FILE"
ok "Version set to ${NEXT_VERSION}"

# ── Update CHANGELOG.md ────────────────────────────────────────────
info "Updating ${CHANGELOG_FILE}..."
git-cliff --tag "$NEXT_TAG" --output "$CHANGELOG_FILE"
ok "Changelog updated"

# ── Commit and tag ──────────────────────────────────────────────────
info "Creating release commit..."
git add "$VERSION_FILE" "$CHANGELOG_FILE"
git commit -m "chore(release): ${NEXT_VERSION}

- Bump version to ${NEXT_VERSION}
- Update CHANGELOG.md"

info "Creating annotated tag ${NEXT_TAG}..."
git tag -a "$NEXT_TAG" -m "Release ${NEXT_TAG}"

echo ""
ok "Release ${NEXT_TAG} created successfully!"
echo ""
echo -e "  ${BOLD}Next steps:${NC}"
echo -e "  Push to trigger CI release pipeline:"
echo ""
echo -e "    ${CYAN}git push origin main --follow-tags${NC}"
echo ""
