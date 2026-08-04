-- Separate what the extractor reported from what the graph derives (§11.3).
--
-- `claims.confidence` has been carrying the model's own number since M1, and
-- §11.3 rejects exactly that: "LLM self-reported confidence numbers are not
-- calibrated and mostly encode fluency". It was not inert. output/report.go
-- ordered the report by it, so an uncalibrated number chose which claims led the
-- answer, and a fluent claim from one anonymous blog outranked a corroborated one.
--
-- The extractor's number is still a real signal, just not confidence. The mine
-- prompt asks "how clearly the document states this, NOT how true you believe it
-- is" — that is assertion strength, a property of the document, and it is an
-- input to the derived figure rather than a substitute for it.
--
-- So: rename it to what it measures, and leave `confidence` for §11.3's derived
-- value. Existing rows get assertion_strength from the old column and confidence
-- 0, which reads correctly as "not yet verified" — the Verifier has never run on
-- them, and pretending otherwise is what this migration exists to stop.

ALTER TABLE claims RENAME COLUMN confidence TO assertion_strength;

-- NULL rather than 0 would be more honest about "never computed", but Grounded
-- already carries a three-state nullable and the ordering code would need a
-- second nil check on every comparison. 0 is the correct derived confidence for
-- an unverified claim anyway: nothing corroborates it yet.
ALTER TABLE claims ADD COLUMN confidence REAL NOT NULL DEFAULT 0;

-- Verifier reads claims per session and skips what it has already scored.
CREATE INDEX IF NOT EXISTS idx_claims_session_verify
    ON claims(session_id, verify_depth);
