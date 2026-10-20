#!/usr/bin/env bash
set -euo pipefail

# Find repo root relative to this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

CHECK_ONLY=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_ONLY=true
fi

# Canonical source files at repo root
CANONICAL_SKILL="$REPO_ROOT/SKILL.md"
CANONICAL_TOOL_CONTRACTS="$REPO_ROOT/docs/tool-contracts.md"
CANONICAL_BLUEPRINT_SPEC="$REPO_ROOT/docs/blueprint-spec.md"

for src in "$CANONICAL_SKILL" "$CANONICAL_TOOL_CONTRACTS" "$CANONICAL_BLUEPRINT_SPEC"; do
  if [[ ! -f "$src" ]]; then
    echo "Error: Canonical file not found: $src" >&2
    exit 1
  fi
done

# Known target directories for skill copies
TARGET_DIRS=(
  "$REPO_ROOT/integrations/antigravity/breakroom-plugin/skills/breakroom"
  "$REPO_ROOT/integrations/claude/breakroom-plugin/skills/breakroom"
  "$REPO_ROOT/integrations/codex/breakroom-plugin/skills/breakroom"
  "$REPO_ROOT/integrations/cursor/breakroom-plugin/skills/breakroom"
  "$REPO_ROOT/integrations/muse/breakroom-plugin/skills/breakroom"
  "$REPO_ROOT/integrations/vscode/breakroom-plugin/skills/breakroom"
)

# Also discover any additional skill directories under integrations/
while IFS= read -r extra_dir; do
  [[ -z "$extra_dir" ]] && continue
  found=false
  for existing in "${TARGET_DIRS[@]}"; do
    if [[ "$existing" == "$extra_dir" ]]; then
      found=true
      break
    fi
  done
  if [[ "$found" == "false" ]]; then
    TARGET_DIRS+=("$extra_dir")
  fi
done < <(find "$REPO_ROOT/integrations" -type d -path "*/skills/breakroom" 2>/dev/null || true)

DRIFT_COUNT=0
SYNCED_COUNT=0

for target in "${TARGET_DIRS[@]}"; do
  # Each target contains SKILL.md and docs/{tool-contracts.md, blueprint-spec.md}
  files=(
    "$CANONICAL_SKILL:$target/SKILL.md"
    "$CANONICAL_TOOL_CONTRACTS:$target/docs/tool-contracts.md"
    "$CANONICAL_BLUEPRINT_SPEC:$target/docs/blueprint-spec.md"
  )

  for pair in "${files[@]}"; do
    src="${pair%%:*}"
    dst="${pair##*:}"

    if [[ "$CHECK_ONLY" == "true" ]]; then
      if [[ ! -f "$dst" ]] || ! cmp -s "$src" "$dst"; then
        rel_dst="${dst#"$REPO_ROOT/"}"
        echo "Drift detected: $rel_dst does not match source"
        DRIFT_COUNT=$((DRIFT_COUNT + 1))
      fi
    else
      mkdir -p "$(dirname "$dst")"
      if [[ ! -f "$dst" ]] || ! cmp -s "$src" "$dst"; then
        cp "$src" "$dst"
        rel_dst="${dst#"$REPO_ROOT/"}"
        echo "Synced: $rel_dst"
        SYNCED_COUNT=$((SYNCED_COUNT + 1))
      fi
    fi
  done
done

if [[ "$CHECK_ONLY" == "true" ]]; then
  if [[ $DRIFT_COUNT -gt 0 ]]; then
    echo "Check failed: $DRIFT_COUNT drifted file(s) found." >&2
    exit 1
  fi
  echo "All skill copies are in sync."
  exit 0
fi

if [[ $SYNCED_COUNT -eq 0 ]]; then
  echo "All skill copies are already up to date."
else
  echo "Successfully synced $SYNCED_COUNT file(s)."
fi
