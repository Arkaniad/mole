-- M8/§12.1: the audit trail for data leaving the user's machine.
--
-- "Every crossing is logged, so a user can audit exactly what left their
-- machine." The gate has emitted a structured log line since M8, which satisfies
-- the letter of that and not the use: logs rotate, Info is off in some setups,
-- and a line cannot be queried per session. A user asking what mole sent about
-- their sales data needs a table, and `mole eval` needs one to report §14.3's
-- exfil number per session instead of naming it blocked.
--
-- Every column is a count, a hash, a name, or one of mole's own strings. There is
-- deliberately no column that can hold a value from the data — an audit trail
-- that is another copy of the thing the user was worried about is worse than
-- none.
--
-- The QUERY is kept, and that is safe here for a reason specific to §12.3: a
-- model cannot author SQL, so a statement is a hypothesis template filled with
-- identifiers the connector profile already published. It is also what makes a
-- local claim traceable — a claim citing connector:sales#<hash> is evidence only
-- if the hash resolves to a readable statement.
CREATE TABLE compute_crossings (
    id                  TEXT PRIMARY KEY,
    session_id          TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id             TEXT,

    connector           TEXT NOT NULL CHECK (length(connector) > 0),
    query               TEXT NOT NULL,
    query_hash          TEXT NOT NULL CHECK (length(query_hash) > 0),

    -- crossed | refused | withheld. withheld is its own outcome rather than a
    -- kind of refusal: a refusal is the gate working, and a withholding is a rule
    -- upstream having broken in a way the exfil backstop caught.
    outcome             TEXT NOT NULL CHECK (outcome IN ('crossed', 'refused', 'withheld')),
    detail              TEXT NOT NULL DEFAULT '',

    rows_described      INTEGER NOT NULL DEFAULT 0,
    columns             INTEGER NOT NULL DEFAULT 0,
    columns_withheld    INTEGER NOT NULL DEFAULT 0,
    buckets             INTEGER NOT NULL DEFAULT 0,
    buckets_suppressed  INTEGER NOT NULL DEFAULT 0,
    buckets_beyond_topk INTEGER NOT NULL DEFAULT 0,
    tests               INTEGER NOT NULL DEFAULT 0,
    truncated           INTEGER NOT NULL DEFAULT 0,

    -- Unix microseconds, per 0001_init's invariant.
    created_at          INTEGER NOT NULL
);

-- Read back per session in order, which is how an audit is read.
CREATE INDEX idx_compute_crossings_session ON compute_crossings(session_id, created_at);
