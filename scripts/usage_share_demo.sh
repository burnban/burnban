#!/bin/sh
set -eu

binary=${BURNBAN_RECORDING_BIN:-./burnban}
fixture=${BURNBAN_RECORDING_FIXTURE:-testdata/usage-share}

printf '\033[2m$ \033[0mburnban usage --share\n'
sleep 0.6
"$binary" usage --share --since 30d \
  --no-auto-metered \
  --claude-dir "$fixture/claude" \
  --codex-dir "$fixture/missing" \
  --gemini-dir "$fixture/missing" \
  --copilot-dir "$fixture/missing" \
  --cursor-db "$fixture/missing" \
  --opencode-db "$fixture/missing" \
  --hermes-db "$fixture/missing" \
  --openclaw-dir "$fixture/missing" \
  --goose-db "$fixture/missing"
