#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
cast=${1:-"$root/docs/usage-share.cast"}
gif=${2:-"$root/docs/usage-share.gif"}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/burnban-share-recording.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

command -v asciinema >/dev/null 2>&1 || {
  echo "asciinema is required: https://docs.asciinema.org/manual/cli/" >&2
  exit 1
}
command -v agg >/dev/null 2>&1 || {
  echo "agg is required: https://docs.asciinema.org/manual/agg/" >&2
  exit 1
}

cd "$root"
go build -trimpath -o "$tmp/burnban" .
mkdir -p "$tmp/fixture/claude"
timestamp=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
sed "s/2026-07-11T22:00:00Z/$timestamp/" \
  testdata/usage-share/claude/demo.jsonl >"$tmp/fixture/claude/demo.jsonl"
BURNBAN_RECORDING_BIN="$tmp/burnban" \
BURNBAN_RECORDING_FIXTURE="$tmp/fixture" \
asciinema record \
  --quiet --headless --overwrite --return \
  --output-format asciicast-v2 --window-size 68x13 \
  --title "burnban usage --share · deterministic fixture" \
  --command ./scripts/usage_share_demo.sh \
  "$cast"
agg --quiet --theme github-dark --font-size 17 --line-height 1.35 \
  --cols 68 --rows 13 --fps-cap 20 --last-frame-duration 4 \
  --select event:0.. \
  "$cast" "$gif"
