#!/usr/bin/env bash
# Run the full Aplexica test suite inside a Linux container so the
# linux-only build paths (notably the inotify Source) are exercised on
# a non-Linux dev host. Requires Docker (Docker Desktop on macOS works).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

IMAGE_TAG="aplexica-linux-test"

echo "Building Linux test image..."
docker build -f docker/Dockerfile.linux -t "$IMAGE_TAG" .

echo "Running tests in container (race-enabled)..."
docker run --rm \
    -v "$REPO_ROOT":/src:rw \
    -w /src \
    "$IMAGE_TAG"

echo "All Linux tests passed."
