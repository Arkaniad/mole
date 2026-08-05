-- Verification lineage on leads (§11.4).
--
-- The claims table has carried root_claim_id and verify_depth since M0, and
-- nothing has ever set them to anything but "this claim is its own root" — because
-- the mechanism that increments them, a follow-up lead spawned to resolve a
-- contradiction, did not exist. A lead had no way to say "the claims you produce
-- belong to this investigation, one level deeper".
--
-- That gap is why the cap has to arrive with the mechanism rather than before it.
-- §11.4's own argument is that a per-row RecheckCount cannot bind, because a
-- follow-up produces a NEW claim whose counter starts at zero; the same is true of
-- a depth cap with nothing to increment it. M1 shipped exactly that shape once
-- already — MaxLeads was checked, tested, and never bound, because no caller set
-- the counter it read.
--
-- Nullable root_claim_id: an ordinary lead from the planner has no claim ancestry,
-- and a sentinel would have to be excluded from every query that groups by root.

ALTER TABLE leads ADD COLUMN root_claim_id TEXT;
ALTER TABLE leads ADD COLUMN verify_depth INTEGER NOT NULL DEFAULT 0;

-- The per-root cap counts existing follow-ups for one root, which is a lookup by
-- (session, root).
CREATE INDEX IF NOT EXISTS idx_leads_root
    ON leads(session_id, root_claim_id);
