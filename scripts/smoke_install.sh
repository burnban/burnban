#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
RELEASE="$TMP/release"
HOME_DIR="$TMP/home"
BIN_DIR="$HOME_DIR/.local/bin"
mkdir -p "$RELEASE" "$HOME_DIR/Desktop"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported smoke-test architecture" >&2; exit 1 ;;
esac
ARCHIVE="burnban_${OS}_${ARCH}.tar.gz"

(cd "$ROOT" && CGO_ENABLED=0 GOCACHE="${GOCACHE:-$TMP/gocache}" GOTMPDIR="$TMP" go build -trimpath -o "$RELEASE/burnban" .)
tar -czf "$RELEASE/$ARCHIVE" -C "$RELEASE" burnban
if command -v sha256sum >/dev/null 2>&1; then
  HASH=$(sha256sum "$RELEASE/$ARCHIVE" | awk '{print $1}')
else
  HASH=$(shasum -a 256 "$RELEASE/$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "$ARCHIVE" > "$RELEASE/checksums.txt"

HOME="$HOME_DIR" SHELL=/bin/zsh BIN_DIR="$BIN_DIR" \
  BURNBAN_DOWNLOAD_BASE_URL="$RELEASE" BURNBAN_CREATE_DESKTOP=1 \
  sh "$ROOT/install.sh"

test -x "$BIN_DIR/burnban"
"$BIN_DIR/burnban" version | grep -q '^burnban '
case "$OS" in
  darwin)
    test -x "$HOME_DIR/Applications/Burnban.app/Contents/MacOS/burnban-launcher"
    test -f "$HOME_DIR/Applications/Burnban.app/Contents/Info.plist"
    test -L "$HOME_DIR/Desktop/Burnban.app"
    ;;
  linux)
    test -x "$HOME_DIR/.local/share/applications/burnban.desktop"
    test -x "$HOME_DIR/Desktop/Burnban.desktop"
    grep -Fq "$BIN_DIR/burnban" "$HOME_DIR/.local/share/applications/burnban.desktop"
    ;;
esac

HOME="$HOME_DIR" BIN_DIR="$BIN_DIR" sh "$ROOT/install.sh" --uninstall
test ! -e "$BIN_DIR/burnban"
echo "installer smoke test passed for $OS/$ARCH"
