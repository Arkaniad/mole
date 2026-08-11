-- M9: extracted dataset rows.
--
-- A row is a claim with columns, and it is stored beside claims rather than
-- inside them because the shapes differ in the one way that matters: a claim has
-- one text, a row has one value per schema field. Encoding a row's values as a
-- claim's text would make the merge parse its own storage back out.
--
-- The values are JSON. A column per field is impossible — the schema is chosen
-- per session — and a key/value side table would turn every read of one row into
-- a join over a variable number of rows. The values are only ever read as a
-- whole row by the merge, never queried by field, so JSON costs nothing here.
--
-- Provenance is columns, not JSON: source and quote are what §11.5 makes
-- mandatory, and they are the fields somebody auditing a dataset filters on.
CREATE TABLE dataset_rows (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lead_id       TEXT,
    seq           INTEGER NOT NULL,

    values_json   TEXT NOT NULL,
    source        TEXT NOT NULL CHECK (length(source) > 0),

    -- NOT NULL is not the constraint that matters here. §11.5 makes the quote
    -- mandatory, and `NOT NULL` admits '' — so the one shape this column exists to
    -- forbid, a row with no evidence behind it, was the one it allowed. The check
    -- is the schema stating the project's own rule instead of trusting every
    -- present and future writer to remember it.
    quote         TEXT NOT NULL CHECK (length(quote) > 0),
    quote_offset  INTEGER NOT NULL DEFAULT 0,

    -- Unix microseconds, per 0001_init's first invariant ("Timestamps are INTEGER
    -- unix microseconds (UTC). Sortable, no parsing."). This said TIMESTAMP and
    -- stored driver-formatted text, which is the one table in the database whose
    -- times neither sort nor compare against any other table's.
    retrieved_at  INTEGER NOT NULL
);

-- Read back per session in insertion order. seq exists for the same reason
-- claim_sequence does (migration 0008): row ids are random, so ordering by id
-- would make a replay's dataset differ from the recording's.
CREATE INDEX idx_dataset_rows_session ON dataset_rows(session_id, seq);

-- The schema a session's rows were extracted against.
--
-- Stored per session, because reading a dataset back needs the schema that
-- produced it: field order for the CSV header, and which fields are keys for the
-- merge. Reconstructing it from the values would lose both.
ALTER TABLE sessions ADD COLUMN dataset_schema TEXT;
