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
case "$OS" in
  darwin) test -x "$HOME_DIR/Applications/Burnban.app/Contents/MacOS/burnban-launcher" ;;
  linux) test -x "$HOME_DIR/.local/share/applications/burnban.desktop" ;;
esac

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
