#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "============================================="
echo " Stage 1/4: Linting & Sanity Checks"
echo "============================================="
"$SCRIPT_DIR/lint.sh"

echo ""
echo "============================================="
echo " Stage 2/4: Running Test Suite"
echo "============================================="
"$SCRIPT_DIR/test.sh"

echo ""
echo "============================================="
echo " Stage 3/4: Cleaning build caches / artifacts"
echo "============================================="
"$SCRIPT_DIR/clean.sh"

echo ""
echo "============================================="
echo " Stage 4/4: Building Binary"
echo "============================================="
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")}"
echo "Building breakroom version: $VERSION"

go build -ldflags "-s -w -X github.com/search-3d/breakroom/internal/version.Version=$VERSION" -o breakroom ./cmd/breakroom

echo "Verifying build..."
./breakroom version

echo ""
echo "==> Build successful: $REPO_ROOT/breakroom"
