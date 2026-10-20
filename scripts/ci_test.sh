#!/usr/bin/env bash
set -euo pipefail

echo "==> [1/2] Testing macOS build"
act push -W .github/workflows/ci.yml --matrix os:macos-latest -P macos-latest=-self-hosted

echo ""
echo "==> [2/2] Testing Ubuntu build"
act push -W .github/workflows/ci.yml --matrix os:ubuntu-latest
