#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Formatting Go files with gofmt..."
gofmt -w .
echo "gofmt complete."

echo "==> Syncing skill and doc copies..."
"$SCRIPT_DIR/sync-skills.sh"

echo "==> Done formatting and syncing!"
