# Pricing provenance and invoice reconciliation

Burnban keeps two records separate:

1. the immutable request ledger, containing the amount and pricing evidence
   available when the request was observed; and
2. immutable provider-invoice imports and reconciliation adjustments.

An invoice import never updates `requests.cost_usd`. Reports compare the two
records so a later invoice, credit, or batch correction cannot rewrite what the
meter knew during enforcement.

## Price resolution

Every new request records `cost_source`, `cost_source_ref`,
`cost_effective_from`, `cost_valid_through`, and `cost_confidence`. Resolution
is deterministic:

1. an inclusive final amount reported by the configured provider/accounting
   gateway in `X-Burnban-Provider-Final-Cost-USD`;
2. the most-specific active customer contract override matching provider,
   model, region, and service tier;
3. the active Burnban public list entry; or
4. `unknown`.

Malformed or duplicate final-cost headers fail closed as unknown; they never
fall back to a cheaper layer. A region-scoped contract rate is treated as the
effective regional rate and is not multiplied by a public-list geo surcharge
again. Equal-specificity overlapping contracts are rejected at startup.

Customer contracts belong in the permission-controlled
`~/.burnban/pricing.json` overlay:

```json
{
  "contracts": [
    {
      "id": "msa-2026-openai-us-priority",
      "provider": "openai",
      "model": "gpt-5.4",
      "region": "us",
      "service_tier": "priority",
      "effective_from": "2026-07-01",
      "valid_through": "2026-12-31",
      "price": {
        "input_per_mtok": 1.1,
        "output_per_mtok": 6.5,
        "cache_read_mult": 0.1,
        "cache_write_mult": 0
      }
    }
  ]
}
```

Contract IDs are persisted as provenance. Use a non-secret internal reference,
not contract text, credentials, or customer PII. Public model overrides and
contracts may coexist in the same file. The overlay is bounded to 1 MiB and
must resolve to one stable regular file while Burnban reads it; stable symlinks
remain supported for dotfile managers. Pricing provenance URLs must be bounded
visible-ASCII HTTP(S) URLs without userinfo, query parameters, or fragments
because the winning source reference is retained in ledger and finance
exports.

## Import formats

Imports are bounded to 32 MiB, 100,000 rows, 64 columns, and 16 KiB per field.
They accept USD decimal strings with at most six fractional digits and reject
exponents, non-finite values, duplicate headers/IDs/JSON keys, unsafe Unicode,
ambiguous CSV widths, and overflowing totals. Invoice paths must be regular
files; symlinks, devices, pipes, directories, and stdin are refused so the
stored content hash corresponds to stable replay evidence.

The canonical CSV requires these fields:

```csv
line_id,occurred_at,billed_usd,model,service_tier,region,line_type,reference_line_id,description
line-1,2026-07-01T00:00:00Z,12.345678,gpt-5.4,priority,us,usage,,July usage
credit-1,2026-07-15T00:00:00Z,-0.500000,gpt-5.4,priority,us,credit,line-1,Delayed credit
```

`line_type` defaults to `usage`. Other immutable adjustment types are
`delayed`, `credit`, `batch`, `tax`, and `fee`; credits must be negative and
usage must be non-negative.

The provider presets are explicit mappings, not a claim that a vendor's export
schema will never change:

| Preset | ID | Time | Amount | Region |
|---|---|---|---|---|
| `canonical` | `line_id` | `occurred_at` | `billed_usd` | `region` |
| `openai` | `id` | `usage_start_time` | `cost_usd` | `region` |
| `anthropic` | `id` | `usage_start_time` | `cost_usd` | `inference_geo` |
| `gemini` | `id` | `usage_start_time` | `cost_usd` | `location` |

Map an export revision without changing code:

```sh
burnban reconcile import \
  --file ./provider-invoice.csv \
  --invoice inv_2026_07 \
  --provider openai \
  --format openai \
  --columns 'line_id=line_item_id,occurred_at=start_time,billed_usd=amount_usd'
```

Canonical API JSON uses schema `burnban.invoice/v1`; money remains a string:

```json
{
  "schema": "burnban.invoice/v1",
  "invoice_id": "inv_2026_07",
  "provider": "openai",
  "currency": "USD",
  "lines": [
    {
      "line_id": "line-1",
      "occurred_at": "2026-07-01T00:00:00Z",
      "billed_usd": "12.345678",
      "model": "gpt-5.4",
      "line_type": "usage"
    }
  ]
}
```

Import that payload with `--input json`. The same provider/invoice identity and
same SHA-256 content is an idempotent replay. CSV replay identity additionally
binds the complete effective column mapping after preset overrides; correcting
a mapping for the same bytes is therefore an immutable conflict instead of a
false replay. The public `content_sha256` remains the digest of the unmodified
source bytes, and `burnban.invoice/v1` remains unchanged.

## Reports and matching confidence

```sh
burnban reconcile report --since 30d
burnban reconcile report --since 30d --provider anthropic --format json
burnban reconcile report --since 30d --format csv > reconciliation.csv
```

Matching is conservative: provider + UTC day + model + service tier + region.
The report separates matched estimate/billed totals, unmatched ledger traffic,
unmatched provider lines, and delayed/credit/batch/tax/fee adjustments. It also
shows variance, confidence, and the last successful import timestamp. CSV text
is spreadsheet-formula neutralized.

These import adapters and synthetic tests establish the reconciliation
foundation. They do **not** claim that a real OpenAI, Anthropic, or Gemini
invoice has been matched for a design partner. Before books-close use, finance
must verify the provider's current export mapping, currency/tax treatment,
account scope, and period boundaries against an actual invoice.
