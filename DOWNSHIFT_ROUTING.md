# Budget-aware downshift routing

Burnban can replace an exact source model with an explicitly allowlisted,
wire-compatible cheaper model when a dollar budget approaches its limit. It is
off by default. It is not a general fallback router, model alias, key vault, or
dialect translator.

Downshift runs after Policy v2 evaluates the request that the client actually
asked for. A policy denial is final. For a policy-allowed request, Burnban then
checks compatibility, resolves the target's current dated/contract price, and
uses that target cost for budget admission. Burn bans, metering-gap failures,
and velocity-fuse incidents are never bypassed.

## Safe rollout

Start with `observe`, inspect historical impact, and then move to
`warn_then_downshift` with a higher revision:

```sh
burnban downshift validate downshift.json
burnban downshift simulate --since 30d downshift.json
burnban downshift apply --since 30d downshift.json
burnban downshift show
```

`apply` runs and durably records a historical replay for the exact config
digest. Enforcing activation requires at least one matched, compatible request
whose source and target costs can both be quantified. Old rows without a
content-free feature receipt are reported as indeterminate, never assumed safe.

A new installation may have no usable history. An operator can activate only
after an explicit review and durable explanation:

```sh
burnban downshift apply --force \
  --force-reason "Reviewed model contract and canary tests; this new gateway has no historical receipts" \
  downshift.json
```

The force reason, config digest, revision, and timestamp are appended to the
local audit. Disable is also explicit and audited:

```sh
burnban downshift disable --reason "Canary quality regressed; return all traffic to requested models"
```

## Version 1 configuration

```json
{
  "api_version": "burnban.downshift/v1",
  "revision": 1,
  "mode": "observe",
  "warn_at_pct": 70,
  "downshift_at_pct": 80,
  "downshift_on_denial": true,
  "rules": [
    {
      "id": "coding-safe",
      "source": {
        "route": "openai",
        "model": "gpt-5",
        "family": "coding",
        "dialect": "openai",
        "context_tokens": 200000
      },
      "target": {
        "route": "vllm",
        "model": "gpt-5-mini",
        "family": "coding",
        "dialect": "openai",
        "context_tokens": 128000
      },
      "scope": {
        "project": "oss"
      },
      "capabilities": {
        "tools": true,
        "structured_output": true,
        "modalities": ["text"]
      }
    }
  ]
}
```

Every source route/model is exact and may appear once. Source and target must
declare the same exact workload family and dialect. Family is an operator-owned
equivalence assertion; Burnban cannot infer that two models have equal quality.
Revisions only move forward and cannot be reused for different content.

The parser rejects unknown, duplicate, case-ambiguous, non-finite, oversized,
control-character, and bidi-control fields. Config files must be stable regular
files; symlinks and devices are rejected. The config contains route names, not
upstream URLs, credentials, or headers.

## Per-request compatibility gate

An allowlisted mapping is still ineligible unless Burnban can prove all of the
following before forwarding:

- the canonical operation is exactly OpenAI `/v1/chat/completions`, Anthropic
  `/v1/messages`, or Gemini `generateContent`/`streamGenerateContent`;
- the live source and target routes use the configured, identical OpenAI,
  Anthropic, or Gemini dialect;
- the request has an explicit positive output-token bound;
- a conservative one-byte-per-input-token bound plus output ceiling fits both
  declared context windows;
- every requested modality is allowlisted on the target;
- structured output is allowlisted when a JSON/object/schema response is
  requested;
- client function-tool descriptors and JSON schemas are well formed, and the
  target is allowlisted for tools;
- no provider-hosted search, retrieval, code execution, MCP, asset generation,
  or unknown future tool is present;
- every configured identity scope matches authenticated device-bound identity.
  Self-reported team/user/project headers cannot select a scoped mapping;
- the target route exists, its price is known, its request cost is bounded, and
  at a utilization threshold its cost bound is actually lower than the source.

Unknown fields in the provider request are relayed normally when no rewrite is
needed, but any feature Burnban cannot classify makes that request ineligible
for downshift. The safe result is the requested model or the original budget
denial—not a guess.

`warn_then_downshift` emits an eligibility warning at `warn_at_pct`, rewrites at
`downshift_at_pct`, and may retry a compatible target when the source request's
known cost bound exceeds remaining calendar-budget headroom. It does not retry
burn bans, an already-tripped fuse, accounting gaps, or persistence failures.
Concurrent retries pass through the same in-flight target-cost reservation as
ordinary admissions.

## Local Ollama and vLLM

The built-in `ollama` and `vllm` routes are eligible targets only when named in
a rule. A custom local endpoint can be registered explicitly:

```sh
burnban serve --upstream lab-vllm=http://127.0.0.1:8000
```

Then use `"route":"lab-vllm"` and `"dialect":"openai"` in the target. Add an
exact target rate—or `{ "free": true }`—to `~/.burnban/pricing.json`; an
unknown target price cannot be admitted under an active dollar guardrail.

For a same-route rewrite, Burnban preserves the caller's provider headers. A
cross-route rewrite instead strips source credentials and every locally
consumed header, then forwards only `Accept`, `Content-Type`, `User-Agent`, and
the dialect-required `Anthropic-Version` header. Any other caller header or any
query parameter makes the request ineligible for cross-route downshift.
Burnban never stores, selects, or injects a target credential, so cross-route
targets must accept that deliberately minimal request contract; explicitly
configured local endpoints commonly require no credential. Burnban does not
translate tool schemas, prompts, response formats, paths across dialects, or
response bodies.

For supported OpenAI chat-completions and Anthropic messages shapes the explicit top-level `model` value is changed
and the JSON object is re-encoded without changing semantic fields. For Gemini,
only the canonical `/models/{model}:operation` path selector is changed. SSE is
relayed and metered with the chosen route's unchanged dialect.

The OpenAI Responses API, embeddings, image/audio generation, batches, files,
and arbitrary custom POST operations remain pass-through in version 1 even if
they contain an allowlisted model. Their request contracts are intentionally
not inferred from chat-completions fields.

## Evidence and exports

When a rule is considered, responses carry gateway-owned headers:

- `X-Burnban-Downshift-Action`: `none`, `warn`, or `downshift`
- `X-Burnban-Downshift-Rule` and `X-Burnban-Downshift-Trigger`
- `X-Burnban-Requested-Route` / `Model`
- `X-Burnban-Chosen-Route` / `Model`
- `X-Burnban-Downshift-Reason` and config digest

Conflicting upstream headers are removed before these values are returned.
Reasons contain bounded route/model metadata, never an upstream URL or secret.

The append-only request receipt stores requested and chosen route/model,
action, rule, trigger, exact reason, config digest, source/target admission
costs, and a content-free feature summary. It never stores prompt text, a tool
name/schema, response content, request headers, or an upstream URL. The fields
are present in JSON/CSV finance export and the optional metadata-only OTLP and
warehouse event. Historical simulation uses those feature receipts and
observed token totals; it excludes incomplete evidence rather than filling it
with optimistic assumptions.

## Boundaries

Downshift reduces an estimated token bill; it does not prove equal answer
quality. Use workload-specific canaries and external quality evidence before
asserting an equivalence family. Provider-side routing, hidden fees, a model
that ignores output limits, or a compromised local OS user remain outside the
guarantee. A provider's final inclusive charge still wins during settlement,
and any resulting accounting gap makes the active dollar guardrail fail closed.
