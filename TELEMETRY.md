# OpenTelemetry and warehouse export

Burnban does not send telemetry by default. Operators can opt a running meter
into metadata-only OTLP/HTTP export, or create a bounded historical warehouse
dataset on disk. Neither path sends prompts, responses, request or response
headers, provider credentials, provider-request URLs or query strings, session
identifiers, or request fingerprints. Public pricing URL references are
reduced to their origin before export.

## Live OTLP/HTTP export

Set an endpoint explicitly when starting the meter:

```sh
export BURNBAN_OTLP_AUTHORIZATION='Bearer collector-token'
burnban serve \
  --otlp-endpoint https://otel.example.com:4318 \
  --otlp-auth-env BURNBAN_OTLP_AUTHORIZATION
```

`BURNBAN_OTLP_ENDPOINT` can provide the endpoint instead of the flag. Burnban
appends `/v1/traces` and `/v1/metrics`; a supplied endpoint already ending in
one of those paths is normalized to the common base. The exporter uses the
standard OTLP/HTTP JSON protobuf mapping and sends `Content-Type:
application/json`.

Remote endpoints must use HTTPS. Plain HTTP is accepted only for an explicit
loopback address or `localhost`, which supports a host-local collector. Public
IP addresses are allowed by default. RFC 1918/ULA addresses require
`--otlp-allow-private-network`; link-local, unspecified, and multicast ranges
are always refused. Shared CGNAT, protocol-assignment, documentation,
benchmarking, translation, and reserved address blocks are also refused;
the private-network option does not turn those special-use ranges into valid
collector destinations. DNS answers are checked again at connection time to
reduce DNS-rebinding and metadata-service SSRF risk. Environment proxy settings
are ignored, redirects are not followed, endpoint credentials/query strings
are rejected, and TLS 1.2 is the minimum version.

The value named by `--otlp-auth-env` is read at delivery time and placed only
in the `Authorization` header. The value is never written to SQLite or logs.
Put the complete header value, such as `Bearer ...`, in that environment
variable. Do not place credentials in the endpoint.

The exporter is asynchronous. It polls already-committed SQLite receipts and
has no callback on the provider request path. Collector failure therefore
never blocks or rejects inference. Delivery is at least once. Sink-bound trace
and metric cursors advance independently after each request reaches a terminal
OTLP response and the new cursor is durably written. This lets a lagging signal
retry without resending a signal that was already accepted or partially
accepted. A collector can still see a duplicate when delivery succeeds but the
local cursor write fails.

`--otlp-max-backlog` bounds pending receipts without creating a second disk
queue. If collector downtime exceeds that bound, the oldest pending receipts
are recorded on a separate dropped cursor, logged, exposed in the private
control status, and included in the next `burnban.telemetry.dropped` metric.
That gauge and `dropped_rows` count only rows discarded by this local backlog
bound. They are never labeled as delivered. An OTLP partial-success response
with a non-zero rejection count is terminal for that signal and is not retried,
as required by the protocol; exact rejected span and metric-point totals are
stored separately. An empty `partial_success` is full success, while a zero
count with a message is a collector warning on a fully accepted request.
Retryable failures use bounded exponential backoff with jitter and honor a
bounded `Retry-After` value.

### Signal schema

Each receipt becomes one `CLIENT` span named from the GenAI operation and
model. Burnban emits the current GenAI semantic-convention keys where it has
the required metadata, including:

- `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`,
  `gen_ai.request.stream`, `gen_ai.usage.input_tokens`, and
  `gen_ai.usage.output_tokens`;
- `gen_ai.client.token.usage` and
  `gen_ai.client.operation.duration` delta histograms; and
- `burnban.*` attributes for agent, trusted identity/project/cost center,
  route, cache-token dimensions, cost/source/confidence, accounting quality,
  policy decision/version/digest/mode, status, retry count, and downshift. A
  routing receipt includes requested/chosen route and model, rule, trigger,
  bounded reason, config digest, and source/target admission estimates; it
  never includes an upstream URL, credential, prompt, or tool schema.

Identity attributes carry `burnban.identity.confidence`. Only an authenticated
principal or service-account claim is copied to
`burnban.identity.trusted_principal`; legacy client headers remain
`self_reported` or `unverified`. Empty retry/downshift fields are retained in
the warehouse contract for forward-compatible recommendation workflows.

Burnban intentionally never emits the opt-in OpenTelemetry content attributes
`gen_ai.input.messages`, `gen_ai.output.messages`, or
`gen_ai.system_instructions`. The transport follows the [OTLP
specification](https://opentelemetry.io/docs/specs/otlp/) and the signal names
follow the [OpenTelemetry GenAI semantic
conventions](https://github.com/open-telemetry/semantic-conventions-genai).

## Historical warehouse/object batches

Create an immutable, object-storage-ready dataset without enabling any network
export:

```sh
burnban telemetry export \
  --since 30d \
  --out ./exports \
  --batch-rows 1000 \
  --max-rows 100000 \
  --max-bytes 268435456
```

The command creates a new private directory rather than overwriting an existing
dataset:

```text
burnban-20260712T120000Z-0123456789abcdef/
├── manifest.json
└── date=2026-07-12/
    └── hour=11/
        └── part-000001.ndjson
```

Rows use the versioned `burnban.telemetry.v1` JSON contract. Each manifest
object records row count, byte count, time bounds, and SHA-256. Files are mode
`0600` and directories mode `0700` where POSIX permissions apply. A staging
directory is removed on error and atomically renamed only after all objects
and the manifest have been flushed and synced. `--max-rows`, `--max-bytes`,
and `--batch-rows` bound disk and memory use; exceeding a bound publishes
nothing.

The output directory itself may not be a symlink. Treat the dataset as
sensitive operational metadata even though it is content-free: agent, project,
principal, model, route, and cost fields can reveal work patterns. Upload the
completed directory with the object-store tool and credentials your
organization already manages. Burnban deliberately does not embed a cloud SDK
or persist cloud credentials.

[`warehouse/schema.json`](warehouse/schema.json) is the canonical row schema,
[`warehouse/schema.sql`](warehouse/schema.sql) is a portable typed-table
starting point, and [`warehouse/dbt`](warehouse/dbt) contains a minimal dbt
source/staging contract. NDJSON was chosen over Parquet so the core binary does
not acquire a large native/columnar dependency; downstream warehouses can
convert objects after checksum verification.
