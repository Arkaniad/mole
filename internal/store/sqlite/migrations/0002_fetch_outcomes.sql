-- Fetch outcome log (§10.4).
--
-- Every fetch that does not yield usable text is recorded with its cause and
-- its domain. This is the input to the §17.1 headless-browser gate, and it is
-- recorded from M1 rather than M2 because adding it later would mean re-running
-- the whole eval corpus to obtain the data.
--
-- Successful fetches are recorded too, so a rate has a denominator.

CREATE TABLE fetch_outcomes (
    id          TEXT    PRIMARY KEY,
    session_id  TEXT    REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id     TEXT,

    url         TEXT    NOT NULL,
    domain      TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    err         TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

-- The two queries this table exists to answer: the rate per cause, and the
-- ranked domains within one cause.
CREATE INDEX idx_fetch_outcomes_outcome ON fetch_outcomes(outcome, created_at);
CREATE INDEX idx_fetch_outcomes_domain  ON fetch_outcomes(outcome, domain);
CREATE INDEX idx_fetch_outcomes_session ON fetch_outcomes(session_id, created_at);

-- session_id is nullable on purpose: a fetch made outside a session (a probe,
-- a warm-up, a CLI one-shot) is still evidence about a domain's behaviour and
-- should not be dropped for lack of a parent row.
