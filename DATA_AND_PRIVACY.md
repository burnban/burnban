# Data and privacy

Burnban is designed to meter locally without a Burnban account or telemetry.
This document describes what the open-source binary reads, stores, and sends.

## Proxy ledger

The default ledger is `~/.burnban/burnban.db`. Burnban creates its directory
with mode `0700` and the database with mode `0600` on operating systems that
support Unix permissions. The ledger stores one row per proxied request:

When local policy v2 is active, the ledger also stores a prompt-free admission
decision: policy digest/revision, matched rule explanations, provider/model/
route/tier/geo, self-reported attribution, token/cost bounds, confidence, and
whether the full gateway admission succeeded. It never stores the prompt,
response, authorization headers, or provider credentials. Forwarded request
rows link to that decision; policy-denied and budget-denied attempts can have a
decision without a request receipt.

- timestamp, provider route, model, agent and session identifiers;
- optional signed or self-reported identity provenance: tenant/device,
  principal or service account, project, cost center, and confidence;
- when an explicit downshift rule is considered: requested and chosen
  provider route/model, action, rule, trigger, bounded reason, config digest,
  source/target admission estimates, and a content-free feature summary
  (dialect, token bound, booleans for tools/structured output, and modality
  names). Prompt text, tool names/schemas, upstream URLs, and headers are not
  part of that summary;
- input, output, cache-read, and cache-write token counts;
- calculated cost, latency, HTTP status, streaming and accounting-quality
  states, service tier, inference geography, provider-tool fee indicators,
  and the price source/reference/effective dates/confidence frozen when the
  request was observed;
  and
- a 128-bit truncated HMAC-SHA-256 fingerprint for duplicate-request
  detection. It covers the request body plus provider, method, path, canonical
  query, agent, session and a five-minute time bucket. Its random 256-bit key is
  generated locally and stored in the same database.

Explicit agent/session identifiers are limited to 128 Unicode characters and
256 UTF-8 bytes and reject unsafe control/format characters before forwarding.
Long or unsafe provider-derived model/route/client labels are stored as a
bounded sanitized prefix plus a deterministic hash suffix; full provider model
IDs are still used for pricing before the display value is bounded.

Burnban does not persist request or response bodies, provider authorization
headers, API keys, or `BURNBAN_TOKEN`. The keyed fingerprint prevents offline
matching from an exported fingerprint alone, but the local database also holds
the key: someone with the complete ledger and the relevant request context
could still test a small set of candidate bodies for a match. Protect the
database accordingly.

For an enrolled Personal/Teams identity, the settings table caches the current
server-issued trust grant: public key, device/tenant IDs, authorized
principal/service account, project allow-list, cost center, revision, and
expiry. The proxy stores only the verified attribution fields on a request row,
not the compact proof header or signature. Replay protection stores the public
key ID, a random claim nonce, and its expiry. The sidecar private key is not in
the ledger or control plane; it stays in the separate mode-`0600` enrollment
state file. Claims are signed, not encrypted, so attribution metadata is
visible to processes or network intermediaries that can read the header; use
TLS for a network gateway.

Provider invoice imports are stored in separate immutable tables as normalized
line metadata: provider, non-secret invoice/line identifiers, occurrence time,
USD amount, model, service tier, region, adjustment type/reference, optional
description, source format, import timestamp, and SHA-256 content digest.
CSV imports also store a domain-separated replay digest bound to the complete
effective column mapping; no plaintext invoice cell values or header names are
added to the import record.
Descriptions can contain provider billing metadata; review exports before
sharing them. Imports never update observed request rows. Reconciliation CSV
output neutralizes spreadsheet formulas, and invoice file paths must be stable
regular files rather than symlinks, devices, pipes, or stdin.

External quality imports use a separate immutable table containing only the
source, score ID, observation time, exact model ID, metric, cohort, a caller-
supplied SHA-256 evaluation-case identifier, a fixed-point score, and import
time/content digest. The schema has no prompt, response, comment, dataset row,
raw case ID, session, principal, or arbitrary metadata field. Exact replays are
idempotent; database triggers reject updates and deletes. Cache and allocation
recommendations read a bounded projection of existing token/cost and
agent/project/provider/model/route metadata. They do not add prompt content or
prefix fingerprints. Case hashes remain pseudonymous/linkable and can be
dictionary-tested when the source case ID has low entropy; external evaluators
should derive them from a high-entropy or secret-keyed identifier. Allocation
confidence describes historical statistics, not whether an unsigned scope
label is an authenticated identity. See [`OPTIMIZATION.md`](OPTIMIZATION.md).

The same SQLite database's settings table stores local/external cap values,
velocity-fuse thresholds and cooldown/trip timestamps, warning/alert delivery
marks, and an optional webhook URL. Historical-baseline configuration records
only a UTC slot duration, lookback count, multiplier, and minimum-dollar floor;
the baseline itself is calculated from bounded ledger aggregates. A fuse
incident records only its rule, rolling duration, either dollar or request
limit, projected dollars or requests, and start/end timestamps; it does not add
prompt, response, credential, model, agent, or session content.

On the first open by a version that uses keyed fingerprints, Burnban clears
legacy unkeyed request hashes written by older prereleases. Those hashes cannot
be safely transformed without the original request bodies. This privacy
migration intentionally removes historical duplicate-receipt grouping while
preserving every request row and all spend/token totals.

Ledger data is retained until the user deletes or replaces the database. There
is currently no automatic retention period. Back up or delete the database with
Burnban stopped so its SQLite WAL and shared-memory files are handled together.

## Local agent usage

The usage report and dashboard read supported Claude Code, Codex, Gemini CLI,
GitHub Copilot CLI, Cursor, OpenCode, Hermes, OpenClaw, and Goose usage stores in place.
Their validated adapter manifests require read-only, offline scanning and
metadata-only output. Burnban extracts token/model/session metadata needed for
aggregation and does not modify or upload those stores. Some source stores,
including resumable chat histories, also contain conversation content; their
adapters discard those fields and never put them in a report, ledger,
diagnostic, or adapter event. Files that a source tool itself synchronizes
remain subject to that tool's privacy policy.
The Copilot adapter selects only durable `session.shutdown` aggregates from
`events.jsonl`; it does not decode message, tool, file, or checkpoint events.
The Cursor adapter opens only a regular-file global `state.vscdb` and projects
tightly type- and size-checked role, model, timestamp, token, and source-ID
fields in SQLite. For Cursor's current split-key layout it reads only messages
referenced by ordered composer headers and requires the exact parent, key,
bubble-ID, and role binding. Raw composer and usage IDs are used only inside the
reader to derive fixed one-way deduplication hashes; they are never emitted into
an event, ledger, diagnostic, or report. SQLite returns no global authentication
values, unreferenced-message values, or composer prompt, response, thinking,
file, terminal, diff, or tool fields to the Go scanner. Cursor events remain
partial because the local composer metadata has no trustworthy cache or
reasoning-token decomposition.

The versioned contract and per-adapter privacy declarations are documented in
[`SOURCE_ADAPTERS.md`](SOURCE_ADAPTERS.md). The registry is compiled into the
binary; Burnban does not fetch or execute third-party adapter code at runtime.

Host-local usage scanning is available only on a local meter (and as synthetic
fixtures in demo mode). A team/network gateway advertises
`local_usage_enabled: false`, returns HTTP 403 from `/api/local-usage`, and does not
poll or display the server operator's local-agent usage panel. This prevents a
remote dashboard user from reading host-user agent history through the gateway.
This host-local scan is disabled whenever Burnban is exposed as a team/network
gateway, so shared-token users cannot inspect the gateway operator's local agent
history. The `burnban usage` command remains a local, read-only CLI workflow.

## Network traffic

The proxy forwards requests, including provider credentials, to the configured
model-provider or custom upstream. Burnban does not add a vendor upload endpoint.
Provider traffic is therefore still governed by the selected provider.

Budget-aware downshift is off by default. When the operator explicitly applies
a validated config, Burnban may change only the model selector of a compatible
request and forward it to the exact allowlisted route. OpenAI/Anthropic JSON is
re-encoded after replacing `model`; Gemini changes its canonical model path
segment. The original body is not persisted. Burnban does not store/select a
target credential or translate dialects, prompts, tools, response schemas, or
modalities. Historical simulations and activation/disable reasons are retained
as prompt-free local audit metadata. See [`DOWNSHIFT_ROUTING.md`](DOWNSHIFT_ROUTING.md).

Additional outbound requests exist only when an operator configures a webhook
or explicitly enables OTLP/HTTP export. Webhook messages contain the
budget/fuse window, threshold, spend or projected/cap amounts,
reset/cooldown description, or denial message. Webhook URLs are stored in the
local settings table and redacted in CLI display.

OTLP export is off by default and has no Burnban-operated destination. It sends
only the prompt-free fields allowlisted in [`TELEMETRY.md`](TELEMETRY.md) to the
operator's collector. It never sends prompt/response content, headers,
credentials, URLs/queries, session IDs, or request fingerprints. The collector
endpoint is not stored. A sink-bound delivery/drop checkpoint is stored in
SQLite; the Authorization value is read from a named environment variable at
send time and is neither persisted nor logged. Historical warehouse export is
local-only and produces private, checksum-manifested NDJSON files for the
operator to upload separately.

Prometheus metrics, dashboard APIs, exports, and MCP tools expose ledger-derived
metadata. On a network deployment they are protected by the same Burnban token,
but operators must also secure metric collectors, reverse proxies, exports, and
backups. A gateway started with `--allow-remote-admin` additionally lets any
token holder change guardrail settings (caps, fuses, ban/lift, alerts) from the
dashboard; leave it off when the token is shared more widely than that
authority.

The optional `burnban mcp --allow-budget-requests` capability sends only a
meter-scoped window, requested USD increase, operator-visible reason/ticket,
and expiry to the explicitly configured Burnban Teams URL. It reads the meter
credential from the environment, never persists or prints it, rejects
redirects, and can create only a pending request. All other MCP modes remain
local stdio operations.

The network dashboard accepts the shared token through its prompt, keeps it in
that tab's `sessionStorage`, and sends it in the `x-burnban-token` header. It
does not persist the token in local storage or a cookie. Legacy `?token=` values
are removed from the current browser URL/history entry before API requests and
are never accepted as authentication; do not place credentials in URLs or
reverse-proxy logs.

Explicit `burnban prune` retention applies to policy decision/context rows and
their counter children as well as request receipts. Active and historical
policy documents and settings are preserved.

The dashboard HTML shell itself contains no ledger data and may load before
team authentication so it can prompt for a token. All dashboard JSON, metrics,
and provider routes remain protected. Tokens are sent in headers, not URLs, and
the browser keeps a submitted dashboard token only in tab-scoped session
storage. A legacy `?token=` parameter is rejected and removed from the current
address without being used.

The default loopback listener has no token. Loopback prevents remote network
access but is not an operating-system user boundary: other processes or users
on a shared host may be able to query it or route provider traffic through it.
Set `BURNBAN_TOKEN` on shared or untrusted machines. `status` and `stop` use a
separate ephemeral HTTP control listener on `127.0.0.1` plus a random token in
the lifecycle-state file; the control listener is never advertised as the public
service URL. The state file is mode `0600` on systems with POSIX permissions.
Windows relies on the containing directory ACL, so custom database/state paths
must be private to the Burnban service account.

On a shared-token gateway, `x-burnban-agent`, `x-burnban-session`, and unsigned
team/user/project values are claims made by the client, not identities
authenticated independently by Burnban. Any client with the shared token can
omit or rename those labels. Treat their spend breakdowns and agent-scoped caps
as cooperative controls, not a security boundary.

An optional `X-Burnban-Identity` proof authenticates an enrolled device's
server-authorized principal/service-account/project/cost-center mapping. It is
consumed locally and stripped with all Burnban attribution headers before the
provider request. It authenticates possession of a software key, not which
same-user process or human initiated the request. The full online/offline and
host-compromise boundary is documented in
[`SIGNED_IDENTITY.md`](SIGNED_IDENTITY.md).

## Uninstall and purge

Official installers record owned files in a local install manifest. Normal
uninstall removes the binary, managed launchers, and installer-added PATH entry,
but retains `~/.burnban`. `--uninstall --purge` on macOS/Linux or
`-Uninstall -Purge` on Windows removes the marked default Burnban data directory.
It does not follow a custom `BURNBAN_DB` path; custom databases must be removed
explicitly by their owner. Purge refuses to proceed while a Burnban process is
running; stop the meter first so SQLite cannot recreate WAL files during removal.
