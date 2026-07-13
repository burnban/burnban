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

Burnban clamps negative counters, defaults calls to one, deduplicates non-empty
event IDs within an adapter, and owns all final pricing and aggregation.
Adapters must not turn missing values into zero-cost certainty.

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

The built-in manifests and the Gemini CLI/OpenCode compatibility fixtures
exercise the same public v1 contract used by new adapters.

Gemini CLI's session record does not identify whether API-key or Vertex traffic
was on a free or paid tier. Its adapter deliberately leaves `BillingProvider`
empty; a user who knows the window was billed can opt in with
`burnban subsidy --metered gemini-cli`.

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
