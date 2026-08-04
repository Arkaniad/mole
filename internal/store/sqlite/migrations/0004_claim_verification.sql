-- Record that the Verifier has looked at a claim (§11.1).
--
-- Confidence alone cannot answer "has this been scored". §11.3's formula can
-- legitimately return 0 — an uncorroborated claim carrying a `contradicts` edge
-- should score 0 — so `confidence = 0` conflates "scored, and it came out badly"
-- with "never examined". The Verifier runs incrementally over the session's whole
-- claim set, so it needs to ask for the claims it has not seen; an ambiguous
-- sentinel would make it rescore claims forever or skip ones it never scored.
--
-- Nullable rather than a zero timestamp: absent means never verified, and there
-- is no plausible real value to confuse it with.

ALTER TABLE claims ADD COLUMN verified_at INTEGER;

-- The Verifier's own query: unscored claims for one session, oldest first.
CREATE INDEX IF NOT EXISTS idx_claims_unverified
    ON claims(session_id, verified_at, created_at);
