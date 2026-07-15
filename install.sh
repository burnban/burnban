#!/bin/sh
# Burnban installer for macOS and Linux — https://burnban.dev
set -eu
umask 077

REPO="burnban/burnban"
CREATE_DESKTOP="${BURNBAN_CREATE_DESKTOP:-1}"
CREATE_AUTOSTART="${BURNBAN_CREATE_AUTOSTART:-1}"
LAUNCH_AFTER_INSTALL="${BURNBAN_LAUNCH_AFTER_INSTALL:-1}"
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
usage: install.sh [--no-desktop] [--no-autostart] [--no-launch]
                  [--no-path] [--bin-dir DIR]
                  [--uninstall [--purge]]

Installs the Burnban CLI, a one-click desktop launcher, and a per-user login
start entry. A successful interactive install starts the meter immediately.
Uninstall removes only files recorded in Burnban's install manifest. User data
is retained unless --purge is explicitly supplied.

Environment:
  BIN_DIR                       binary destination override
  BURNBAN_CREATE_DESKTOP=0      skip desktop/application launchers
  BURNBAN_CREATE_AUTOSTART=0    skip the per-user login start entry
  BURNBAN_LAUNCH_AFTER_INSTALL=0 do not start the meter after guided setup
  BURNBAN_UPDATE_PATH=0         do not update the shell PATH
  BURNBAN_DOWNLOAD_BASE_URL=... release download override (testing/mirrors)
  BURNBAN_INSTALL_STATE_DIR=... install-manifest directory override
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-desktop) CREATE_DESKTOP=0 ;;
    --no-autostart) CREATE_AUTOSTART=0 ;;
    --no-launch) LAUNCH_AFTER_INSTALL=0 ;;
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

is_managed_macos_startup() {
  [ -f "$1" ] && [ ! -L "$1" ] &&
    grep -Fq '<!-- burnban-installer-v1 -->' "$1" 2>/dev/null &&
    grep -Fq '<string>dev.burnban.meter</string>' "$1" 2>/dev/null
}

is_managed_linux_startup() {
  [ -f "$1" ] && [ ! -L "$1" ] &&
    grep -Fqx 'X-Burnban-Managed=true' "$1" 2>/dev/null &&
    grep -Fqx 'X-Burnban-Autostart=true' "$1" 2>/dev/null
}

xml_escape() {
  printf '%s' "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g'
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

remove_startup_path() {
  kind=$1
  path=$2
  [ -n "$path" ] || return 0
  reject_newline "$path"
  case "$kind" in
    mac-agent)
      if is_managed_macos_startup "$path"; then
        if [ -x /bin/launchctl ]; then
          /bin/launchctl bootout "gui/$(id -u)" "$path" >/dev/null 2>&1 || true
        fi
        rm -f "$path"
      elif [ -e "$path" ]; then
        echo "burnban: leaving unmarked login-start agent: $path" >&2
      fi
      ;;
    linux-autostart)
      if is_managed_linux_startup "$path"; then
        rm -f "$path"
      elif [ -e "$path" ]; then
        echo "burnban: leaving unmarked login-start entry: $path" >&2
      fi
      ;;
  esac
}

deactivate_startup_path() {
  kind=$1
  path=$2
  [ -n "$path" ] || return 0
  if [ "$kind" = mac-agent ] && is_managed_macos_startup "$path" && [ -x /bin/launchctl ]; then
    /bin/launchctl bootout "gui/$(id -u)" "$path" >/dev/null 2>&1 || true
  fi
}

stop_default_meter() {
  binary=$1
  [ -x "$binary" ] || return 0
  "$binary" stop >/dev/null 2>&1 || true
}

require_meter_stopped_for_purge() {
  if command -v pgrep >/dev/null 2>&1; then
    if pgrep -f '[b]urnban (serve|desktop|demo)( |$)' >/dev/null 2>&1; then
      echo "burnban: stop the running Burnban meter before using --purge" >&2
      return 1
    else
      pgrep_status=$?
      if [ "$pgrep_status" -eq 1 ]; then
        return 0
      fi
      echo "burnban: cannot inspect running processes (pgrep exited $pgrep_status); refusing --purge" >&2
      return 1
    fi
  fi
  # procps is not installed by default on some minimal Linux systems. Use the
  # macOS/Linux ps interface when available, but never interpret an unavailable
  # liveness check as proof that recursive data deletion is safe.
  if command -v ps >/dev/null 2>&1; then
    if ! process_list=$(ps -ax -o command= 2>/dev/null); then
      echo "burnban: cannot inspect running processes; refusing --purge" >&2
      return 1
    fi
    if printf '%s\n' "$process_list" |
       grep -E '[b]urnban (serve|desktop|demo)( |$)' >/dev/null 2>&1; then
      echo "burnban: stop the running Burnban meter before using --purge" >&2
      return 1
    fi
    return 0
  fi
  echo "burnban: cannot verify meter liveness (pgrep or ps is required); refusing --purge" >&2
  return 1
}

purge_data() {
  require_meter_stopped_for_purge || return 1
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
  incomplete=0
  recorded_binary=$(manifest_get binary)
  recorded_profile=$(manifest_get profile)
  path_added=$(manifest_get path_added)
  desktop_kind_1=$(manifest_get desktop_kind_1)
  desktop_path_1=$(manifest_get desktop_path_1)
  desktop_kind_2=$(manifest_get desktop_kind_2)
  desktop_path_2=$(manifest_get desktop_path_2)
  startup_kind=$(manifest_get startup_kind)
  startup_path=$(manifest_get startup_path)

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

  target_is_burnban=0
  if [ -e "$target" ]; then
    if ! is_burnban_binary "$target"; then
      echo "burnban: refusing to remove a file that is not a Burnban binary: $target" >&2
      incomplete=1
    else
      target_is_burnban=1
    fi
  fi

  deactivate_startup_path "$startup_kind" "$startup_path"
  if [ ! -f "$MANIFEST" ]; then
    if [ "$OS" = darwin ]; then
      legacy_startup_kind=mac-agent
      legacy_startup_path="$HOME/Library/LaunchAgents/dev.burnban.meter.plist"
    else
      legacy_startup_kind=linux-autostart
      legacy_startup_path="${XDG_CONFIG_HOME:-$HOME/.config}/autostart/burnban-meter.desktop"
    fi
    deactivate_startup_path "$legacy_startup_kind" "$legacy_startup_path"
  fi
  if [ "$target_is_burnban" -eq 1 ]; then
    stop_default_meter "$target"
  fi
  if [ "$PURGE" -eq 1 ]; then
    require_meter_stopped_for_purge || return 1
  fi

  remove_startup_path "$startup_kind" "$startup_path"
  if [ ! -f "$MANIFEST" ]; then
    remove_startup_path "$legacy_startup_kind" "$legacy_startup_path"
  fi

  if [ "$target_is_burnban" -eq 1 ]; then
    if rm -f "$target" 2>/dev/null; then
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
    echo "burnban removed"
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
STAGED_PATH=""
cleanup_install() {
  rc=$?
  trap - EXIT HUP INT TERM
  if [ -n "$STAGED_PATH" ] && [ -e "$STAGED_PATH" ]; then
    rm -f "$STAGED_PATH" 2>/dev/null || {
      if command -v sudo >/dev/null 2>&1; then sudo rm -f "$STAGED_PATH" || true; fi
    }
  fi
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup_install EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

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

echo "downloading burnban for $OS/$ARCH"
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
if STAGED_PATH=$(mktemp "$BIN_DIR/.burnban.install.XXXXXX" 2>/dev/null); then
  :
elif command -v sudo >/dev/null 2>&1; then
  sudo mkdir -p "$BIN_DIR"
  STAGED_PATH=$(sudo mktemp "$BIN_DIR/.burnban.install.XXXXXX")
else
  echo "burnban: cannot create a safe staging file in $BIN_DIR" >&2
  exit 1
fi
if install -m 755 "$TMP/burnban" "$STAGED_PATH" 2>/dev/null; then
  :
elif command -v sudo >/dev/null 2>&1; then
  sudo install -m 755 "$TMP/burnban" "$STAGED_PATH"
else
  echo "burnban: cannot install to $BIN_DIR; rerun with --bin-dir \"$HOME/.local/bin\"" >&2
  exit 1
fi
if ! is_burnban_binary "$STAGED_PATH"; then
  echo "burnban: downloaded binary did not pass its version check; existing install was retained" >&2
  exit 1
fi
# Recheck immediately before the atomic replacement so a changed target is
# never silently overwritten during the download window.
if [ -e "$BIN_PATH" ] && ! is_burnban_binary "$BIN_PATH"; then
  echo "burnban: refusing to overwrite a non-Burnban file: $BIN_PATH" >&2
  exit 1
fi
if mv -f "$STAGED_PATH" "$BIN_PATH" 2>/dev/null; then
  STAGED_PATH=""
elif command -v sudo >/dev/null 2>&1; then
  sudo mv -f "$STAGED_PATH" "$BIN_PATH"
  STAGED_PATH=""
else
  echo "burnban: cannot replace $BIN_PATH; the existing install was retained" >&2
  exit 1
fi
if ! is_burnban_binary "$BIN_PATH"; then
  echo "burnban: installed binary did not pass its version check" >&2
  exit 1
fi

# Preserve ownership metadata across upgrades. Otherwise a reinstall with
# --no-path, --no-desktop, or --no-autostart could orphan integrations created
# by an earlier installer run.
PROFILE_PATH=$(manifest_get profile)
PATH_ADDED=$(manifest_get path_added)
DESKTOP_KIND_1=$(manifest_get desktop_kind_1)
DESKTOP_PATH_1=$(manifest_get desktop_path_1)
DESKTOP_KIND_2=$(manifest_get desktop_kind_2)
DESKTOP_PATH_2=$(manifest_get desktop_path_2)
STARTUP_KIND=$(manifest_get startup_kind)
STARTUP_PATH=$(manifest_get startup_path)
case "$PATH_ADDED" in 1) ;; *) PATH_ADDED=0 ;; esac
case "$DESKTOP_KIND_1" in mac-app|linux-entry) ;; *) DESKTOP_KIND_1=""; DESKTOP_PATH_1="" ;; esac
case "$DESKTOP_KIND_2" in mac-link|linux-entry) ;; *) DESKTOP_KIND_2=""; DESKTOP_PATH_2="" ;; esac
case "$STARTUP_KIND" in mac-agent|linux-autostart) ;; *) STARTUP_KIND=""; STARTUP_PATH="" ;; esac
for value in "$PROFILE_PATH" "$DESKTOP_PATH_1" "$DESKTOP_PATH_2" "$STARTUP_PATH"; do reject_newline "$value"; done

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
    printf 'startup_kind=%s\n' "$STARTUP_KIND"
    printf 'startup_path=%s\n' "$STARTUP_PATH"
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

create_macos_startup() {
  agent="$HOME/Library/LaunchAgents/dev.burnban.meter.plist"
  if [ -e "$agent" ] && ! is_managed_macos_startup "$agent"; then
    echo "burnban: leaving unrecognized login-start agent: $agent" >&2
    return 0
  fi
  mkdir -p "$(dirname "$agent")"
  escaped_bin=$(xml_escape "$BIN_PATH")
  escaped_log=$(xml_escape "$STATE_DIR/startup.log")
  cat > "$agent" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- burnban-installer-v1 -->
<plist version="1.0"><dict>
  <key>Label</key><string>dev.burnban.meter</string>
  <key>ProgramArguments</key><array>
    <string>$escaped_bin</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>$escaped_log</string>
  <key>StandardErrorPath</key><string>$escaped_log</string>
</dict></plist>
EOF
  chmod 600 "$agent"
  STARTUP_KIND=mac-agent
  STARTUP_PATH=$agent
  echo "   starts at login: $agent"
}

create_linux_startup() {
  config_home="${XDG_CONFIG_HOME:-$HOME/.config}"
  entry="$config_home/autostart/burnban-meter.desktop"
  if [ -e "$entry" ] && ! is_managed_linux_startup "$entry"; then
    echo "burnban: leaving unrecognized login-start entry: $entry" >&2
    return 0
  fi
  mkdir -p "$(dirname "$entry")"
  cat > "$entry" <<EOF
[Desktop Entry]
Type=Application
Name=Burnban Meter
Comment=Keep the local AI spend meter available
Exec="$BIN_PATH" serve
Terminal=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
X-Burnban-Autostart=true
X-Burnban-Managed=true
EOF
  chmod 600 "$entry"
  STARTUP_KIND=linux-autostart
  STARTUP_PATH=$entry
  echo "   starts at login: $entry"
}

wait_for_meter() {
  attempt=0
  while [ "$attempt" -lt 50 ]; do
    if "$BIN_PATH" status >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
    attempt=$((attempt + 1))
  done
  return 1
}

start_meter_now() {
  if "$BIN_PATH" status >/dev/null 2>&1; then
    return 0
  fi

  started_by_supervisor=0
  if [ "$STARTUP_KIND" = mac-agent ] && is_managed_macos_startup "$STARTUP_PATH" &&
     [ -x /bin/launchctl ]; then
    /bin/launchctl bootout "gui/$(id -u)" "$STARTUP_PATH" >/dev/null 2>&1 || true
    if /bin/launchctl bootstrap "gui/$(id -u)" "$STARTUP_PATH" >/dev/null 2>&1; then
      started_by_supervisor=1
    fi
  fi

  if [ "$started_by_supervisor" -eq 0 ]; then
    mkdir -p "$STATE_DIR"
    if command -v nohup >/dev/null 2>&1; then
      nohup "$BIN_PATH" serve >> "$STATE_DIR/startup.log" 2>&1 &
    else
      "$BIN_PATH" serve >> "$STATE_DIR/startup.log" 2>&1 &
    fi
  fi

  if wait_for_meter; then
    echo "   meter running: http://localhost:4141"
    return 0
  fi
  echo "burnban: the meter did not become healthy; inspect $STATE_DIR/startup.log" >&2
  return 1
}

open_configured_interface() {
  mkdir -p "$STATE_DIR"
  if command -v nohup >/dev/null 2>&1; then
    nohup "$BIN_PATH" >> "$STATE_DIR/launch.log" 2>&1 &
  else
    "$BIN_PATH" >> "$STATE_DIR/launch.log" 2>&1 &
  fi
}

add_user_path
write_manifest
if [ "$CREATE_DESKTOP" = 1 ]; then
  if [ "$OS" = darwin ]; then create_macos_app; else create_linux_desktop; fi
fi
write_manifest
if [ "$CREATE_AUTOSTART" = 1 ]; then
  if [ "$OS" = darwin ]; then create_macos_startup; else create_linux_startup; fi
fi
write_manifest

echo "installed: $("$BIN_PATH" version)"
echo

# Hand a real person straight into the guided setup. `curl | sh` leaves stdin
# as the pipe, so read the terminal from /dev/tty when one is attached; if
# there is none (CI, no TTY), just point them at the command.
if [ -r /dev/tty ] && [ -t 1 ]; then
  if "$BIN_PATH" setup --if-needed --no-launch </dev/tty; then
    if [ "$LAUNCH_AFTER_INSTALL" = 1 ]; then
      echo "Starting Burnban..."
      if start_meter_now; then
        open_configured_interface
      fi
    fi
  else
    echo "burnban: guided setup paused; finish later with: burnban setup" >&2
  fi
else
  echo "Get started:"
  echo "   burnban setup     guided setup: see local usage or connect enforcement"
  echo "   burnban guide     what burnban does, in plain language"
fi
