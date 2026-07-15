# Gemini CLI synthetic fixture

`session-v1.jsonl` is synthetic and contains no real conversation data. Its
metadata shape was checked on 2026-07-12 against
`google-gemini/gemini-cli@f354eebaf43b25bacb176007e449bb9a638fd101`, specifically
`packages/core/src/services/chatRecordingTypes.ts` and
`packages/core/src/services/chatRecordingService.ts`.

The fixture exercises the append-only metadata header, a Gemini message later
re-appended with `TokensSummary`, duplicate message IDs, cached input as a
subset of prompt input, separately billed thinking/tool tokens, and an
out-of-window record. Prompt and response strings are synthetic sentinels used
only to prove the adapter never copies content into an event or diagnostic.
