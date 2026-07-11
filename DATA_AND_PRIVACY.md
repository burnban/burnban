# Data and privacy

Burnban is designed to meter locally without a Burnban account or telemetry.
This document describes what the open-source binary reads, stores, and sends.

## Proxy ledger

The default ledger is `~/.burnban/burnban.db`. Burnban creates its directory
with mode `0700` and the database with mode `0600` on operating systems that
support Unix permissions. The ledger stores one row per proxied request:

- timestamp, provider route, model, agent and session identifiers;
- input, output, cache-read, and cache-write token counts;
- calculated cost, latency, HTTP status, streaming and accounting-quality
  states, service tier, inference geography and provider-tool fee indicators;
  and
- a 128-bit truncated HMAC-SHA-256 fingerprint for duplicate-request
  detection. It covers the request body plus provider, method, path, canonical
  query, agent, session and a five-minute time bucket. Its random 256-bit key is
  generated locally and stored in the same database.

Burnban does not persist request or response bodies, provider authorization
headers, API keys, or `BURNBAN_TOKEN`. The keyed fingerprint prevents offline
matching from an exported fingerprint alone, but the local database also holds
the key: someone with the complete ledger and the relevant request context
could still test a small set of candidate bodies for a match. Protect the
database accordingly.

On the first open by a version that uses keyed fingerprints, Burnban clears
legacy unkeyed request hashes written by older prereleases. Those hashes cannot
be safely transformed without the original request bodies. This privacy
migration intentionally removes historical duplicate-receipt grouping while
preserving every request row and all spend/token totals.

Ledger data is retained until the user deletes or replaces the database. There
is currently no automatic retention period. Back up or delete the database with
Burnban stopped so its SQLite WAL and shared-memory files are handled together.

## Local agent usage

The subsidy report and dashboard read supported Claude Code, Codex, Hermes,
OpenClaw, and Goose usage stores in place. They read token/model/session metadata
needed for aggregation and do not modify or upload those stores. Files that a
source tool itself synchronizes remain subject to that tool's privacy policy.

Host-local usage scanning is available only on a local meter (and as synthetic
fixtures in demo mode). A team/network gateway advertises
`local_usage_enabled: false`, returns HTTP 403 from `/api/subsidy`, and does not
poll or display the server operator's local-agent usage panel. This prevents a
remote dashboard user from reading host-user agent history through the gateway.
This host-local scan is disabled whenever Burnban is exposed as a team/network
gateway, so shared-token users cannot inspect the gateway operator's local agent
history. The `burnban subsidy` command remains a local, read-only CLI workflow.

## Network traffic

The proxy forwards requests, including provider credentials, to the configured
model-provider or custom upstream. Burnban does not add a vendor upload endpoint.
Provider traffic is therefore still governed by the selected provider.

The only additional outbound request made by the binary is an optional webhook
configured by the user. Webhook messages contain the budget window, threshold,
spend/cap amounts, reset description, or cap-denial message. Webhook URLs are
stored in the local settings table and redacted in CLI display.

Prometheus metrics, dashboard APIs, exports, and MCP tools expose ledger-derived
metadata. On a network deployment they are protected by the same Burnban token,
but operators must also secure metric collectors, reverse proxies, exports, and
backups.

The network dashboard accepts the shared token through its prompt, keeps it in
that tab's `sessionStorage`, and sends it in the `x-burnban-token` header. It
does not persist the token in local storage or a cookie. Legacy `?token=` values
are removed from the current browser URL/history entry before API requests and
are never accepted as authentication; do not place credentials in URLs or
reverse-proxy logs.

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

On a shared-token gateway, `x-burnban-agent` and `x-burnban-session` are claims
made by the client, not identities authenticated independently by Burnban. Any
client with the shared token can omit or rename those labels. Treat their spend
breakdowns and per-agent caps as cooperative controls, not a security boundary.

## Uninstall and purge

Official installers record owned files in a local install manifest. Normal
uninstall removes the binary, managed launchers, and installer-added PATH entry,
but retains `~/.burnban`. `--uninstall --purge` on macOS/Linux or
`-Uninstall -Purge` on Windows removes the marked default Burnban data directory.
It does not follow a custom `BURNBAN_DB` path; custom databases must be removed
explicitly by their owner. Purge refuses to proceed while a Burnban process is
running; stop the meter first so SQLite cannot recreate WAL files during removal.
