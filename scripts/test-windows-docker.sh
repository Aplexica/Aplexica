#!/usr/bin/env bash
# Run the Aplexica test suite inside a Windows container so the windows-only
# build paths (notably ReadDirectoryChangesW Source) are exercised.
#
# IMPORTANT: Windows containers require a Windows host. On macOS or Linux
# Docker Desktop, this script will fail with "no matching manifest" or
# "image platform mismatch" — Windows containers cannot run on a Linux
# kernel. For local validation on non-Windows hosts, use the GitHub Actions
# workflow at .github/workflows/test.yml (windows-latest matrix entry).
set -euo pipefail

if [[ "$(uname -s)" != "Windows_NT" && "$(uname -s)" != *"MINGW"* && "$(uname -s)" != *"CYGWIN"* ]]; then
    echo "WARNING: This script requires a Windows host."
    echo "On macOS / Linux, Windows containers won't run. Use the GitHub Actions"
    echo "workflow (.github/workflows/test.yml) for CI-based Windows test runs."
    echo ""
    echo "Attempting docker build anyway in case Docker is configured for"
    echo "Windows containers via a remote daemon..."
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

IMAGE_TAG="aplexica-windows-test"

echo "Building Windows test image..."
docker build -f docker/Dockerfile.windows -t "$IMAGE_TAG" .

echo "Running tests in Windows container (race-enabled)..."
docker run --rm \
    -v "$REPO_ROOT":C:/src \
    -w C:/src \
    "$IMAGE_TAG"

echo "All Windows tests passed."
