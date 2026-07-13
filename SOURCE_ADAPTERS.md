# Source adapter contract

Burnban source adapters recover usage metadata from stores that local coding
agents already maintain. They do not proxy traffic. The public Go contract is
[`sourceadapter`](sourceadapter/adapter.go), currently
`burnban.source/v1`.

## Safety contract

Every adapter manifest must declare and satisfy all three invariants:

- the source store is read-only;
- scanning performs no network access;
- emitted events contain no prompt or response content.

`Manifest.Validate` rejects an adapter that violates those invariants or uses a
different API version. Burnban also gives every adapter its own file, byte,
record, line-size, and wall-clock limits. Hitting a bound produces an explicit
partial report instead of a silently incomplete total.

Some source files contain conversation text because the source agent uses the
same file for resume history. An adapter may parse only the metadata fields it
needs; it must never copy content into an event, diagnostic, fixture, or error.

## Normalized event rules

An adapter emits one `sourceadapter.Event` per model call:

- `ID`: stable source-local deduplication key when available;
- `Model` and `Time`: observed model ID and request timestamp;
- `In`: full-price input tokens only;
- `CacheRead`: cached input removed from `In` when the source includes it there;
- `Out`: every output-priced token, including hidden reasoning tokens when the
  provider bills them as output;
- `CacheWrite5m` / `CacheWrite1h`: provider cache-write tiers when known;
- `CostUSD` + `CostKnown`: optional source estimate used only when Burnban has no
  matching dated table price;
- `BillingProvider`: non-empty only when the record proves pay-per-token usage;
- `Confidence`: `exact`, `estimated`, or `partial`.

`Event.Validate` rejects missing confidence/model/time, negative or implausibly
large counters, non-finite or inconsistent cost metadata, oversized labels,
and control characters. Burnban defaults only an omitted call count to one,
deduplicates non-empty event IDs within an adapter, uses checked integer and
floating-point aggregation, and owns all final pricing. A rejected event makes
the source/report explicitly partial; it never becomes exact zero-cost usage.
Billing is also classified per event: a provider containing both proven
pay-per-token and subscription events exposes separate dollar buckets and an
explicit mixed state instead of assigning the whole provider from its last
event.

## Implementing v1

```go
package exampleadapter

import (
    "path/filepath"
    "time"

    "github.com/burnban/burnban/sourceadapter"
)

type Adapter struct{}

func (Adapter) Manifest() sourceadapter.Manifest {
    return sourceadapter.Manifest{
        APIVersion:  sourceadapter.APIVersion,
        ID:          "example-agent",
        DisplayName: "Example Agent",
        Store:       "append-only JSONL usage records",
        Privacy:     sourceadapter.Privacy{ReadOnly: true},
    }
}

func (Adapter) DefaultPath(home string) string {
    return filepath.Join(home, ".example-agent", "usage")
}

func (Adapter) Scan(
    path string,
    since time.Time,
    limits sourceadapter.ScanLimits,
    emit func(sourceadapter.Event),
) (sourceadapter.ScanResult, error) {
    // Open path read-only, honor every limit, and emit metadata-only events.
    return sourceadapter.ScanResult{}, nil
}
```

The v1 registry is compile-time: Burnban does not download or execute adapter
binaries. A first-party or community adapter is reviewed in this repository and
added to `internal/subsidy/BuiltinAdapters`. This keeps one native binary and
avoids turning log discovery into a plugin-execution surface.

## Compatibility fixtures

Each adapter contribution must include synthetic fixtures that cover:

1. discovery at the documented default path;
2. exact token normalization and timestamps;
3. duplicate/resumed records;
4. records outside the requested window;
5. malformed or unknown records without prompt/response leakage;
6. at least one scan limit producing a visible partial result;
7. auth/billing classification when the source supports both subscription and
   pay-per-token modes.

Never commit a real source log. Run:

```sh
go test ./sourceadapter ./internal/subsidy
```

The built-in manifests and Gemini CLI, GitHub Copilot CLI, Cursor, and OpenCode
compatibility fixtures exercise the same public v1 contract used by new
adapters.

## Codex compatibility

Codex rollout files expose cumulative `token_count` totals rather than a stable
request ledger. Burnban advances a per-file baseline for every valid counter
record and emits only the delta, including when an older record falls before the
selected report window.

Subagent rollout files also begin by replaying the parent's conversation and
cumulative token history. Their first `session_meta` identifies the structured
`subagent` source, and the first trigger-turn
`inter_agent_communication_metadata` record marks the end of that replay.
Burnban uses inherited counters to establish the child's baseline but does not
emit them a second time; live child deltas after the boundary remain separate
usage. A subagent file without that explicit boundary fails closed with a
partial-scan warning instead of presenting copied parent traffic as exact usage.

## Gemini CLI compatibility

Gemini CLI support was checked on 2026-07-12 against upstream commit
[`f354eeb`](https://github.com/google-gemini/gemini-cli/commit/f354eebaf43b25bacb176007e449bb9a638fd101).
Burnban reads only the session header plus `gemini` message ID, timestamp,
model, and `TokensSummary` metadata under project `chats/` directories. It
validates the upstream composite total, cached-input subset, timestamp, and
bounded input/output/thinking/tool counters before emitting an exact event.
Only a validated re-appended record for the same message ID supersedes an
earlier copy.

Gemini CLI's session record does not identify whether API-key or Vertex traffic
was on a free or paid tier. Its adapter deliberately leaves `BillingProvider`
empty; a user who knows the window was billed can opt in with
`burnban subsidy --metered gemini-cli`.

## GitHub Copilot CLI compatibility

GitHub Copilot CLI support was checked on 2026-07-12 against the official
[`@github/copilot` 1.0.70 package](https://www.npmjs.com/package/@github/copilot/v/1.0.70)
(build metadata commit `1a7a0a2e78`) and its shipped
`schemas/session-events.schema.json`. GitHub documents that the configurable
[`COPILOT_HOME`](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-config-dir-reference)
contains `session-state/{session-id}/events.jsonl`.

The adapter considers only the latest valid durable `session.shutdown` event in
each file. It decodes per-model request counts, billing `tokenDetails`, and
input/output/cache totals, never message, reasoning text, tool, file, or
checkpoint events. Upstream's shutdown
`inputTokens` includes cache reads and writes, so Burnban removes both subsets
from full-price input before pricing. `reasoningTokens` is validated as a
subset of output and is not double-counted.

The shutdown schema exposes one aggregate cache-write count but not the
provider retention tier. Burnban maps that count to the 5-minute field for
pricing compatibility and marks the affected event/report partial instead of
claiming exact cache-write pricing.

Shutdown usage is cumulative for the session. A session that began before the
requested lower bound is included with `partial` confidence and a visible
boundary warning because the durable schema cannot split its aggregate at that
instant. Subscription usage is the default classification; use
`--metered github-copilot-cli` only when you know Copilot CLI used separately
billed model credentials. `--copilot-dir` overrides discovery.

## Cursor compatibility

Cursor support was checked on 2026-07-13 by static inspection of the official
stable macOS arm64 3.11.13 build, commit
`3f21b08f0b436a07be29fbfe00b304fa15553353`, from Cursor's
[download service](https://cursor.com/downloads) (downloaded DMG SHA-256
`8f5938d261590d69cd4e5fc8c02077f4967e65ce3f27012d84d87c48f0970191`). In that build, the
`cursorDiskKV` table of the per-user global `state.vscdb` stores each
`composerData:<id>` descriptor with an ordered
`fullConversationHeadersOnly` array and each full message separately at the
exact `bubbleId:<composer-id>:<bubble-id>` key. Submitted messages carry model
and timestamp metadata; usage-bearing AI messages carry stable bubble/usage
identifiers and input/output token totals.

Burnban opens only a regular-file database read-only. For the current layout it
walks the descriptor's ordered headers and joins only the exact composer/bubble
message key; parent, role, bubble ID, order, and timestamp bindings must agree.
Its SQLite projection selects tightly type- and size-checked composer/usage
identifiers, role, model, timestamp, and token counters. Raw source identifiers
are used only inside the reader to derive fixed, length-delimited SHA-256
deduplication keys; they are never emitted into an adapter event or report. The
projection returns no prompt/response text, thinking, file context, terminal
data, diffs, tool payloads, authentication state, unreferenced messages, or
other global storage values to the Go scanner. `--cursor-db` overrides platform
discovery.

This is an internal, undocumented store, and it does not expose a trustworthy
cache-read/cache-write or reasoning-token decomposition. The older embedded
`conversation` layout is retained as a compatibility path, but emits only when
the stored rows themselves prove the same model/time/token association; older
databases without that metadata can correctly produce zero events. Burnban
therefore emits only exactly associated records, treats every stored input
token as full-price input, and marks every Cursor event/report `partial`.
Malformed, oversized, missing, cross-composer, duplicate, or unknown records
are skipped with field-only diagnostics; Burnban never guesses from transcript
content. Cursor billing classification is also absent, so the default is
subscription/API-equivalent and `--metered cursor` requires the operator to
know the window was separately billed. Cursor's networked Admin API is
intentionally not used.

A bounded live smoke against the locally installed legacy Cursor 1.1.7 store
completed read-only and emitted zero sessions/calls with a generic partial
association warning: that database's token rows did not carry enough exact
model/time binding, so zero is the fail-closed result. Current-layout coverage
comes from the inspected official 3.11.13 bundle and adversarial split-key
fixtures; it is not presented as a live customer-database validation.

## OpenCode compatibility

OpenCode support was checked on 2026-07-12 against upstream commit
[`8168f0f`](https://github.com/anomalyco/opencode/commit/8168f0f0f6645a0ca741fe02e90ff532bce04148).
Released builds select `opencode.db` in the `opencode` XDG data directory
(`~/.local/share/opencode/opencode.db` without an XDG override); Burnban honors
OpenCode's absolute or XDG-relative `OPENCODE_DB` override and also exposes
`burnban subsidy --opencode-db`.

The adapter opens the database read-only and supports both upstream message
projections. Its SQLite query extracts only role, provider/model, timestamp,
token, and cost metadata; it never selects prompt, response, reasoning, or tool
content. OpenCode has already separated full-price input from cache reads and
writes. Burnban adds reasoning to output-priced tokens and retains the source's
cost estimate only as a fallback for models missing from Burnban's dated table.

The stored provider and estimated cost do not prove whether a call used a
subscription/OAuth allowance, a free tier, or a billed API key. The adapter
therefore leaves `BillingProvider` empty. Users who can classify the selected
window may opt in with `burnban subsidy --metered opencode`.
