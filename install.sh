#!/bin/sh
# burnban installer — https://burnban.dev
set -e

REPO="syft8/burnban"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux | darwin) ;;
  *) echo "burnban: unsupported operating system: $OS" >&2; exit 1 ;;
esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "burnban: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

ARCHIVE="burnban_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/$REPO/releases/latest/download"
URL="$BASE_URL/$ARCHIVE"
CHECKSUMS_URL="$BASE_URL/checksums.txt"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

echo "🔥 installing burnban → $BIN_DIR/burnban"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/burnban.tgz"
curl -fsSL "$CHECKSUMS_URL" -o "$TMP/checksums.txt"
EXPECTED=$(awk -v file="$ARCHIVE" '$2 == file { print $1; exit }' "$TMP/checksums.txt")
if [ -z "$EXPECTED" ]; then
  echo "burnban: $ARCHIVE is missing from release checksums" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP/burnban.tgz" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "$TMP/burnban.tgz" | awk '{print $1}')
else
  echo "burnban: sha256sum or shasum is required to verify the download" >&2
  exit 1
fi
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "burnban: checksum verification failed for $ARCHIVE" >&2
  exit 1
fi
tar -xzf "$TMP/burnban.tgz" -C "$TMP" burnban
install "$TMP/burnban" "$BIN_DIR/burnban" 2>/dev/null || sudo install "$TMP/burnban" "$BIN_DIR/burnban"

echo "✅ installed: $("$BIN_DIR/burnban" version)"
echo "   try it:    burnban demo"
echo "   real use:  burnban serve"
