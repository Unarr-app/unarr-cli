#!/usr/bin/env bash
# scripts/download-ffmpeg-static.sh — fetch static ffmpeg + ffprobe binaries
# for every platform we ship. Run by goreleaser's `before.hooks` so each
# tarball can bundle the binaries adjacent to `unarr`.
#
# Output:
#   dist-ffbinaries/<goos>-<goarch>/{ffmpeg, ffprobe}[.exe]
# Idempotent: skips downloads when the target file already exists (the release
# workflow caches dist-ffbinaries, so a good download is never repeated).
#
# Resilience: every binary has an ordered list of sources. Each source is tried
# with exponential backoff that honours Retry-After; a download only counts once
# the archive unpacks to a plausibly-sized file, and a failed source falls
# through to the next one. A single third-party host rate-limiting the CI runner
# (evermeet.cx returned 429 for three releases of v1.18.1) can no longer block a
# release. Source order, per platform:
#   linux-amd64/arm64, darwin-amd64, windows-amd64:
#       ffbinaries (GitHub release) -> eugeneware/ffmpeg-static (GitHub release)
#   darwin-arm64:
#       eugeneware (native arm64) -> evermeet.cx (x86_64, runs under Rosetta)
#   windows-arm64: BtbN/FFmpeg-Builds (only source with a windows-arm64 build)
#
# Env: FFBINARIES_DEST (output dir), FFBIN_BACKOFF_BASE (seconds, default 5;
# 0 disables sleeping — used by tests).

set -euo pipefail

DEST_ROOT="${FFBINARIES_DEST:-dist-ffbinaries}"
INDEX_URL="https://ffbinaries.com/api/v1/version/latest"
# Used when the ffbinaries index is unreachable: same GitHub release assets the
# index points at, pinned to the last version we verified.
FFB_PINNED_VERSION="6.1"
FFB_BASE="https://github.com/ffbinaries/ffbinaries-prebuilt/releases/download/v${FFB_PINNED_VERSION}"
EU_BASE="https://github.com/eugeneware/ffmpeg-static/releases/download/b6.1.1"
BTBN_WINARM="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-winarm64-gpl.zip"
BACKOFF_BASE="${FFBIN_BACKOFF_BASE:-5}"
MAX_ATTEMPTS=4
MIN_BYTES=1000000 # a real ffmpeg/ffprobe is tens of MB; smaller = error page

for cmd in curl jq unzip gunzip; do
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "[ffbin] missing required tool: $cmd" >&2
    exit 2
  }
done

mkdir -p "$DEST_ROOT"
FAILED=0
ARCHIVES="$(mktemp -d)"
trap 'rm -rf "$ARCHIVES"' EXIT

# download URL OUT — GET with retries on any non-200 (429/5xx/network), waiting
# BACKOFF_BASE * 3^(n-1) seconds or the server's Retry-After (capped at 120).
download() {
  local url="$1" out="$2" attempt=1 code hdr ra wait
  hdr="$(mktemp)"
  while :; do
    code="$(curl -sSL --connect-timeout 15 --max-time 300 -D "$hdr" -o "$out" \
      -w '%{http_code}' "$url" 2>/dev/null)" || code=000
    if [ "$code" = 200 ]; then
      rm -f "$hdr"
      return 0
    fi
    if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
      break
    fi
    wait=$((BACKOFF_BASE * 3 ** (attempt - 1)))
    ra="$(awk 'tolower($1)=="retry-after:"{gsub(/\r/,"",$2); v=$2} END{print v}' "$hdr")"
    case "$ra" in
      '' | *[!0-9]*) ;;
      *) [ "$ra" -le 120 ] && [ "$BACKOFF_BASE" -gt 0 ] && wait="$ra" ;;
    esac
    echo "[ffbin] HTTP $code from $url (attempt $attempt/$MAX_ATTEMPTS), retry in ${wait}s" >&2
    sleep "$wait"
    attempt=$((attempt + 1))
  done
  echo "[ffbin] HTTP $code from $url, giving up on this source" >&2
  rm -f "$hdr" "$out"
  return 1
}

# extract KIND ARCHIVE MEMBER DEST — unpack one binary to DEST atomically.
# zip: MEMBER is an unzip pattern (so "*/bin/ffmpeg.exe" works); gz: a single file.
extract() {
  local kind="$1" file="$2" member="$3" dest="$4" part="$4.part" m
  case "$kind" in
    zip)
      m="$(unzip -Z1 "$file" "$member" 2>/dev/null | head -1)"
      [ -n "$m" ] || return 1
      unzip -p "$file" "$m" >"$part" || return 1
      ;;
    gz) gunzip -c "$file" >"$part" || return 1 ;;
    *) return 1 ;;
  esac
  if [ "$(wc -c <"$part")" -lt "$MIN_BYTES" ]; then
    rm -f "$part"
    return 1
  fi
  chmod +x "$part"
  mv "$part" "$dest"
}

# fetch_tool DEST MEMBER SOURCE... — SOURCE is "kind|url"; first one that works wins.
fetch_tool() {
  local dest="$1" member="$2" src tmp
  shift 2
  if [ -f "$dest" ]; then
    echo "[ffbin] skip $dest (already present)"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  for src in "$@"; do
    echo "[ffbin] fetch $dest from ${src#*|}"
    # One archive can hold several binaries (BtbN): download each URL once.
    tmp="$ARCHIVES/$(printf '%s' "${src#*|}" | cksum | tr -d ' ')"
    if { [ -s "$tmp" ] || download "${src#*|}" "$tmp"; } && extract "${src%%|*}" "$tmp" "$member" "$dest"; then
      return 0
    fi
    rm -f "$tmp" "$dest.part"
    echo "[ffbin] WARN source failed for $dest, trying the next one" >&2
  done
  echo "[ffbin] ERROR every source failed for $dest" >&2
  FAILED=1
  return 0
}

echo "[ffbin] fetching index from $INDEX_URL"
INDEX_JSON=""
idx="$(mktemp)"
if download "$INDEX_URL" "$idx" && jq -e .bin "$idx" >/dev/null 2>&1; then
  INDEX_JSON="$(cat "$idx")"
  echo "[ffbin] ffbinaries version: $(jq -r .version "$idx")"
else
  echo "[ffbin] WARN ffbinaries index unavailable; using pinned v${FFB_PINNED_VERSION} URLs" >&2
fi
rm -f "$idx"

# ffb_url KEY TOOL SUFFIX — index URL, else the pinned release asset.
ffb_url() {
  local url=""
  if [ -n "$INDEX_JSON" ]; then
    url="$(jq -r ".bin[\"$1\"][\"$2\"] // empty" <<<"$INDEX_JSON")"
  fi
  echo "${url:-$FFB_BASE/$2-$FFB_PINNED_VERSION-$3.zip}"
}

# "ffbinaries key : goreleaser os-arch : ffbinaries asset suffix : eugeneware os-arch"
PLATFORMS=(
  "linux-64:linux-amd64:linux-64:linux-x64"
  "linux-arm64:linux-arm64:linux-arm-64:linux-arm64"
  "osx-64:darwin-amd64:macos-64:darwin-x64"
  "windows-64:windows-amd64:win-64:win32-x64"
)
for entry in "${PLATFORMS[@]}"; do
  IFS=: read -r ffbkey goplat suffix euplat <<<"$entry"
  for tool in ffmpeg ffprobe; do
    bin="$tool"
    [[ "$goplat" == windows-* ]] && bin="${tool}.exe"
    fetch_tool "$DEST_ROOT/$goplat/$bin" "$bin" \
      "zip|$(ffb_url "$ffbkey" "$tool" "$suffix")" \
      "gz|$EU_BASE/$tool-$euplat.gz"
  done
done

# darwin-arm64: ffbinaries has no build. eugeneware ships a native arm64 one;
# evermeet.cx (x86_64, Rosetta) stays as the fallback — it is the host that
# rate-limits CI, so it is no longer first.
for tool in ffmpeg ffprobe; do
  fetch_tool "$DEST_ROOT/darwin-arm64/$tool" "$tool" \
    "gz|$EU_BASE/$tool-darwin-arm64.gz" \
    "zip|https://evermeet.cx/ffmpeg/getrelease/$tool/zip"
done

# windows-arm64: BtbN ships one zip with both .exe under */bin/.
for tool in ffmpeg ffprobe; do
  fetch_tool "$DEST_ROOT/windows-arm64/$tool.exe" "*/bin/$tool.exe" "zip|$BTBN_WINARM"
done

echo "[ffbin] layout:"
find "$DEST_ROOT" -type f -printf "  %p (%s bytes)\n"

if [ "$FAILED" -ne 0 ]; then
  echo "[ffbin] FAILED: at least one binary could not be fetched from any source" >&2
  exit 1
fi
echo "[ffbin] done."
