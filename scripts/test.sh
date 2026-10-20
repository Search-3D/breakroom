#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Running tests..."
if [[ $# -eq 0 ]]; then
  go test -race -timeout 3m ./...
else
  go test -race -timeout 3m "$@"
fi
echo "==> All tests passed!"
