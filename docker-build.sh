#!/bin/sh
# Build the unarr Docker image.
# Must be run from the unarr directory (or its parent).
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PARENT_DIR="$(dirname "$SCRIPT_DIR")"

# Build from parent dir so both unarr/ and go-client/ are in context
docker build \
    -f "$SCRIPT_DIR/Dockerfile" \
    -t torrentclaw/unarr:latest \
    "$PARENT_DIR"

echo ""
echo "✓ Built: torrentclaw/unarr:latest"
docker images torrentclaw/unarr:latest --format "  Size: {{.Size}}"
