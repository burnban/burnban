# GitHub Copilot CLI synthetic fixture

`session-v1.jsonl` is synthetic and contains no real conversation data. Its
metadata shape was checked on 2026-07-12 against the official
`@github/copilot@1.0.70` package (build metadata commit `1a7a0a2e78`) and the
`schemas/session-events.schema.json` shipped by its platform package.

The fixture mirrors the durable `session.shutdown` schema introduced in the
official Copilot CLI 1.0.22 changelog and retained in 1.0.70: cumulative
per-model request counts, normalized billing `tokenDetails`, and input, output,
cache-read, cache-write, and reasoning-token totals. Two shutdown records prove that the latest cumulative
snapshot supersedes an earlier exit. User and assistant content are synthetic
sentinels used only to prove the adapter never decodes or emits message data.
