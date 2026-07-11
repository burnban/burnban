#!/bin/sh
# Burnban installer for macOS and Linux — https://burnban.dev
set -eu
umask 077

REPO="burnban/burnban"
CREATE_DESKTOP="${BURNBAN_CREATE_DESKTOP:-1}"
UPDATE_PATH="${BURNBAN_UPDATE_PATH:-1}"
UNINSTALL=0
PURGE=0
BIN_DIR_EXPLICIT=0
STATE_DIR="${BURNBAN_INSTALL_STATE_DIR:-$HOME/.burnban}"
MANIFEST="$STATE_DIR/install-manifest"
DATA_DIR="${BURNBAN_PURGE_DIR:-$HOME/.burnban}"
DATA_MARKER="$DATA_DIR/.burnban-installer-data"
PATH_BEGIN="# >>> burnban installer managed PATH >>>"
PATH_END="# <<< burnban installer managed PATH <<<"

usage() {
  cat <<'EOF'
usage: install.sh [--no-desktop] [--no-path] [--bin-dir DIR]
                  [--uninstall [--purge]]

Installs the Burnban CLI plus a one-click desktop launcher. Uninstall removes
only files recorded in Burnban's install manifest. User data is retained unless
--purge is explicitly supplied.

Environment:
  BIN_DIR                       binary destination override
  BURNBAN_CREATE_DESKTOP=0      skip desktop/application launchers
  BURNBAN_UPDATE_PATH=0         do not update the shell PATH
  BURNBAN_DOWNLOAD_BASE_URL=... release download override (testing/mirrors)
  BURNBAN_INSTALL_STATE_DIR=... install-manifest directory override
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
      BIN_DIR_EXPLICIT=1
      ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "burnban: unknown installer option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [ "$PURGE" -eq 1 ] && [ "$UNINSTALL" -ne 1 ]; then
  echo "burnban: --purge is only valid with --uninstall" >&2
  exit 2
fi

reject_newline() {
  case "$1" in
    *'
'*) echo "burnban: paths containing newlines are not supported" >&2; exit 2 ;;
  esac
}

for value in "$STATE_DIR" "$DATA_DIR"; do reject_newline "$value"; done

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
reject_newline "$BIN_DIR"
BIN_PATH="$BIN_DIR/burnban"

manifest_get() {
  key=$1
  [ -f "$MANIFEST" ] || return 0
  sed -n "s/^${key}=//p" "$MANIFEST" | sed -n '1p'
}

is_burnban_binary() {
  candidate=$1
  [ -f "$candidate" ] && [ -x "$candidate" ] &&
    "$candidate" version 2>/dev/null | grep '^burnban ' >/dev/null 2>&1
}

is_managed_linux_entry() {
  [ -f "$1" ] && grep -Fqx 'X-Burnban-Managed=true' "$1" 2>/dev/null
}

is_legacy_linux_entry() {
  [ -f "$1" ] && grep -Fqx 'Name=Burnban' "$1" 2>/dev/null &&
    grep -E '^Exec="?[^[:space:]"]*/burnban"?[[:space:]]+desktop([[:space:]]|$)' "$1" >/dev/null 2>&1
}

remove_profile_block() {
  profile=$1
  [ -f "$profile" ] || return 0
  case "$profile" in
    "$HOME/.zprofile"|"$HOME/.bashrc"|"$HOME/.profile") ;;
    *) echo "burnban: leaving unrecognized recorded profile path: $profile" >&2; return 0 ;;
  esac

  tmp="${profile}.burnban-remove.$$"
  awk -v begin="$PATH_BEGIN" -v end="$PATH_END" '
    $0 == begin { managed=1; next }
    managed && $0 == end { managed=0; next }
    managed { next }
    pending_legacy {
      if ($0 == "export PATH=\"$HOME/.local/bin:$PATH\"") {
        pending_legacy=0
        next
      }
      print "# Added by the Burnban installer"
      pending_legacy=0
    }
    $0 == "# Added by the Burnban installer" { pending_legacy=1; next }
    { print }
    END { if (pending_legacy) print "# Added by the Burnban installer" }
  ' "$profile" > "$tmp"
  cat "$tmp" > "$profile"
  rm -f "$tmp"
}

remove_desktop_path() {
  kind=$1
  path=$2
  [ -n "$path" ] || return 0
  reject_newline "$path"
  case "$kind" in
    mac-app)
      marker="$path/Contents/Resources/burnban-managed"
      if [ -f "$marker" ] && grep -Fqx 'burnban-installer-v1' "$marker"; then
        rm -rf "$path"
      elif [ -e "$path" ]; then
        echo "burnban: leaving unmarked application bundle: $path" >&2
      fi
      ;;
    mac-link)
      [ ! -L "$path" ] || rm -f "$path"
      ;;
    linux-entry)
      if is_managed_linux_entry "$path"; then
        rm -f "$path"
      elif [ -e "$path" ]; then
        echo "burnban: leaving unmarked desktop entry: $path" >&2
      fi
      ;;
  esac
}

burnban_meter_running() {
  command -v pgrep >/dev/null 2>&1 &&
    pgrep -f '[b]urnban (serve|desktop|demo)( |$)' >/dev/null 2>&1
}

purge_data() {
  if burnban_meter_running; then
    echo "burnban: stop the running Burnban meter before using --purge" >&2
    return 1
  fi
  case "$(basename "$DATA_DIR")" in
    .burnban) ;;
    *) echo "burnban: refusing to purge unexpected data directory: $DATA_DIR" >&2; return 1 ;;
  esac
  if [ ! -f "$DATA_MARKER" ] || ! grep -Fqx 'burnban-installer-data-v1' "$DATA_MARKER"; then
    echo "burnban: refusing to purge unmarked data directory: $DATA_DIR" >&2
    return 1
  fi
  rm -rf "$DATA_DIR"
  echo "   data purged: $DATA_DIR"
}

uninstall() {
  if [ "$PURGE" -eq 1 ] && burnban_meter_running; then
    echo "burnban: stop the running Burnban meter before using --purge" >&2
    return 1
  fi
  incomplete=0
  recorded_binary=$(manifest_get binary)
  recorded_profile=$(manifest_get profile)
  path_added=$(manifest_get path_added)
  desktop_kind_1=$(manifest_get desktop_kind_1)
  desktop_path_1=$(manifest_get desktop_path_1)
  desktop_kind_2=$(manifest_get desktop_kind_2)
  desktop_path_2=$(manifest_get desktop_path_2)

  if [ -n "$recorded_binary" ]; then
    if [ "$BIN_DIR_EXPLICIT" -eq 1 ] && [ "$recorded_binary" != "$BIN_PATH" ]; then
      echo "burnban: --bin-dir does not match the recorded install: $recorded_binary" >&2
      return 1
    fi
    target=$recorded_binary
  else
    target=$BIN_PATH
    echo "burnban: no install manifest found; using conservative legacy cleanup" >&2
  fi
  reject_newline "$target"

  if [ -e "$target" ]; then
    if ! is_burnban_binary "$target"; then
      echo "burnban: refusing to remove a file that is not a Burnban binary: $target" >&2
      incomplete=1
    elif rm -f "$target" 2>/dev/null; then
      :
    elif command -v sudo >/dev/null 2>&1; then
      sudo rm -f "$target"
    else
      echo "burnban: cannot remove $target (permission denied)" >&2
      return 1
    fi
  fi

  if [ "$path_added" = 1 ] && [ -n "$recorded_profile" ]; then
    remove_profile_block "$recorded_profile"
  elif [ ! -f "$MANIFEST" ]; then
    for profile in "$HOME/.zprofile" "$HOME/.bashrc" "$HOME/.profile"; do
      remove_profile_block "$profile"
    done
  fi

  remove_desktop_path "$desktop_kind_2" "$desktop_path_2"
  remove_desktop_path "$desktop_kind_1" "$desktop_path_1"

  if [ ! -f "$MANIFEST" ]; then
    if [ "$OS" = darwin ]; then
      remove_desktop_path mac-link "$HOME/Desktop/Burnban.app"
      remove_desktop_path mac-app "$HOME/Applications/Burnban.app"
    else
      data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
      remove_desktop_path linux-entry "$data_home/applications/burnban.desktop"
      remove_desktop_path linux-entry "$HOME/Desktop/Burnban.desktop"
    fi
  fi

  if [ "$incomplete" -eq 0 ]; then
    if [ "$PURGE" -eq 1 ]; then
      if ! purge_data; then
        echo "burnban: purge stopped; the install manifest was retained at $MANIFEST" >&2
        return 1
      fi
      rm -f "$MANIFEST"
    else
      rm -f "$MANIFEST"
      echo "   data retained: $DATA_DIR (use --uninstall --purge to remove it)"
      rmdir "$STATE_DIR" 2>/dev/null || true
    fi
    echo "✅ burnban removed"
    return 0
  fi

  echo "burnban: uninstall incomplete; the manifest was retained at $MANIFEST" >&2
  return 1
}

if [ "$UNINSTALL" -eq 1 ]; then
  uninstall
  exit $?
fi

if [ -f "$MANIFEST" ]; then
  previous_binary=$(manifest_get binary)
  if [ -n "$previous_binary" ] && [ "$previous_binary" != "$BIN_PATH" ]; then
    echo "burnban: already installed at $previous_binary; uninstall it before changing --bin-dir" >&2
    exit 1
  fi
fi

if [ -e "$BIN_PATH" ] && ! is_burnban_binary "$BIN_PATH"; then
  echo "burnban: refusing to overwrite a non-Burnban file: $BIN_PATH" >&2
  exit 1
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
    curl -fsSL --proto '=https' --tlsv1.2 "${BASE_URL%/}/$name" -o "$destination"
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
if install -m 755 "$TMP/burnban" "$BIN_PATH" 2>/dev/null; then
  :
elif command -v sudo >/dev/null 2>&1; then
  sudo mkdir -p "$BIN_DIR"
  sudo install -m 755 "$TMP/burnban" "$BIN_PATH"
else
  echo "burnban: cannot install to $BIN_DIR; rerun with --bin-dir \"$HOME/.local/bin\"" >&2
  exit 1
fi
if ! is_burnban_binary "$BIN_PATH"; then
  echo "burnban: installed binary did not pass its version check" >&2
  exit 1
fi

# Preserve ownership metadata across upgrades. Otherwise a reinstall with
# --no-path or --no-desktop could orphan the PATH block or launchers created by
# an earlier installer run.
PROFILE_PATH=$(manifest_get profile)
PATH_ADDED=$(manifest_get path_added)
DESKTOP_KIND_1=$(manifest_get desktop_kind_1)
DESKTOP_PATH_1=$(manifest_get desktop_path_1)
DESKTOP_KIND_2=$(manifest_get desktop_kind_2)
DESKTOP_PATH_2=$(manifest_get desktop_path_2)
case "$PATH_ADDED" in 1) ;; *) PATH_ADDED=0 ;; esac
case "$DESKTOP_KIND_1" in mac-app|linux-entry) ;; *) DESKTOP_KIND_1=""; DESKTOP_PATH_1="" ;; esac
case "$DESKTOP_KIND_2" in mac-link|linux-entry) ;; *) DESKTOP_KIND_2=""; DESKTOP_PATH_2="" ;; esac
for value in "$PROFILE_PATH" "$DESKTOP_PATH_1" "$DESKTOP_PATH_2"; do reject_newline "$value"; done

write_manifest() {
  mkdir -p "$STATE_DIR" "$DATA_DIR"
  chmod 700 "$STATE_DIR" "$DATA_DIR" 2>/dev/null || true
  printf '%s\n' 'burnban-installer-data-v1' > "$DATA_MARKER"
  manifest_tmp="${MANIFEST}.tmp.$$"
  {
    printf 'format=1\n'
    printf 'binary=%s\n' "$BIN_PATH"
    printf 'profile=%s\n' "$PROFILE_PATH"
    printf 'path_added=%s\n' "$PATH_ADDED"
    printf 'desktop_kind_1=%s\n' "$DESKTOP_KIND_1"
    printf 'desktop_path_1=%s\n' "$DESKTOP_PATH_1"
    printf 'desktop_kind_2=%s\n' "$DESKTOP_KIND_2"
    printf 'desktop_path_2=%s\n' "$DESKTOP_PATH_2"
  } > "$manifest_tmp"
  chmod 600 "$manifest_tmp"
  mv "$manifest_tmp" "$MANIFEST"
}

# Record the binary before making any additional user-environment changes. If
# installation is interrupted, uninstall still knows what it owns.
write_manifest

add_user_path() {
  [ "$UPDATE_PATH" = 1 ] || return 0
  [ "$BIN_DIR" = "$HOME/.local/bin" ] || return 0
  case "${SHELL:-}" in
    */zsh) profile="$HOME/.zprofile" ;;
    */bash) profile="$HOME/.bashrc" ;;
    *) profile="$HOME/.profile" ;;
  esac
  touch "$profile"
  if ! grep -F "\$HOME/.local/bin" "$profile" >/dev/null 2>&1; then
    {
      printf '\n%s\n' "$PATH_BEGIN"
      printf '%s\n' "export PATH=\"\$HOME/.local/bin:\$PATH\""
      printf '%s\n' "$PATH_END"
    } >> "$profile"
    PROFILE_PATH=$profile
    PATH_ADDED=1
    echo "   PATH updated in $profile (applies to new terminals)"
  fi
}

create_macos_app() {
  app="$HOME/Applications/Burnban.app"
  launcher="$app/Contents/MacOS/burnban-launcher"
  marker="$app/Contents/Resources/burnban-managed"
  if [ -e "$app" ] && [ ! -f "$marker" ] &&
     ! grep -Fq '<string>dev.burnban.desktop</string>' "$app/Contents/Info.plist" 2>/dev/null; then
    echo "burnban: leaving unrecognized application bundle: $app" >&2
    return 0
  fi
  mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
  # A single-quoted shell word keeps $, backticks, spaces, and other path
  # characters literal when Finder launches the generated script.
  escaped_bin=$(printf '%s' "$BIN_PATH" | sed "s/'/'\\\\''/g")
  printf '#!/bin/sh\nexec '\''%s'\'' desktop "$@"\n' "$escaped_bin" > "$launcher"
  chmod 755 "$launcher"
  printf '%s\n' 'burnban-installer-v1' > "$marker"
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
  DESKTOP_KIND_1=mac-app
  DESKTOP_PATH_1=$app
  link="$HOME/Desktop/Burnban.app"
  if [ -d "$HOME/Desktop" ] && [ ! -e "$link" ]; then
    ln -s "$app" "$link"
    DESKTOP_KIND_2=mac-link
    DESKTOP_PATH_2=$link
  elif [ -L "$link" ] && [ "$(readlink "$link")" = "$app" ]; then
    DESKTOP_KIND_2=mac-link
    DESKTOP_PATH_2=$link
  fi
  echo "   desktop app: $app"
}

create_linux_desktop() {
  data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
  entry="$data_home/applications/burnban.desktop"
  mkdir -p "$data_home/applications"
  if [ -e "$entry" ] && ! is_managed_linux_entry "$entry" && ! is_legacy_linux_entry "$entry"; then
    echo "burnban: leaving unrecognized desktop entry: $entry" >&2
    return 0
  fi
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
X-Burnban-Managed=true
EOF
  chmod 755 "$entry"
  DESKTOP_KIND_1=linux-entry
  DESKTOP_PATH_1=$entry
  desktop_dir="$HOME/Desktop"
  if command -v xdg-user-dir >/dev/null 2>&1; then
    desktop_dir=$(xdg-user-dir DESKTOP 2>/dev/null || printf '%s' "$desktop_dir")
  fi
  desktop_entry="$desktop_dir/Burnban.desktop"
  if [ -d "$desktop_dir" ]; then
    if [ ! -e "$desktop_entry" ] || is_managed_linux_entry "$desktop_entry" || is_legacy_linux_entry "$desktop_entry"; then
      cp "$entry" "$desktop_entry"
      chmod 755 "$desktop_entry"
      DESKTOP_KIND_2=linux-entry
      DESKTOP_PATH_2=$desktop_entry
      if command -v gio >/dev/null 2>&1; then
        gio set "$desktop_entry" metadata::trusted true >/dev/null 2>&1 || true
      fi
    else
      echo "burnban: leaving unrecognized desktop file: $desktop_entry" >&2
    fi
  fi
  if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$data_home/applications" >/dev/null 2>&1 || true
  fi
  echo "   application: $entry"
}

add_user_path
write_manifest
if [ "$CREATE_DESKTOP" = 1 ]; then
  if [ "$OS" = darwin ]; then create_macos_app; else create_linux_desktop; fi
fi
write_manifest

echo "✅ installed: $("$BIN_PATH" version)"
echo "   real dashboard: $BIN_PATH desktop"
echo "   terminal meter:  $BIN_PATH serve"
echo "   local usage:     $BIN_PATH subsidy"
