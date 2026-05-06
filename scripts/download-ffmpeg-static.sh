#!/usr/bin/env bash
# scripts/download-ffmpeg-static.sh — fetch static ffmpeg + ffprobe binaries
# for every platform we ship. Run by goreleaser's `before.hooks` so each
# tarball can bundle the binaries adjacent to `unarr`.
#
# Source: https://ffbinaries.com (same index the runtime fallback uses).
# Output:
#   dist-ffbinaries/<goos>-<goarch>/{ffmpeg, ffprobe}[.exe]
# Idempotent: skips downloads when the target file already exists.

set -euo pipefail

# Map ffbinaries platform key → goreleaser {Os}-{Arch}. ffbinaries.com only
# ships an x86_64 macOS build; for darwin-arm64 we fall back to evermeet.cx
# universal binaries (handled separately below).
PLATFORMS=(
  "linux-64:linux-amd64"
  "linux-arm64:linux-arm64"
  "osx-64:darwin-amd64"
  "windows-64:windows-amd64"
)
DEST_ROOT="${FFBINARIES_DEST:-dist-ffbinaries}"
INDEX_URL="https://ffbinaries.com/api/v1/version/latest"

for cmd in curl jq unzip; do
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "[ffbin] missing required tool: $cmd" >&2
    exit 2
  }
done

mkdir -p "$DEST_ROOT"

echo "[ffbin] fetching index from $INDEX_URL"
INDEX_JSON="$(curl -fsSL "$INDEX_URL")"
VERSION="$(echo "$INDEX_JSON" | jq -r .version)"
echo "[ffbin] ffbinaries version: $VERSION"

for entry in "${PLATFORMS[@]}"; do
  ffbkey="${entry%%:*}"
  goplat="${entry##*:}"
  outdir="$DEST_ROOT/$goplat"
  mkdir -p "$outdir"

  for tool in ffmpeg ffprobe; do
    binname="$tool"
    [[ "$goplat" == windows-* ]] && binname="${tool}.exe"

    if [ -f "$outdir/$binname" ]; then
      echo "[ffbin] skip $goplat/$binname (already present)"
      continue
    fi

    url="$(echo "$INDEX_JSON" | jq -r ".bin[\"$ffbkey\"][\"$tool\"] // empty")"
    if [ -z "$url" ]; then
      echo "[ffbin] WARN $goplat/$tool: no download URL in index" >&2
      continue
    fi

    tmpzip="$(mktemp --suffix=.zip)"
    echo "[ffbin] fetch $goplat/$tool from $url"
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "$url" -o "$tmpzip"
    unzip -p "$tmpzip" "$binname" > "$outdir/$binname"
    chmod +x "$outdir/$binname"
    rm -f "$tmpzip"
  done
done

# --- darwin-arm64 via evermeet.cx (universal binary; ffbinaries lacks it) ---
darwin_arm_dir="$DEST_ROOT/darwin-arm64"
mkdir -p "$darwin_arm_dir"
for tool in ffmpeg ffprobe; do
  out="$darwin_arm_dir/$tool"
  if [ -f "$out" ]; then
    echo "[ffbin] skip darwin-arm64/$tool (already present)"
    continue
  fi
  url="https://evermeet.cx/ffmpeg/getrelease/$tool/zip"
  tmpzip="$(mktemp --suffix=.zip)"
  echo "[ffbin] fetch darwin-arm64/$tool from $url"
  curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "$url" -o "$tmpzip"
  unzip -p "$tmpzip" "$tool" > "$out"
  chmod +x "$out"
  rm -f "$tmpzip"
done

# --- windows-arm64 via BtbN/FFmpeg-Builds (ffbinaries lacks it) ---
# BtbN ships a single zip per platform with ffmpeg.exe + ffprobe.exe under
# ffmpeg-master-latest-winarm64-gpl/bin/. Extract both in one fetch.
win_arm_dir="$DEST_ROOT/windows-arm64"
mkdir -p "$win_arm_dir"
needs_win_arm=0
for tool in ffmpeg.exe ffprobe.exe; do
  [ -f "$win_arm_dir/$tool" ] || needs_win_arm=1
done
if [ "$needs_win_arm" = "1" ]; then
  url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-winarm64-gpl.zip"
  tmpzip="$(mktemp --suffix=.zip)"
  echo "[ffbin] fetch windows-arm64/{ffmpeg,ffprobe}.exe from $url"
  curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "$url" -o "$tmpzip"
  for tool in ffmpeg.exe ffprobe.exe; do
    out="$win_arm_dir/$tool"
    member="$(unzip -Z1 "$tmpzip" "*/bin/$tool" 2>/dev/null | head -1)"
    if [ -z "$member" ]; then
      echo "[ffbin] WARN windows-arm64/$tool: not found in BtbN zip" >&2
      continue
    fi
    unzip -p "$tmpzip" "$member" > "$out"
    chmod +x "$out"
  done
  rm -f "$tmpzip"
else
  echo "[ffbin] skip windows-arm64 (already present)"
fi

echo "[ffbin] done. layout:"
find "$DEST_ROOT" -type f -printf "  %p (%s bytes)\n"
