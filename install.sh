#!/bin/sh
# burnban installer — https://burnban.dev
set -e

REPO="syft8/burnban"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "burnban: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

URL="https://github.com/$REPO/releases/latest/download/burnban_${OS}_${ARCH}.tar.gz"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

echo "🔥 installing burnban → $BIN_DIR/burnban"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/burnban.tgz"
tar -xzf "$TMP/burnban.tgz" -C "$TMP" burnban
install "$TMP/burnban" "$BIN_DIR/burnban" 2>/dev/null || sudo install "$TMP/burnban" "$BIN_DIR/burnban"

echo "✅ installed: $("$BIN_DIR/burnban" version)"
echo "   try it:    burnban demo"
echo "   real use:  burnban serve"
