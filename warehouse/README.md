# Burnban warehouse contract

`burnban telemetry export` writes newline-delimited JSON objects described by
[`schema.json`](schema.json). Object paths are Hive-style UTC partitions:
`date=YYYY-MM-DD/hour=HH/part-NNNNNN.ndjson`.

Verify every object's byte count and SHA-256 against `manifest.json` before
loading. Then load the objects into a typed table based on
[`schema.sql`](schema.sql). The [`dbt`](dbt) example assumes that table is named
`burnban_raw.requests`; override `burnban_raw_schema` and
`burnban_raw_table` for the target warehouse.

The SQL is intentionally conservative rather than tied to a cloud vendor.
Timestamp and numeric conversion syntax varies across BigQuery, Snowflake,
Databricks, Redshift, and Postgres, so the ingestion layer should validate the
JSON schema and populate the typed columns before dbt runs.
