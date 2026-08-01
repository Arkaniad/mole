-- Mole schema, initial revision.
--
-- Conventions:
--   * Timestamps are INTEGER unix microseconds (UTC). Sortable, no parsing.
--   * Money is INTEGER micro-dollars. Never a float.
--   * Enum columns carry CHECK constraints so a typo is a write error, not a
--     row that silently fails to match any query later.

CREATE TABLE sessions (
    id               TEXT    PRIMARY KEY,
    prompt           TEXT    NOT NULL,
    mode             TEXT    NOT NULL CHECK (mode IN ('report','dataset','chain','ask')),
    actor_types      TEXT    NOT NULL,
    budget_unit      TEXT    NOT NULL CHECK (budget_unit IN ('usd','tokens')),

    budget           INTEGER NOT NULL CHECK (budget  >= 0),
    -- These three are the budget invariant. The CHECKs are the last line of
    -- defence: a settle that would drive held negative aborts the transaction
    -- rather than corrupting the ledger.
    spent            INTEGER NOT NULL DEFAULT 0 CHECK (spent  >= 0),
    held             INTEGER NOT NULL DEFAULT 0 CHECK (held   >= 0),
    escrow           INTEGER NOT NULL DEFAULT 0 CHECK (escrow >= 0),

    max_tool_calls   INTEGER NOT NULL DEFAULT 0,
    max_leads        INTEGER NOT NULL DEFAULT 0,
    max_wallclock_ns INTEGER NOT NULL DEFAULT 0,
    tool_call_count  INTEGER NOT NULL DEFAULT 0 CHECK (tool_call_count >= 0),
    lead_count       INTEGER NOT NULL DEFAULT 0 CHECK (lead_count      >= 0),

    status           TEXT    NOT NULL CHECK (status IN ('running','done','budget_exhausted','cancelled','failed')),
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX idx_sessions_status_created ON sessions(status, created_at DESC);

-- Append-only cost ledger. This table is the source of truth for spend;
-- sessions.spent is a materialized sum of it, written in the same transaction.
CREATE TABLE tool_calls (
    id                 TEXT    PRIMARY KEY,
    session_id         TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id            TEXT,
    role               TEXT    NOT NULL CHECK (role IN ('planner','executor','verifier','output')),
    type               TEXT    NOT NULL,
    model              TEXT    NOT NULL DEFAULT '',
    input              TEXT    NOT NULL DEFAULT '',

    usd_micros         INTEGER NOT NULL DEFAULT 0 CHECK (usd_micros         >= 0),
    input_tokens       INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens       >= 0),
    output_tokens      INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens      >= 0),
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens  >= 0),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),

    err                TEXT    NOT NULL DEFAULT '',
    duration_ms        INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);

CREATE INDEX idx_tool_calls_session      ON tool_calls(session_id, created_at);
CREATE INDEX idx_tool_calls_session_role ON tool_calls(session_id, role);

-- A hold placed on budget before dispatch, settled to actual cost afterwards.
CREATE TABLE reservations (
    id          TEXT    PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id     TEXT,
    amount      INTEGER NOT NULL CHECK (amount > 0),
    status      TEXT    NOT NULL CHECK (status IN ('held','settled','released')),
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    resolved_at INTEGER
);

CREATE INDEX idx_reservations_session_status ON reservations(session_id, status);
CREATE INDEX idx_reservations_expiry         ON reservations(status, expires_at);

-- Leads are leased rather than merely marked "running": a daemon restart
-- requeues leads whose lease expired instead of stranding them forever.
CREATE TABLE leads (
    id            TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    actor_type    TEXT    NOT NULL CHECK (actor_type IN ('web','academic','local_compute')),
    query         TEXT    NOT NULL,
    parent_id     TEXT,
    depth         INTEGER NOT NULL DEFAULT 0,
    priority      INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL CHECK (status IN ('queued','leased','done','failed','skipped_cached')),
    lease_owner   TEXT,
    lease_expires INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX idx_leads_dispatch ON leads(session_id, status, priority DESC, created_at);
CREATE INDEX idx_leads_lease    ON leads(status, lease_expires);

-- Claims carry the evidence quote at extraction time. A claim whose quote does
-- not appear verbatim in the extracted text is rejected at the actor boundary,
-- which is what makes grounding checkable without the verifier re-reading pages.
CREATE TABLE claims (
    id            TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id       TEXT    NOT NULL,
    text          TEXT    NOT NULL,

    source        TEXT    NOT NULL,
    tool_call_id  TEXT    NOT NULL DEFAULT '',
    quote         TEXT    NOT NULL DEFAULT '',
    quote_offset  INTEGER NOT NULL DEFAULT 0,

    published_at  INTEGER,
    retrieved_at  INTEGER NOT NULL,

    -- Verification lineage. A per-row counter cannot bind, because a follow-up
    -- lead produces a new claim that would restart it.
    root_claim_id TEXT    NOT NULL,
    verify_depth  INTEGER NOT NULL DEFAULT 0,

    confidence    REAL    NOT NULL DEFAULT 0,
    grounded      INTEGER,
    created_at    INTEGER NOT NULL
);

CREATE INDEX idx_claims_session ON claims(session_id, created_at);
CREATE INDEX idx_claims_root    ON claims(root_claim_id);

CREATE TABLE claim_edges (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    from_id    TEXT    NOT NULL,
    to_id      TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('supports','contradicts','duplicate_of','supersedes','refines')),
    weight     REAL    NOT NULL DEFAULT 1.0,
    created_by TEXT    NOT NULL DEFAULT '',
    rationale  TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (from_id, to_id, kind)
);

CREATE INDEX idx_claim_edges_session ON claim_edges(session_id);
CREATE INDEX idx_claim_edges_from    ON claim_edges(from_id);
CREATE INDEX idx_claim_edges_to      ON claim_edges(to_id);

CREATE TABLE spans (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id    TEXT,
    parent_id  TEXT,
    name       TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    status     TEXT    NOT NULL DEFAULT '',
    attrs      TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_spans_session ON spans(session_id, started_at);
