#!/bin/sh
# Burnban installer for macOS and Linux — https://burnban.dev
set -eu

REPO="burnban/burnban"
CREATE_DESKTOP="${BURNBAN_CREATE_DESKTOP:-1}"
UPDATE_PATH="${BURNBAN_UPDATE_PATH:-1}"
UNINSTALL=0

usage() {
  cat <<'EOF'
usage: install.sh [--no-desktop] [--no-path] [--bin-dir DIR] [--uninstall]

Installs the burnban CLI plus a one-click desktop launcher. Environment:
  BIN_DIR                       binary destination override
  BURNBAN_CREATE_DESKTOP=0      skip desktop/application launchers
  BURNBAN_UPDATE_PATH=0         do not update the shell PATH
  BURNBAN_DOWNLOAD_BASE_URL=... release download override (testing/mirrors)
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-desktop) CREATE_DESKTOP=0 ;;
    --no-path) UPDATE_PATH=0 ;;
    --bin-dir)
      shift
      [ "$#" -gt 0 ] || { echo "burnban: --bin-dir needs a directory" >&2; exit 2; }
      BIN_DIR=$1
      ;;
    --uninstall) UNINSTALL=1 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "burnban: unknown installer option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux|darwin) ;;
  *) echo "burnban: unsupported operating system: $OS" >&2; exit 1 ;;
esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "burnban: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

if [ -z "${BIN_DIR:-}" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    BIN_DIR=/usr/local/bin
  elif [ "$(id -u)" -eq 0 ]; then
    BIN_DIR=/usr/local/bin
  else
    BIN_DIR="$HOME/.local/bin"
  fi
fi
BIN_PATH="$BIN_DIR/burnban"

remove_desktop() {
  if [ "$OS" = darwin ]; then
    rm -rf "$HOME/Applications/Burnban.app"
    [ ! -L "$HOME/Desktop/Burnban.app" ] || rm -f "$HOME/Desktop/Burnban.app"
  else
    data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
    rm -f "$data_home/applications/burnban.desktop"
    desktop_dir="$HOME/Desktop"
    if command -v xdg-user-dir >/dev/null 2>&1; then
      desktop_dir=$(xdg-user-dir DESKTOP 2>/dev/null || printf '%s' "$desktop_dir")
    fi
    rm -f "$desktop_dir/Burnban.desktop"
  fi
}

if [ "$UNINSTALL" -eq 1 ]; then
  remove_desktop
  if [ -e "$BIN_PATH" ]; then
    if rm -f "$BIN_PATH" 2>/dev/null; then :
    elif command -v sudo >/dev/null 2>&1; then sudo rm -f "$BIN_PATH"
    else echo "burnban: cannot remove $BIN_PATH (permission denied)" >&2; exit 1
    fi
  fi
  echo "✅ burnban removed"
  exit 0
fi

ARCHIVE="burnban_${OS}_${ARCH}.tar.gz"
DEFAULT_BASE="https://github.com/$REPO/releases/latest/download"
BASE_URL="${BURNBAN_DOWNLOAD_BASE_URL:-$DEFAULT_BASE}"
CHECKSUMS="checksums.txt"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fetch() {
  name=$1
  destination=$2
  if [ -d "$BASE_URL" ]; then
    cp "$BASE_URL/$name" "$destination"
  else
    command -v curl >/dev/null 2>&1 || { echo "burnban: curl is required" >&2; exit 1; }
    curl -fsSL "${BASE_URL%/}/$name" -o "$destination"
  fi
}

echo "🔥 downloading burnban for $OS/$ARCH"
fetch "$ARCHIVE" "$TMP/burnban.tgz"
fetch "$CHECKSUMS" "$TMP/checksums.txt"
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
mkdir -p "$BIN_DIR" 2>/dev/null || true
if install -m 755 "$TMP/burnban" "$BIN_PATH" 2>/dev/null; then :
elif command -v sudo >/dev/null 2>&1; then
  sudo mkdir -p "$BIN_DIR"
  sudo install -m 755 "$TMP/burnban" "$BIN_PATH"
else
  echo "burnban: cannot install to $BIN_DIR; rerun with --bin-dir \"$HOME/.local/bin\"" >&2
  exit 1
fi

add_user_path() {
  [ "$UPDATE_PATH" = 1 ] || return 0
  [ "$BIN_DIR" = "$HOME/.local/bin" ] || return 0
  case "${SHELL:-}" in
    */zsh) profile="$HOME/.zprofile" ;;
    */bash) profile="$HOME/.bashrc" ;;
    *) profile="$HOME/.profile" ;;
  esac
  touch "$profile"
  if ! grep -F '$HOME/.local/bin' "$profile" >/dev/null 2>&1; then
    printf '\n# Added by the Burnban installer\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$profile"
    echo "   PATH updated in $profile (applies to new terminals)"
  fi
}

create_macos_app() {
  app="$HOME/Applications/Burnban.app"
  launcher="$app/Contents/MacOS/burnban-launcher"
  mkdir -p "$app/Contents/MacOS"
  escaped_bin=$(printf '%s' "$BIN_PATH" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '#!/bin/sh\nexec "%s" desktop "$@"\n' "$escaped_bin" > "$launcher"
  chmod 755 "$launcher"
  cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Burnban</string>
  <key>CFBundleDisplayName</key><string>Burnban</string>
  <key>CFBundleIdentifier</key><string>dev.burnban.desktop</string>
  <key>CFBundleExecutable</key><string>burnban-launcher</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSBackgroundOnly</key><false/>
</dict></plist>
EOF
  if [ -d "$HOME/Desktop" ] && [ ! -e "$HOME/Desktop/Burnban.app" ]; then
    ln -s "$app" "$HOME/Desktop/Burnban.app"
  fi
  echo "   desktop app: $app"
}

create_linux_desktop() {
  data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
  entry="$data_home/applications/burnban.desktop"
  mkdir -p "$data_home/applications"
  cat > "$entry" <<EOF
[Desktop Entry]
Type=Application
Name=Burnban
Comment=Meter, itemize, and cap AI agent usage
Exec="$BIN_PATH" desktop
Icon=utilities-system-monitor
Terminal=false
Categories=Development;System;Utility;
StartupNotify=true
EOF
  chmod 755 "$entry"
  desktop_dir="$HOME/Desktop"
  if command -v xdg-user-dir >/dev/null 2>&1; then
    desktop_dir=$(xdg-user-dir DESKTOP 2>/dev/null || printf '%s' "$desktop_dir")
  fi
  if [ -d "$desktop_dir" ]; then
    cp "$entry" "$desktop_dir/Burnban.desktop"
    chmod 755 "$desktop_dir/Burnban.desktop"
    if command -v gio >/dev/null 2>&1; then
      gio set "$desktop_dir/Burnban.desktop" metadata::trusted true >/dev/null 2>&1 || true
    fi
  fi
  if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$data_home/applications" >/dev/null 2>&1 || true
  fi
  echo "   application: $entry"
}

add_user_path
if [ "$CREATE_DESKTOP" = 1 ]; then
  if [ "$OS" = darwin ]; then create_macos_app; else create_linux_desktop; fi
fi

echo "✅ installed: $("$BIN_PATH" version)"
echo "   real dashboard: $BIN_PATH desktop"
echo "   terminal meter:  $BIN_PATH serve"
echo "   local usage:     $BIN_PATH subsidy"
