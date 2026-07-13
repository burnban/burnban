-- Synthetic OpenCode compatibility store. Never replace this with a user DB.
-- Schema and metadata paths verified against anomalyco/opencode commit
-- 8168f0f0f6645a0ca741fe02e90ff532bce04148 on 2026-07-12.
CREATE TABLE session_message (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    type TEXT NOT NULL,
    seq INTEGER NOT NULL,
    time_created INTEGER NOT NULL,
    data TEXT NOT NULL
);

CREATE TABLE message (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    time_created INTEGER NOT NULL,
    data TEXT NOT NULL
);

INSERT INTO session_message VALUES (
    'msg_exact', 'ses_resumed', 'assistant', 1, 1783252800000,
    '{"model":{"id":"claude-sonnet-5","providerID":"anthropic"},"tokens":{"input":725,"output":200,"reasoning":50,"cache":{"read":300,"write":25}},"cost":0.0042,"content":[{"type":"text","text":"fixture response must stay private"}]}'
);
INSERT INTO session_message VALUES (
    'msg_resumed', 'ses_resumed', 'assistant', 2, 1783252801000,
    '{"model":{"id":"gpt-fixture","providerID":"openai"},"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":10,"write":0}},"cost":0.0007}'
);
INSERT INTO session_message VALUES (
    'msg_bridge', 'ses_bridge', 'assistant', 1, 1783252802000,
    '{"model":{"id":"gemini-fixture","providerID":"google"},"tokens":{"input":30,"output":10,"reasoning":0,"cache":{"read":4,"write":2}},"cost":0.0002}'
);
INSERT INTO session_message VALUES (
    'msg_user', 'ses_resumed', 'user', 3, 1783252802500,
    '{"text":"fixture prompt must stay private","files":[],"agents":[]}'
);
INSERT INTO session_message VALUES (
    'msg_unknown', 'ses_resumed', 'model-switched', 4, 1783252802600,
    '{"model":{"id":"ignored","providerID":"ignored"}}'
);
INSERT INTO session_message VALUES (
    'msg_malformed', 'ses_bad', 'assistant', 1, 1783252804000,
    '{not-json'
);
INSERT INTO session_message VALUES (
    'msg_missing_tokens', 'ses_bad', 'assistant', 2, 1783252805000,
    '{"model":{"id":"unknown-shape","providerID":"fixture"},"tokens":{"input":10,"output":2}}'
);
INSERT INTO session_message VALUES (
    'msg_outside', 'ses_old', 'assistant', 1, 1782000000000,
    '{"model":{"id":"old-model","providerID":"fixture"},"tokens":{"input":999,"output":999,"reasoning":0,"cache":{"read":0,"write":0}}}'
);

-- The event bridge can expose the same stable message in both projections.
-- Burnban must emit ses_bridge/msg_bridge only once.
INSERT INTO message VALUES (
    'msg_bridge', 'ses_bridge', 1783252802000,
    '{"role":"assistant","providerID":"google","modelID":"gemini-fixture","tokens":{"input":30,"output":10,"reasoning":0,"cache":{"read":4,"write":2}},"cost":0.0002}'
);
INSERT INTO message VALUES (
    'msg_legacy', 'ses_legacy', 1783252803000,
    '{"role":"assistant","providerID":"openrouter","modelID":"vendor/legacy-fixture","tokens":{"input":80,"output":15,"reasoning":3,"cache":{"read":8,"write":1}},"cost":0.0011,"response":"legacy fixture response must stay private"}'
);
INSERT INTO message VALUES (
    'msg_legacy_user', 'ses_legacy', 1783252803100,
    '{"role":"user","content":"legacy fixture prompt must stay private"}'
);
