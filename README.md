# 🔥 burnban

**Meter, itemize, and cap what your AI agents spend. Meters watch. Burnban acts.**

Your agents run all day on your API keys. Burnban is a single-binary local proxy that sits between them and every provider, shows you the burn in real time, itemizes the waste with dollar amounts attached, and cuts spend off when you say so. No signup, no cloud, no telemetry — your traffic never leaves your machine.

> Two frontier models launched on the same day this week, four-x apart on price. You can't manage what you don't meter.

## Quickstart

```sh
# 1. run the meter
burnban serve

# 2. point your agents at it (keys stay in your env — burnban never stores them)
export ANTHROPIC_BASE_URL=http://localhost:4141/anthropic
export OPENAI_BASE_URL=http://localhost:4141/openai/v1

# 3. watch the burn
burnban top                      # in the terminal, or
open http://localhost:4141       # the live dashboard
```

Set a budget and forget about surprise bills:

```sh
burnban cap --daily 10     # proxy returns 402 once today's spend hits $10
burnban ban                # emergency stop: pause ALL agent spend now
burnban lift --today       # resume, overriding today's cap
```

## What you get

- **Live dashboard** at `http://localhost:4141` — the burn total glowing ember, a fuse-style budget bar, per-model/per-agent tables, and waste receipts. One embedded HTML file served from the binary: no CDNs, no build step, nothing loads from the internet.
- **`burnban top`** — the same live view in your terminal: per-model and per-agent spend, cache hit rate, $/hour rate, and a budget bar that goes red before your bill does.
- **`burnban report`** — spend for any window, plus **waste receipts**: duplicate requests that burned money twice, and cache hit rates that mean you're paying full price for context the provider would re-serve at a 90% discount.
- **Budget enforcement** — a daily dollar cap enforced in the request path with a clear 402 your agent surfaces verbatim, and a manual **burn ban** kill switch.
- **Honest numbers** — usage comes from provider usage frames, priced per model including cache read/write economics. Unknown models are recorded as unpriced, never guessed. Estimated counts are flagged as estimates.

## How it works

```
agents (Claude Code, Codex, OpenClaw, Hermes, your app)
   │  one env var change
   ▼
burnban serve  ──►  api.anthropic.com / api.openai.com / api.x.ai
   │
   ├─ passes every byte through unmodified (SSE included)
   ├─ reads usage frames, prices them (cache-aware)
   ├─ SQLite at ~/.burnban/burnban.db — yours, greppable
   └─ refuses to forward when you're over budget
```

Burnban binds to `127.0.0.1` only. It is a pass-through observer: request and response bodies are never rewritten, and API keys are forwarded, never persisted.

## Providers

| provider  | point your client at                | env var the SDKs read |
|-----------|-------------------------------------|-----------------------|
| Anthropic | `http://localhost:4141/anthropic`   | `ANTHROPIC_BASE_URL`  |
| OpenAI    | `http://localhost:4141/openai/v1`   | `OPENAI_BASE_URL`     |
| xAI       | `http://localhost:4141/xai/v1`      | `OPENAI_BASE_URL` (xAI SDKs are OpenAI-compatible) |

Attribution: burnban groups spend by the client's `User-Agent`. For finer tracking, send `x-burnban-agent` / `x-burnban-session` headers (Claude Code: `ANTHROPIC_CUSTOM_HEADERS`).

OpenAI streaming note: send `stream_options: {"include_usage": true}` for exact counts; without it burnban estimates output tokens and flags them as estimates in reports.

## Pricing table

Current prices for the July 2026 lineup (GPT-5.6 Sol/Terra/Luna, Grok 4.5, Claude Opus 4.7/Sonnet 4.6/Haiku 4.5) ship embedded. Vendors change prices; override or extend without waiting for a release by creating `~/.burnban/pricing.json`:

```json
{"models": {"grok-4.5": {"input_per_mtok": 2.0, "output_per_mtok": 6.0, "cache_read_mult": 0.1}}}
```

## Roadmap

- **Cache-aware request shaping** — stabilize prompt prefixes to turn cache misses into 90%-off hits
- **Downshift routing** — send trivial calls to a cheap tier or your local Ollama, by policy
- **Per-agent budgets** and webhook alerts
- **State of Agent Spend** — opt-in anonymous aggregates, published monthly

## Development

```sh
make build   # single static binary, no cgo
make test    # offline: fixtures, not API calls — development burns $0
```

MIT © Oday Brahem
