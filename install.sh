#!/usr/bin/env bash
set -euo pipefail

# Breakroom installer script
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Search-3D/breakroom/main/install.sh | bash

REPO="Search-3D/breakroom"
INSTALL_DIR="${BREAKROOM_INSTALL_DIR:-/usr/local/bin}"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Error: required command '$1' not found on PATH." >&2
    exit 1
  fi
}

need_cmd curl
need_cmd tar
need_cmd uname

# Detect Operating System
OS="$(uname -s)"
case "$OS" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux" ;;
  *)
    echo "Error: Unsupported operating system: $OS" >&2
    echo "Breakroom currently supports macOS and Linux (and Windows via WSL)." >&2
    exit 1
    ;;
esac

# Detect Architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Error: Unsupported architecture: $ARCH" >&2
    echo "Breakroom currently supports amd64 and arm64." >&2
    exit 1
    ;;
esac

# Step 1: Determine release version
if [ -n "${BREAKROOM_VERSION:-}" ]; then
  VERSION="${BREAKROOM_VERSION#v}"
  TAG="v${VERSION}"
else
  # Resolve latest tag via GitHub redirect or releases API with timeouts
  TAG=""
  EFFECTIVE_URL=$(curl -fsSLI -o /dev/null -w "%{url_effective}" --connect-timeout 5 --max-time 10 "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)
  case "$EFFECTIVE_URL" in
    */releases/tag/*)
      TAG="${EFFECTIVE_URL##*/tag/}"
      TAG="${TAG%%\?*}"
      TAG="${TAG%%/}"
      ;;
    *)
      TAG=""
      ;;
  esac

  if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL --connect-timeout 5 --max-time 10 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -n1 | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/' || true)
  fi

  if [ -z "$TAG" ]; then
    echo "====================================================================" >&2
    echo "Unable to locate a release for ${REPO} on GitHub." >&2
    echo "====================================================================" >&2
    exit 1
  fi
  VERSION="${TAG#v}"
fi

TARBALL="breakroom_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="${BREAKROOM_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download/${TAG}/${TARBALL}}"
CHECKSUMS_URL="${BREAKROOM_CHECKSUMS_URL:-https://github.com/${REPO}/releases/download/${TAG}/checksums.txt}"

# Step 2: Extract to a temporary directory
TMP_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t 'breakroom-install')
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

if [ -n "${BREAKROOM_TARBALL_PATH:-}" ]; then
  echo "==> Using local archive: $BREAKROOM_TARBALL_PATH"
  cp "$BREAKROOM_TARBALL_PATH" "$TMP_DIR/$TARBALL"
else
  echo "==> Downloading Breakroom ${TAG} for ${OS}/${ARCH}..."
  if ! curl -fsSL --connect-timeout 10 --max-time 120 "$DOWNLOAD_URL" -o "$TMP_DIR/$TARBALL"; then
    echo "" >&2
    echo "Error: Failed to download release asset from:" >&2
    echo "  $DOWNLOAD_URL" >&2
    echo "" >&2
    echo "The release '${TAG}' is referenced, but the archive for ${OS}/${ARCH} could not be downloaded." >&2
    echo "You can check available releases at: https://github.com/${REPO}/releases" >&2
    exit 1
  fi

  # A local archive is the user's own choice; a download must match the
  # release's checksums.txt before anything runs with sudo.
  echo "==> Verifying checksum..."
  if ! curl -fsSL --connect-timeout 10 --max-time 30 "$CHECKSUMS_URL" -o "$TMP_DIR/checksums.txt"; then
    echo "Error: Failed to download checksums from $CHECKSUMS_URL" >&2
    exit 1
  fi
  WANT=$(awk -v f="$TARBALL" '$2 == f || $2 == "*" f { print $1 }' "$TMP_DIR/checksums.txt")
  if command -v sha256sum >/dev/null 2>&1; then
    GOT=$(sha256sum "$TMP_DIR/$TARBALL" | awk '{ print $1 }')
  elif command -v shasum >/dev/null 2>&1; then
    GOT=$(shasum -a 256 "$TMP_DIR/$TARBALL" | awk '{ print $1 }')
  else
    echo "Error: need sha256sum or shasum to verify the download." >&2
    exit 1
  fi
  if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
    echo "Error: Checksum mismatch for $TARBALL (expected '${WANT:-none listed}', got '$GOT')." >&2
    exit 1
  fi
fi

echo "==> Extracting archive..."
if ! tar -xzf "$TMP_DIR/$TARBALL" -C "$TMP_DIR"; then
  echo "Error: Failed to extract archive." >&2
  exit 1
fi

if [ ! -f "$TMP_DIR/breakroom" ]; then
  echo "Error: Extracted archive did not contain 'breakroom' binary." >&2
  exit 1
fi

# Step 3: Install to /usr/local/bin with sudo
echo ""

if [ "$INSTALL_DIR" = "/usr/local/bin" ]; then
  echo "Now we need your permission to install breakroom in /usr/local/bin (sudo access required)"
  if [ -r /dev/tty ]; then
    sudo mkdir -p /usr/local/bin </dev/tty
    sudo mv "$TMP_DIR/breakroom" /usr/local/bin/breakroom </dev/tty
    sudo chmod +x /usr/local/bin/breakroom </dev/tty

    if [ "$OS" = "darwin" ]; then
      echo "Disabling first launch quarantine for /usr/local/bin/breakroom (sudo access required)"
      sudo /usr/bin/xattr -dr com.apple.quarantine /usr/local/bin/breakroom 2>/dev/null </dev/tty || true
    fi
  else
    sudo mkdir -p /usr/local/bin
    sudo mv "$TMP_DIR/breakroom" /usr/local/bin/breakroom
    sudo chmod +x /usr/local/bin/breakroom

    if [ "$OS" = "darwin" ]; then
      echo "Disabling first launch quarantine for /usr/local/bin/breakroom"
      sudo /usr/bin/xattr -dr com.apple.quarantine /usr/local/bin/breakroom 2>/dev/null || true
    fi
  fi
else
  # Non-default install dir (e.g. testing)
  mkdir -p "$INSTALL_DIR"
  mv "$TMP_DIR/breakroom" "$INSTALL_DIR/breakroom"
  chmod +x "$INSTALL_DIR/breakroom"
  if [ "$OS" = "darwin" ]; then
    echo "Disabling first launch quarantine for $INSTALL_DIR/breakroom"
    /usr/bin/xattr -dr com.apple.quarantine "$INSTALL_DIR/breakroom" 2>/dev/null || true
  fi
fi

# Step 4: Remove temp dir
rm -rf "$TMP_DIR"
trap - EXIT INT TERM

# Step 5: Run breakroom doctor
echo ""
echo "==> Running breakroom doctor..."
if [ -x "$INSTALL_DIR/breakroom" ]; then
  "$INSTALL_DIR/breakroom" doctor || true
elif command -v breakroom >/dev/null 2>&1; then
  breakroom doctor || true
fi

# Verify PATH
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo ""
    echo "Note: $INSTALL_DIR is not currently in your PATH."
    echo "You may want to add it to your shell configuration:"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

# Step 6: Inform users and show breakroom install
echo ""
echo "===================================================================="
echo "🎉 Breakroom is now installed in $INSTALL_DIR/breakroom"
echo "===================================================================="
echo ""
echo "Next, run 'breakroom install' to configure your agent host:"
echo ""
echo "  breakroom install"
echo ""
echo "Or specify your host directly:"
echo "  breakroom install --host claude"
echo "  breakroom install --host antigravity"
echo "  breakroom install --host cursor"
echo "  breakroom install --host codex"
echo "  breakroom install --host vscode"
echo "  breakroom install --host muse"
echo "  breakroom install --host opencode"
echo ""
