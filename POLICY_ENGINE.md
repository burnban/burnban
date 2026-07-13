# Local policy engine v2

Burnban policy v2 adds deterministic metadata policy to the existing dollar
caps, burn ban, and spend-velocity fuse. It does not replace or weaken those
guards: a request must pass every active guard before Burnban forwards it.

Policies are strict JSON. Generate a starting point with:

```sh
burnban policy templates starter > policy.json
burnban policy validate policy.json
burnban policy simulate --since 7d policy.json
burnban policy apply --db ~/.burnban/burnban.db policy.json
burnban policy show
burnban policy coverage --since 7d --stale-after 24h
```

`apply` is atomic and revisioned. A lower revision, or different content that
reuses the active revision, is rejected. `metadata.namespace` is the stable
counter identity. A normal apply rejects changing it, because doing so would
silently reset rolling and fixed counters. Start another namespace only through
an explicit lineage reset/unlink procedure:

```sh
burnban policy events --limit 100
burnban policy reset --confirm-digest <sha256> --confirm-source local --reason "retire test counters"
burnban policy takeover --confirm-digest <sha256> --confirm-source <external-owner> --reason "approved ownership transfer"
```

`reset` accepts only a locally owned active policy. `takeover` is the separate,
deliberate operation for an externally owned policy. Both retain immutable
documents and append an immutable lineage event with actor, reason, prior
source, namespace, and digest.

Burnban Teams can distribute the same canonical document in a versioned,
SHA-256-addressed envelope. The connector independently validates the full
schema, envelope metadata, digest, and timestamp and atomically activates it
with the downloaded absolute caps. A Team-owned active document cannot be
overwritten by a local apply; rollback, same-revision mutation, namespace
changes, and a different external owner fail closed. Outages and invalid
responses retain the last active document. Teams-side SCIM groups are expanded
at publication into exact signed-user selectors rather than adding an
unauthenticated group header to this language.

`burnban policy templates --list` includes six role-specific starting points:
`individual-coding-agent`, `ci-review-bot`, `autonomous-research-agent`,
`production-customer-app`, `local-private-model`, and
`unknown-price-sandbox`. The production template begins in `warn` mode for a
staged rollout. The local and unknown-price templates deliberately rely on
request, token, concurrency, and output-bound controls instead of pretending a
missing dollar price is zero. Templates are examples to simulate and revise,
not universal capacity recommendations.

## Document shape

```json
{
  "apiVersion": "burnban.dev/v2",
  "kind": "PolicySet",
  "metadata": {
    "name": "developer-workstation",
    "namespace": "workstation-policy",
    "revision": 1
  },
  "mode": "enforce",
  "rules": [
    {
      "id": "provider-and-rate-boundary",
      "scope": {"provider": ["openai", "anthropic"]},
      "match": {
        "model": {"deny": ["*-preview"]},
        "route": {"allow": ["/v1/*"]},
        "tier": {"allow": ["", "default", "standard"]},
        "geo": {"allow": ["", "global", "us"]}
      },
      "limits": {
        "requests": [
          {"id": "rpm", "max": 60, "window": "1m", "window_type": "rolling"}
        ],
        "tokens": [
          {"id": "input-hour", "kind": "input", "max": 700000, "window": "1h", "window_type": "fixed"},
          {"id": "output-hour", "kind": "output", "max": 300000, "window": "1h", "window_type": "fixed"},
          {"id": "total-day", "kind": "total", "max": 4000000, "window": "24h", "window_type": "rolling"}
        ],
        "dollars": [
          {"id": "spend-day", "max_microusd": 25000000, "window": "24h", "window_type": "rolling"}
        ],
        "concurrency": 4,
        "max_estimated_call_cost_usd": 0.5,
        "require_output_bound": true
      }
    }
  ]
}
```

Scope dimensions are `organization`, `tenant`, `meter`, `device`, `team`,
`cost_center`, `principal`, `service_account`, `user` (compatibility alias),
`project`, `environment`, `agent`, `session`, `provider`, `model`,
`model_class`, `route`, `tier`, `service_tier`, `geo`, and `inference_geo`.
Dimensions are ANDed; selectors within a dimension are ORed. `*` matches any
sequence and `?` one Unicode code point.

Every applicable rule is intersected. There is no override order: one
applicable enforcing deny or exceeded limit denies the request. Explanations
are ordered by scope specificity and rule ID, so the same input and ledger
state produce the same explanation.

Provider, model, model class, route, tier/service tier, and geo/inference geo
have allow/deny match lists. Deny wins inside one dimension; a nonempty allow
list requires a match. Rule `mode` overrides the document mode:

- `observe` records violations without a response header or block;
- `warn` records them and adds `X-Burnban-Policy-Warn: true`;
- `enforce` rejects before upstream forwarding.

## Identity trust boundary

Unsigned attribution headers are local, self-reported metadata. Burnban strips
them before forwarding, but they are not an authorization boundary.

An enrolled Personal or Teams sidecar can issue a short-lived
`X-Burnban-Identity` proof whose device key, attribution, audience, route, raw
query, body, expiry, and one-time nonce the OSS proxy verifies. A verified cost
center supplies `team` and `cost_center`; the signed tenant supplies
`organization` and `tenant`; the enrolled device supplies `meter` and `device`;
principal and service-account claims remain distinct while also populating the
compatibility `user` field; the grant supplies `environment`; and only a
project named exactly in the server grant supplies trusted `project`. A
wildcard-granted signed project remains `self_reported`. These dimensions have
independent confidence in the policy context. If an enforcing rule sees a
self-reported or omitted scoped dimension, Burnban applies the rule while
ignoring only that untrusted selector and returns 401
`authenticated_identity_required`. Other exact trusted dimensions continue to
match normally. This prevents a caller from bypassing a scoped rule by
relabeling or omitting itself without broadening the rule across another
authenticated team or user. Signed and unsigned override headers cannot be
combined.

Agent, session, and model-class labels are cooperative rather than
authenticated, so enforcing scopes over those fields (and enforcing
model-class matches) are rejected at policy compile time; use `warn` or
`observe`. Provider, exact model, route, tier, service tier, and geography
enforcement does not depend on signed identity. See
[device-bound signed identity](SIGNED_IDENTITY.md)
for issuance, trust expiry, rotation/revocation, and its host-compromise limit.

## Atomic limits and admission order

Request, kind-specific token, integer micro-USD, and concurrency capacity is
reserved under one policy-engine lock before forwarding. Burnban then evaluates
its existing dollar guard. A two-phase commit charges durable window counters
only if the dollar guard also admits the request; a budget-denied attempt cannot
exhaust a policy window. Concurrency is released on every exit path.

Input-token windows charge only the conservative request-body input upper bound
and do not require an output bound. Output and total windows require the
caller-provided finite maximum output tokens and fail closed in `enforce` mode
when it is absent. Dollar windows use `max_microusd` integer limits and the
conservative priced request bound; unknown, incomplete, or internally
inconsistent pricing fails closed rather than becoming zero spend.

After complete gateway admission, request counters and conservative input,
output, total-token, and micro-USD bounds are charged permanently. They are not
settled downward to eventual provider usage. A budget-denied or cancelled
admission does not charge these windows. Decision records expose
`window_accounting: admission_requests_and_conservative_bounds` so exports and
simulations cannot mistake reservation bounds for actual usage.

Rolling windows look backward by the configured duration. Fixed windows are
UTC epoch-aligned duration buckets. Counters are keyed by stable policy
namespace and rule ID and survive policy display-name changes and revisions.
The normal single `serve` lease is the one-machine writer boundary.

## Decision records, export, simulation, and retention

Each evaluation writes a durable decision containing policy digest/revision,
matched rules, violations, bounded request metadata, confidence, policy
outcome, and whether the complete gateway admission succeeded. Prompts,
responses, authorization headers, and provider credentials are never included.
Forwarded request rows link to their decision. Both JSON and CSV export include
that linked metadata, and MCP exposes the read-only `policy_status` tool.

`burnban policy simulate` replays a candidate against historical request
metadata without changing live policy or counters. Rows written by policy v2
contain the original admission estimate. Legacy rows use actual ledger
input/output/cache-read/cache-write/one-hour-cache-write tokens and cost as a
conservative proxy, cannot prove an output bound, and are reported as
partial/indeterminate rather than silently treated as exact. Replay evaluates
input/output/total token kinds and fixed/rolling micro-USD windows with the same
boundary and saturation semantics as live admission. Reconstructed concurrency
is labeled estimated. Text and JSON reports state how many calls would block,
warn, or produce observe-only impact, plus bounded affected agent, user, model,
and project breakdowns. When multiple rules violate on the same call, the
report groups the interacting rule IDs and modes and explains the resolved
outcome. All applicable rules intersect: enforce wins over warn, warn wins over
observe, and specificity orders explanations without granting an override.
Breakdown and interaction lists are deliberately bounded and explicitly marked
when limited so attacker-controlled historical labels cannot inflate a report
without bound.

`burnban policy coverage` reports the metadata-only fleet-health pipeline:
whether policy is configured, traffic is routed, decisions are observed, and
any rule is enforcing; evaluated versus uncovered routed requests; trusted,
self-reported, and unverified identity counts; and external-policy freshness.
It separately reports project trust as `none`, `self_reported_wildcard`,
`exact_allowlist`, or `mixed`, including the number of exact project grants.
A local policy document remains valid until explicitly replaced and is not
called stale merely because it is old. `--stale-after` applies to the
coordinator-owned `external_policy_updated_at` heartbeat. If immutable provider
invoice evidence has been imported, unmatched provider lines are surfaced as a
possible bypass signal. They are not proof: metadata mismatches and delayed
billing can also leave lines unmatched, while provider traffic that never
passes Burnban is otherwise invisible to a local proxy.

`burnban prune` removes old request rows and policy decisions (including their
rule-counter children) in bounded transactions. Active and historical policy
documents, caps, and settings are preserved. This keeps the explicit retention
contract true for identity and explanation metadata as well as cost receipts.

## Request hardening

Policy matching and upstream forwarding use one canonical route. Encoded
slashes, dot segments, control characters, backslashes, and ambiguous raw paths
are rejected. JSON requests reject duplicate keys, ASCII case-colliding keys,
and noncanonical spellings of security fields such as `model`, `max_tokens`,
`tools`, service tier, and geo. This prevents Burnban and an upstream parser
from applying policy to different interpretations of the same bytes. Malformed
typed security fields are rejected rather than treated as absent. Provider,
model, canonical route, service tier, and geography admission values must each
be valid UTF-8 of at most 4 KiB. Policy matches their complete bounded values,
with unsafe formatting code points normalized and a deterministic hash suffix,
before applying the separate 256-byte display-label bound used by durable
receipts. A deny suffix therefore cannot disappear behind receipt truncation.

Custom upstream URLs cannot contain userinfo. Transport failures return a
generic client error and logs omit upstream path/query data, so credentials in
an operator-configured base query are not reflected.
