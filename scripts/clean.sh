#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "==> [1/2] Cleaning build artifacts..."
rm -f breakroom
rm -rf dist/
rm -rf bench/sidecar/out* bench/sidecar/run*log

echo "==> [2/2] Cleaning Go test cache..."
go clean -testcache

echo "==> Workspace cleaned."
