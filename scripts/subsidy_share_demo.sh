#!/bin/sh
set -eu

binary=${BURNBAN_RECORDING_BIN:-./burnban}
fixture=${BURNBAN_RECORDING_FIXTURE:-testdata/subsidy-share}

printf '\033[2m$ \033[0mburnban subsidy --share\n'
sleep 0.6
"$binary" subsidy --share --since 30d \
  --claude-dir "$fixture/claude" \
  --codex-dir "$fixture/missing" \
  --hermes-db "$fixture/missing" \
  --openclaw-dir "$fixture/missing" \
  --goose-db "$fixture/missing"
