#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "==> [1/4] Checking skill & doc copy drift..."
"$SCRIPT_DIR/sync-skills.sh" --check

echo "==> [2/4] Checking gofmt..."
UNFORMATTED=$(gofmt -l .)
if [[ -n "$UNFORMATTED" ]]; then
  echo "Error: Unformatted Go files found:" >&2
  echo "$UNFORMATTED" >&2
  echo "Run ./scripts/fmt.sh to fix automatically." >&2
  exit 1
fi
echo "gofmt passed."

echo "==> [3/4] Running go vet..."
go vet ./...
echo "go vet passed."

echo "==> [4/4] Checking linters (golangci-lint)..."
GOLANGCI_BIN=""
if command -v golangci-lint >/dev/null 2>&1; then
  GOLANGCI_BIN="golangci-lint"
elif [[ -x "$(go env GOPATH 2>/dev/null)/bin/golangci-lint" ]]; then
  GOLANGCI_BIN="$(go env GOPATH)/bin/golangci-lint"
fi

if [[ -n "$GOLANGCI_BIN" ]]; then
  "$GOLANGCI_BIN" run ./...
  echo "golangci-lint passed."
else
  echo "Note: golangci-lint not found in PATH or GOPATH/bin; skipping."
fi

echo "==> All lint checks passed!"
