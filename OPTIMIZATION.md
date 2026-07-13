# Content-free optimization and external quality evidence

Burnban's optimization layer reads request metadata already present in the
local ledger. It does not persist prompt/response bodies, prompt prefixes,
embeddings, evaluation datasets, comments, or raw evaluation-case IDs.

## Cache-shaping receipts

```sh
burnban optimize cache --since 30d
burnban optimize cache --since 7d --format json --large-context 32000 --low-reuse 20
```

The detector groups provider/model/route metadata under a project (or agent
when no project is present). It reports a pattern only after the configured
number of large-input calls and a cache-read ratio below the configured
threshold. Every receipt contains observed token counts, dates, confidence,
and a provider-neutral action: check whether that provider/model supports
prompt caching, place reusable instructions/tool definitions before
request-specific context, configure the provider's documented TTL, and then
measure again.

This evidence cannot reveal prefix stability. A low cache-read ratio can also
mean that caching is unsupported, disabled, expired, or uneconomical. Burnban
therefore reports `prefix_stability: "unobserved"` and never invents a savings
amount. It does not store a prefix fingerprint as a substitute for prompt
content.

Queries cover at most 90 days and 100,000 ledger rows. When `--max-rows` is
reached, the JSON receipt says `truncated: true`, covers only the most recent
bounded sample, and lowers confidence.

## Allocation proposals

```sh
burnban optimize allocation --dimension agent --days 30
burnban optimize allocation --dimension project --days 30 --format json
burnban optimize allocation --dimension meter --days 30 --format json
burnban optimize allocation --dimension team --days 30 --format csv
burnban optimize allocation --dimension agent --days 14 --format csv > proposals.csv
```

For each attributed scope, Burnban computes:

- average daily historical spend;
- the configured daily percentile (p90 by default);
- seven-day spend velocity;
- a proposed daily budget above the larger of velocity and percentile (20%
  headroom by default);
- a relative demand weight; and
- request-by-request historical blocked-call count and spend if that proposed
  daily limit had been active.

The replay uses complete UTC days and recorded costs normalized to exact
micro-dollar integers in timestamp/ledger order. It cannot model concurrent
reservations, different
future traffic, model/tokenizer changes, or quality. Unknown-price calls are
counted as exclusions; partial/enforcement-unsafe calls lower confidence.
Unsafe or unbounded legacy scope labels are excluded rather than merged.
Confidence is statistical confidence in the sampled spend pattern, not proof
that an agent/project label is authenticated. Unsigned labels remain
cooperative attribution; use signed identity when that boundary matters.

Meter and team proposals use the signed identity device and cost-center fields,
respectively. Self-reported, unverified, and unattributed rows are excluded and
counted rather than allowed to influence fleet weights.

Allocation commands never write cap settings or policy documents. Agent
JSON/CSV rows include a POSIX-shell-quoted command for an operator to review;
project rows point to an
explicit, versioned Policy v2 change. If the bounded row limit is reached,
Burnban emits no allocation proposal because the blocked-call simulation is
incomplete.
Meter/team rows are likewise review evidence for a Teams operator; they never
mutate meter weights, organization budgets, or policy documents automatically.

CSV output neutralizes spreadsheet formula prefixes in metadata fields.

## External quality/outcome evidence

Burnban accepts a narrow, immutable score envelope from an existing evaluation
or outcome system. It is an integration point, not an evaluation framework.

```json
{
  "schema": "burnban.external-quality/v1",
  "source": "braintrust",
  "metric": "task_success",
  "cohort": "release-2026-07",
  "direction": "higher_is_better",
  "scores": [
    {
      "id": "score-018f",
      "observed_at": "2026-07-12T12:00:00Z",
      "model": "gpt-example",
      "case_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "score": "0.875"
    }
  ]
}
```

`case_hash` is a lowercase SHA-256 digest created by the external evaluator
from a stable, non-content case identifier. Do not send the raw identifier or
case content. The digest is pseudonymous, not anonymous: repeated values are
linkable within exported/backed-up ledger data and low-entropy raw IDs may be
guessable offline unless the evaluator uses a secret-keyed or high-entropy
identifier before SHA-256. `score` is an exact decimal string from 0 through 1
with at most six fractional digits. The v1 contract accepts only
`higher_is_better`; invert
or normalize a lower-is-better metric in the source system and give that
transformation a distinct metric name.

The schema allows only source, metric, cohort, model, timestamp, score ID,
case hash, and numeric score. Unknown, duplicate, or case-ambiguous JSON fields
fail closed. An import contains at most 10,000 scores and 8 MiB.

Import a permission-controlled regular file:

```sh
burnban optimize quality-import --file scores.json
```

Or POST the same JSON to a running, authenticated Burnban gateway:

```sh
curl --fail-with-body \
  -H 'content-type: application/json' \
  -H "x-burnban-token: $BURNBAN_TOKEN" \
  --data-binary @scores.json \
  http://127.0.0.1:4141/api/quality-scores
```

The endpoint is protected by the same Burnban token and local-origin safety as
other `/api/` routes. Network/team deployments must use authentication. An
exact replay of `source + id` succeeds idempotently. Reusing that identity with
different evidence, or submitting a second score for the same
source/metric/cohort/model/case, returns a conflict. Database triggers reject
updates and deletes; deleting the whole local database remains the operator's
explicit retention boundary.

### Quality-constrained what-if

```sh
burnban whatif --since 30d \
  --quality-source braintrust \
  --quality-metric task_success \
  --quality-cohort release-2026-07 \
  --min-quality 0.85 \
  --quality-min-samples 10 \
  --quality-min-coverage 0.8
```

Quality flags are all-or-nothing. Burnban includes a candidate model only when
its exact model ID has at least the requested number of externally scored cases,
meets the score threshold, and covers the requested fraction of distinct case
hashes observed within the same `--since` window in that source/metric/cohort.
Candidates without evidence are excluded rather than assigned an inferred
quality. Cost repricing still assumes
the same token counts and cannot predict tokenizer or verbosity changes.

The external evaluator remains responsible for cohort comparability, score
validity, and whether its metric represents acceptable production quality.
Burnban does not infer target quality and deliberately does not build datasets,
judges, prompt tracing, or a playground.
