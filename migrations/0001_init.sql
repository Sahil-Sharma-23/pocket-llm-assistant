-- All timestamps are RFC 3339 UTC text (e.g. 2026-11-02T16:00:55Z).

CREATE TABLE conversations (
    id          INTEGER PRIMARY KEY,
    channel     TEXT NOT NULL CHECK (channel IN ('telegram', 'web', 'system')),
    external_id TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (channel, external_id)
);

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    agent           TEXT NOT NULL DEFAULT '',
    content         TEXT NOT NULL,
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_messages_conversation ON messages(conversation_id, id);

CREATE VIRTUAL TABLE messages_fts USING fts5(
    content, content='messages', content_rowid='id', tokenize='unicode61'
);
CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TABLE preferences (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE tasks (
    id           INTEGER PRIMARY KEY,
    title        TEXT NOT NULL,
    notes        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'cancelled')),
    priority     INTEGER NOT NULL DEFAULT 2 CHECK (priority BETWEEN 1 AND 3),
    due_at       TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    completed_at TEXT
);
CREATE INDEX idx_tasks_status ON tasks(status, due_at);

CREATE TABLE knowledge (
    id         INTEGER PRIMARY KEY,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    -- user: told by the user; research: summarised from the web (external);
    -- conversation: extracted from chat history.
    source     TEXT NOT NULL CHECK (source IN ('user', 'research', 'conversation')),
    url        TEXT NOT NULL DEFAULT '',
    tags       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE knowledge_fts USING fts5(
    title, body, tags, content='knowledge', content_rowid='id', tokenize='unicode61'
);
CREATE TRIGGER knowledge_ai AFTER INSERT ON knowledge BEGIN
    INSERT INTO knowledge_fts(rowid, title, body, tags) VALUES (new.id, new.title, new.body, new.tags);
END;
CREATE TRIGGER knowledge_ad AFTER DELETE ON knowledge BEGIN
    INSERT INTO knowledge_fts(knowledge_fts, rowid, title, body, tags) VALUES ('delete', old.id, old.title, old.body, old.tags);
END;
CREATE TRIGGER knowledge_au AFTER UPDATE ON knowledge BEGIN
    INSERT INTO knowledge_fts(knowledge_fts, rowid, title, body, tags) VALUES ('delete', old.id, old.title, old.body, old.tags);
    INSERT INTO knowledge_fts(rowid, title, body, tags) VALUES (new.id, new.title, new.body, new.tags);
END;

CREATE TABLE agent_runs (
    id                INTEGER PRIMARY KEY,
    conversation_id   INTEGER REFERENCES conversations(id) ON DELETE SET NULL,
    agent             TEXT NOT NULL,
    trigger           TEXT NOT NULL, -- message | command | job | approval
    input             TEXT NOT NULL,
    output            TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK (status IN ('running', 'ok', 'error', 'awaiting_approval')),
    error             TEXT NOT NULL DEFAULT '',
    llm_calls         INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    started_at        TEXT NOT NULL,
    finished_at       TEXT
);
CREATE INDEX idx_runs_started ON agent_runs(started_at);

CREATE TABLE tool_calls (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER REFERENCES agent_runs(id) ON DELETE CASCADE,
    tool        TEXT NOT NULL,
    permission  TEXT NOT NULL CHECK (permission IN ('read', 'write', 'sensitive')),
    args        TEXT NOT NULL,
    result      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('ok', 'error', 'awaiting_approval', 'invalid')),
    error       TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
CREATE INDEX idx_tool_calls_run ON tool_calls(run_id);

CREATE TABLE approvals (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER REFERENCES agent_runs(id) ON DELETE SET NULL,
    tool        TEXT NOT NULL,
    permission  TEXT NOT NULL CHECK (permission IN ('write', 'sensitive')),
    args        TEXT NOT NULL,
    summary     TEXT NOT NULL,           -- human-readable description of the action
    channel     TEXT NOT NULL DEFAULT '',-- where to report the outcome
    external_id TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'executed', 'failed')),
    decided_by  TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    decided_at  TEXT,
    executed_at TEXT
);
CREATE INDEX idx_approvals_status ON approvals(status, created_at);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,           -- reminder | monitor | daily_plan | github_digest | backup
    name        TEXT NOT NULL,
    payload     TEXT NOT NULL DEFAULT '{}',
    -- schedule: "once" | "every <go-duration>" | "daily HH:MM"
    schedule    TEXT NOT NULL,
    next_run_at TEXT NOT NULL,
    last_run_at TEXT,
    last_status TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    UNIQUE (kind, name)
);
CREATE INDEX idx_jobs_due ON jobs(enabled, next_run_at);

CREATE TABLE monitor_results (
    id          INTEGER PRIMARY KEY,
    monitor     TEXT NOT NULL,
    ok          INTEGER NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    checked_at  TEXT NOT NULL
);
CREATE INDEX idx_monitor_results ON monitor_results(monitor, checked_at);
