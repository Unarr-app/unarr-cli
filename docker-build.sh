#!/bin/sh
# Build the unarr Docker image.
# Must be run from the unarr directory (or its parent).
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PARENT_DIR="$(dirname "$SCRIPT_DIR")"

# Build from parent dir so both unarr/ and go-client/ are in context
docker build \
    -f "$SCRIPT_DIR/Dockerfile" \
    -t unarr/cli:latest \
    "$PARENT_DIR"

echo ""
echo "✓ Built: unarr/cli:latest"
docker images unarr/cli:latest --format "  Size: {{.Size}}"
