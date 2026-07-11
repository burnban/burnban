# External policy contract

Burnban's MIT meter has a small, vendor-neutral SQLite seam for optional local
sidecars. Anyone may use it to build a private, self-hosted, or commercial sync
system. The open-source binary does not contain a hosted-service URL, upload
client, account system, entitlement check, or vendor protocol.

This document describes external-policy contract version 1. It is a local
integration contract, not a promise that Burnban will operate a remote service
for a third-party sidecar.

## Ledger reads

The default database is `~/.burnban/burnban.db`. Open it with SQLite WAL support,
a busy timeout, and short transactions. Sidecars may read but must never update
or delete rows in `requests`.

The stable aggregate-safe read fields are:

```text
id, ts, model, agent, session
in_tokens, out_tokens, cache_read_tokens, cache_write_tokens, cache_write_1h_tokens
cost_usd, enforcement_unsafe
```

`id` is the monotonically increasing cursor within one ledger. Persist the last
remote acknowledgement outside request history, read `(ack, to]` in bounded
batches, and fail safely if `MAX(id)` falls below an acknowledged cursor. Group
rows locally before upload if the remote service does not need request-level
metadata. A useful privacy-preserving bucket is UTC hour plus model, agent, and
session with summed request, token, cost, and `enforcement_unsafe` counts.

`enforcement_unsafe` marks a successful request whose cost could not be safely
accounted for under an active cap. A shared-budget coordinator must not grant
new headroom from an understated window when this count is nonzero. Model,
agent, and session labels can still identify projects or clients; they are
metadata, not anonymous data.

## Policy writes

Sidecars may write only these rows in `settings`:

| Key | Meaning |
| --- | --- |
| `external_cap_daily_usd` | Absolute device cap for the current UTC day |
| `external_cap_weekly_usd` | Absolute device cap for the UTC week beginning Monday |
| `external_cap_monthly_usd` | Absolute device cap for the current UTC month |
| `external_ban_active` | `1` for an explicit external burn ban; otherwise delete it |
| `external_policy_version` | Monotonic policy/state revision |
| `external_policy_source` | Unique owner chosen by the sidecar |
| `external_policy_updated_at` | RFC 3339 computation timestamp |

Caps are finite, positive USD values and are **absolute local caps**, not
remaining balances. Missing or zero means “unset,” so delete an uncapped key and
never write zero to mean exhausted. For example, if a device has already spent
$18 and may spend $2 more, write `20`, not `2`. Burnban enforces the stricter of
the user's local cap and the external cap. A local one-day override never
bypasses external policy.

Only the cap and ban keys affect admission. Version, source, and timestamp are
cooperative metadata for sidecar ownership and rollback protection; the meter
stores but does not authenticate a remote controller. External policy has no
TTL and remains enforced until its owner replaces or explicitly clears it.

At exact exhaustion, zero cannot be used because it disables the cap. A
coordinator can write the device's already-synced spend, or a tiny positive
floor when that spend is zero. Burnban intentionally permits one request with
no output-token ceiling under any positive cap, so this floor is a guardrail,
not a perfect zero-spend fuse. Use bounded output tokens when exact exhaustion
matters, and reserve `external_ban_active` for an explicit remote ban rather
than ordinary budget exhaustion.

Use a namespaced source such as `org.example.sync:device-123`; the `burnban-`
prefix is reserved for first-party sidecars. Before any usage
leaves the machine, claim `external_policy_source` in a `BEGIN IMMEDIATE`
transaction. Continue only when the row is absent or already equals the same
source. Refuse a different owner; this prevents two coordinators from uploading
the same ledger or overwriting each other's policy.

Apply the complete policy in one transaction. Reject a lower revision and, for
an equal revision, reject an older `external_policy_updated_at`. On unlink,
delete only keys owned by the exact source. Preserve the remote cursor or retain
the source binding so reconnecting cannot replay the ledger.

## Distributed-budget limit

A coordinator that lets several machines spend while offline can provide only
an eventually coordinated guardrail. Two devices can consume the same stale
headroom before either reports it. An exact global ceiling requires fixed
allocations or server-issued grants whose unused portions cannot be reclaimed
from offline devices.

## Security and compatibility

- Protect the database and sidecar credential files as user secrets.
- Use TLS for remote sync; never put bearer credentials in URLs or logs.
- Keep management credentials separate from per-device credentials.
- Bound request/response bodies, dimensions, counters, and batch sizes.
- Treat schema additions as compatible, but depend only on the fields and keys
  listed above for contract version 1.

Burnban's MIT license permits using, modifying, and distributing your own
implementation. The neutral local contract is public; any hosted protocol,
account lifecycle, billing integration, or operational service you build on top
is yours to design.
