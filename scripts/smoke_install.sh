#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
if [ -n "${BURNBAN_RELEASE_DIR:-}" ]; then
  RELEASE=$(CDPATH='' cd -- "$BURNBAN_RELEASE_DIR" && pwd)
else
  command -v goreleaser >/dev/null 2>&1 || {
    echo "smoke_install.sh: set BURNBAN_RELEASE_DIR or install goreleaser" >&2
    exit 2
  }
  (cd "$ROOT" && goreleaser release --snapshot --clean)
  RELEASE="$ROOT/dist"
fi

TMP=$(mktemp -d)
SERVE_PID=""
cleanup() {
  rc=$?
  if [ -n "$SERVE_PID" ]; then
    kill "$SERVE_PID" >/dev/null 2>&1 || true
    wait "$SERVE_PID" >/dev/null 2>&1 || true
  fi
  if [ "$rc" -ne 0 ] && [ -f "$TMP/serve.log" ]; then cat "$TMP/serve.log" >&2; fi
  rm -rf "$TMP"
  trap - EXIT HUP INT TERM
  exit "$rc"
}
trap cleanup EXIT HUP INT TERM
HOME_DIR="$TMP/home"
BIN_DIR="$HOME_DIR/.local/bin"
mkdir -p "$HOME_DIR/Desktop" "$BIN_DIR"
printf '%s\n' keep > "$BIN_DIR/unrelated.keep"
printf '%s\n' 'export KEEP_THIS_PROFILE_LINE=1' > "$HOME_DIR/.zprofile"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported smoke-test architecture" >&2; exit 1 ;;
esac
ARCHIVE="burnban_${OS}_${ARCH}.tar.gz"

PREEXISTING_METER=0
if command -v pgrep >/dev/null 2>&1 &&
   pgrep -f '[b]urnban (serve|desktop|demo)( |$)' >/dev/null 2>&1; then
  PREEXISTING_METER=1
fi

test -f "$RELEASE/$ARCHIVE"
test -f "$RELEASE/checksums.txt"
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)burnban$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)LICENSE$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)DATA_AND_PRIVACY.md$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)SECURITY.md$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)THIRD_PARTY_NOTICES.md$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)docs/dashboard.png$'
tar -tzf "$RELEASE/$ARCHIVE" | grep -Eq '(^|/)third_party_licenses/.+/LICENSE'

run_install() {
  HOME="$HOME_DIR" SHELL=/bin/zsh BIN_DIR="$BIN_DIR" \
    BURNBAN_DOWNLOAD_BASE_URL="$RELEASE" BURNBAN_CREATE_DESKTOP=1 \
    sh "$ROOT/install.sh" "$@"
}

run_install
test -x "$BIN_DIR/burnban"
"$BIN_DIR/burnban" version | grep -q '^burnban '
test -f "$HOME_DIR/.burnban/install-manifest"
test -f "$HOME_DIR/.burnban/.burnban-installer-data"
grep -Fq '# >>> burnban installer managed PATH >>>' "$HOME_DIR/.zprofile"
case "$OS" in
  darwin)
    test -x "$HOME_DIR/Applications/Burnban.app/Contents/MacOS/burnban-launcher"
    test -f "$HOME_DIR/Applications/Burnban.app/Contents/Resources/burnban-managed"
    test -L "$HOME_DIR/Desktop/Burnban.app"
    ;;
  linux)
    test -x "$HOME_DIR/.local/share/applications/burnban.desktop"
    test -x "$HOME_DIR/Desktop/Burnban.desktop"
    grep -Fq 'X-Burnban-Managed=true' "$HOME_DIR/.local/share/applications/burnban.desktop"
    ;;
esac

# Reinstalling with optional integrations disabled must retain ownership of the
# PATH and launchers from the previous run so the next uninstall can clean up.
run_install --no-desktop --no-path
test "$(grep -Fc '# >>> burnban installer managed PATH >>>' "$HOME_DIR/.zprofile")" -eq 1
grep -Fq 'path_added=1' "$HOME_DIR/.burnban/install-manifest"
for staged_binary in "$BIN_DIR"/.burnban.install.*; do
  test ! -e "$staged_binary" || {
    echo "atomic reinstall left a staging executable behind: $staged_binary" >&2
    exit 1
  }
done
case "$OS" in
  darwin) test -x "$HOME_DIR/Applications/Burnban.app/Contents/MacOS/burnban-launcher" ;;
  linux) test -x "$HOME_DIR/.local/share/applications/burnban.desktop" ;;
esac

# A checksum-valid but invalid replacement must fail while preserving the
# preceding executable. This exercises staging/validation, not just the
# checksum rejection path covered below.
INVALID_UPGRADE="$TMP/invalid-upgrade-release"
INVALID_PAYLOAD="$TMP/invalid-upgrade-payload"
mkdir -p "$INVALID_UPGRADE" "$INVALID_PAYLOAD"
printf '#!/bin/sh\nprintf "not burnban\\n"\n' > "$INVALID_PAYLOAD/burnban"
chmod 755 "$INVALID_PAYLOAD/burnban"
tar -czf "$INVALID_UPGRADE/$ARCHIVE" -C "$INVALID_PAYLOAD" burnban
if command -v sha256sum >/dev/null 2>&1; then
  INVALID_HASH=$(sha256sum "$INVALID_UPGRADE/$ARCHIVE" | awk '{print $1}')
else
  INVALID_HASH=$(shasum -a 256 "$INVALID_UPGRADE/$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$INVALID_HASH" "$ARCHIVE" > "$INVALID_UPGRADE/checksums.txt"
VERSION_BEFORE_INVALID_UPGRADE=$("$BIN_DIR/burnban" version)
if HOME="$HOME_DIR" SHELL=/bin/zsh BIN_DIR="$BIN_DIR" \
   BURNBAN_DOWNLOAD_BASE_URL="$INVALID_UPGRADE" BURNBAN_CREATE_DESKTOP=0 \
   BURNBAN_UPDATE_PATH=0 sh "$ROOT/install.sh" --no-desktop --no-path \
   > "$TMP/invalid-upgrade.log" 2>&1; then
  echo "invalid checksum-valid upgrade unexpectedly installed" >&2
  exit 1
fi
grep -Fq 'existing install was retained' "$TMP/invalid-upgrade.log"
test "$("$BIN_DIR/burnban" version)" = "$VERSION_BEFORE_INVALID_UPGRADE"
for staged_binary in "$BIN_DIR"/.burnban.install.*; do test ! -e "$staged_binary"; done

# Exercise the release binary's OS-assigned port, private lifecycle state,
# status control request, and authenticated graceful stop.
RUNTIME_DB="$TMP/runtime.db"
RUNTIME_STATE="$RUNTIME_DB.server.json"
printf '%s\n' keep > "$TMP/runtime-unrelated.keep"
"$BIN_DIR/burnban" serve --port 0 --db "$RUNTIME_DB" > "$TMP/serve.log" 2>&1 &
SERVE_PID=$!
attempt=0
while [ ! -f "$RUNTIME_STATE" ] && [ "$attempt" -lt 100 ]; do
  kill -0 "$SERVE_PID" >/dev/null 2>&1 || {
    echo "installed burnban exited before publishing lifecycle state" >&2
    exit 1
  }
  sleep 0.1
  attempt=$((attempt + 1))
done
test -f "$RUNTIME_STATE"
grep -Fq "\"pid\": $SERVE_PID" "$RUNTIME_STATE"
RUNTIME_URL=$(sed -n 's/.*"url": "\([^"]*\)".*/\1/p' "$RUNTIME_STATE")
test -n "$RUNTIME_URL"
case "$RUNTIME_URL" in *:0|*:0/) echo "port 0 was not replaced in lifecycle state" >&2; exit 1 ;; esac
curl -fsS "$RUNTIME_URL/health" | grep -q '"ok":true'
"$BIN_DIR/burnban" status --db "$RUNTIME_DB" | grep -Fq 'is running'
if run_install --uninstall --purge > "$TMP/running-purge.log" 2>&1; then
  echo "purge unexpectedly removed a running meter" >&2
  exit 1
fi
grep -Fq 'stop the running Burnban meter' "$TMP/running-purge.log"
test -x "$BIN_DIR/burnban"
test -f "$HOME_DIR/.burnban/install-manifest"
"$BIN_DIR/burnban" stop --db "$RUNTIME_DB" | grep -Fq 'burnban stopped'
wait "$SERVE_PID"
SERVE_PID=""
test ! -e "$RUNTIME_STATE"
test -f "$TMP/runtime-unrelated.keep"

printf '%s\n' ledger > "$HOME_DIR/.burnban/unrelated-data.keep"
run_install --uninstall
test ! -e "$BIN_DIR/burnban"
test -f "$BIN_DIR/unrelated.keep"
test -f "$HOME_DIR/.burnban/unrelated-data.keep"
test ! -e "$HOME_DIR/.burnban/install-manifest"
grep -Fq 'export KEEP_THIS_PROFILE_LINE=1' "$HOME_DIR/.zprofile"
if grep -Fq '# >>> burnban installer managed PATH >>>' "$HOME_DIR/.zprofile"; then
  echo "managed PATH block survived uninstall" >&2
  exit 1
fi
case "$OS" in
  darwin) test ! -e "$HOME_DIR/Applications/Burnban.app"; test ! -L "$HOME_DIR/Desktop/Burnban.app" ;;
  linux) test ! -e "$HOME_DIR/.local/share/applications/burnban.desktop"; test ! -e "$HOME_DIR/Desktop/Burnban.desktop" ;;
esac

run_install --no-desktop
if [ "$PREEXISTING_METER" -eq 1 ]; then
  if run_install --uninstall --purge; then
    echo "purge ignored a pre-existing running meter" >&2
    exit 1
  fi
  test -x "$BIN_DIR/burnban"
  test -f "$HOME_DIR/.burnban/install-manifest"
  run_install --uninstall
  test -d "$HOME_DIR/.burnban"
  echo "purge refusal passed; successful purge skipped because another Burnban meter was already running"
else
  run_install --uninstall --purge
  test ! -e "$HOME_DIR/.burnban"
fi
test ! -e "$BIN_DIR/burnban"
test -f "$BIN_DIR/unrelated.keep"

# Purge must fail closed when the installer cannot inspect running processes,
# while the portable ps fallback must keep minimal systems without pgrep usable.
PROCESS_PATH="$TMP/process-path"
NO_PROCESS_PATH="$TMP/no-process-path"
mkdir -p "$PROCESS_PATH" "$NO_PROCESS_PATH"
for command_name in uname tr ps grep basename rm; do
  command_path=$(command -v "$command_name")
  ln -s "$command_path" "$PROCESS_PATH/$command_name"
  case "$command_name" in ps) ;; *) ln -s "$command_path" "$NO_PROCESS_PATH/$command_name" ;; esac
done

PS_FALLBACK_HOME="$TMP/ps-fallback-home"
mkdir -p "$PS_FALLBACK_HOME/.burnban" "$PS_FALLBACK_HOME/bin"
printf '%s\n' 'burnban-installer-data-v1' > "$PS_FALLBACK_HOME/.burnban/.burnban-installer-data"
if [ "$PREEXISTING_METER" -eq 1 ]; then
  if PATH="$PROCESS_PATH" HOME="$PS_FALLBACK_HOME" BIN_DIR="$PS_FALLBACK_HOME/bin" \
     BURNBAN_CREATE_DESKTOP=0 BURNBAN_UPDATE_PATH=0 \
     /bin/sh "$ROOT/install.sh" --uninstall --purge > "$TMP/ps-fallback.log" 2>&1; then
    echo "ps fallback ignored a pre-existing running meter" >&2
    exit 1
  fi
  grep -Fq 'stop the running Burnban meter' "$TMP/ps-fallback.log"
  test -f "$PS_FALLBACK_HOME/.burnban/.burnban-installer-data"
else
  PATH="$PROCESS_PATH" HOME="$PS_FALLBACK_HOME" BIN_DIR="$PS_FALLBACK_HOME/bin" \
    BURNBAN_CREATE_DESKTOP=0 BURNBAN_UPDATE_PATH=0 \
    /bin/sh "$ROOT/install.sh" --uninstall --purge >/dev/null
  test ! -e "$PS_FALLBACK_HOME/.burnban"
fi

NO_PROCESS_HOME="$TMP/no-process-home"
mkdir -p "$NO_PROCESS_HOME/.burnban" "$NO_PROCESS_HOME/bin"
printf '%s\n' 'burnban-installer-data-v1' > "$NO_PROCESS_HOME/.burnban/.burnban-installer-data"
if PATH="$NO_PROCESS_PATH" HOME="$NO_PROCESS_HOME" BIN_DIR="$NO_PROCESS_HOME/bin" \
   BURNBAN_CREATE_DESKTOP=0 BURNBAN_UPDATE_PATH=0 \
   /bin/sh "$ROOT/install.sh" --uninstall --purge > "$TMP/no-process.log" 2>&1; then
  echo "purge succeeded without a process-liveness tool" >&2
  exit 1
fi
grep -Fq 'cannot verify meter liveness' "$TMP/no-process.log"
test -f "$NO_PROCESS_HOME/.burnban/.burnban-installer-data"

PGREP_FAILURE_PATH="$TMP/pgrep-failure-path"
PGREP_FAILURE_HOME="$TMP/pgrep-failure-home"
mkdir -p "$PGREP_FAILURE_PATH" "$PGREP_FAILURE_HOME/.burnban" "$PGREP_FAILURE_HOME/bin"
for command_name in uname tr; do
  command_path=$(command -v "$command_name")
  ln -s "$command_path" "$PGREP_FAILURE_PATH/$command_name"
done
printf '#!/bin/sh\nexit 2\n' > "$PGREP_FAILURE_PATH/pgrep"
chmod 755 "$PGREP_FAILURE_PATH/pgrep"
printf '%s\n' 'burnban-installer-data-v1' > "$PGREP_FAILURE_HOME/.burnban/.burnban-installer-data"
if PATH="$PGREP_FAILURE_PATH" HOME="$PGREP_FAILURE_HOME" BIN_DIR="$PGREP_FAILURE_HOME/bin" \
   BURNBAN_CREATE_DESKTOP=0 BURNBAN_UPDATE_PATH=0 \
   /bin/sh "$ROOT/install.sh" --uninstall --purge > "$TMP/pgrep-failure.log" 2>&1; then
  echo "purge treated a pgrep inspection failure as no running meter" >&2
  exit 1
fi
grep -Fq 'pgrep exited 2' "$TMP/pgrep-failure.log"
test -f "$PGREP_FAILURE_HOME/.burnban/.burnban-installer-data"

# A corrupt artifact must fail before anything is installed.
CORRUPT="$TMP/corrupt-release"
CORRUPT_HOME="$TMP/corrupt-home"
mkdir -p "$CORRUPT" "$CORRUPT_HOME/.local/bin"
cp "$RELEASE/$ARCHIVE" "$CORRUPT/$ARCHIVE"
cp "$RELEASE/checksums.txt" "$CORRUPT/checksums.txt"
printf 'corrupt' >> "$CORRUPT/$ARCHIVE"
if HOME="$CORRUPT_HOME" BIN_DIR="$CORRUPT_HOME/.local/bin" \
   BURNBAN_DOWNLOAD_BASE_URL="$CORRUPT" BURNBAN_CREATE_DESKTOP=0 \
   BURNBAN_UPDATE_PATH=0 sh "$ROOT/install.sh" >/dev/null 2>&1; then
  echo "corrupt artifact unexpectedly installed" >&2
  exit 1
fi
test ! -e "$CORRUPT_HOME/.local/bin/burnban"

echo "installer artifact smoke test passed for $OS/$ARCH"
