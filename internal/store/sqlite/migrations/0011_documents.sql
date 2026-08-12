-- Toolkit mode: the source text a claim was verified against.
--
-- mole otherwise discards source text by design — a claim keeps its quote and
-- offset, and `mole eval --citations` re-fetches to check them. Toolkit mode
-- cannot work that way. When an agent's model mines a claim and asks mole to
-- record it, verifying the quote against text the AGENT supplied proves nothing:
-- a model that invents a quote can invent the passage to match. mole has to hold
-- the document itself.
--
-- This is the first table that stores third-party content rather than facts about
-- it, so it carries a retention rule rather than growing forever. Two mechanisms,
-- because they cover different failures:
--
--   * ON DELETE CASCADE removes a session's documents with the session.
--   * expires_at is a hard TTL for a session nobody ever deletes.
--
-- Expiry is also enforced on READ, not only by the sweep. A TTL that depends on a
-- background job having run is not a TTL; it is a hope.
CREATE TABLE documents (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,

    url         TEXT NOT NULL CHECK (length(url) > 0),
    title       TEXT NOT NULL DEFAULT '',
    -- The extracted text, exactly as the agent was handed it. A quote is checked
    -- against this and nothing else, so it must not be normalised after the fact:
    -- an offset that moves is provenance that lies.
    text        TEXT NOT NULL,
    -- Whether the extractor cut the page short. A quote from beyond the cut cannot
    -- be verified, and a caller needs to know that is why.
    truncated   INTEGER NOT NULL DEFAULT 0,

    -- Unix microseconds, per 0001_init's first invariant.
    fetched_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

-- Read back per session, and swept by expiry.
CREATE INDEX idx_documents_session ON documents(session_id, fetched_at);
CREATE INDEX idx_documents_expiry  ON documents(expires_at);
