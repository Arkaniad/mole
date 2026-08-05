-- Why a grounding check reached its verdict (§11.5).
--
-- `grounded` is three-state and answers one question: does the quote support the
-- claim. That leaves two outcomes it cannot express, and both matter.
--
-- A quote that has VANISHED from its source is not evidence against the claim. The
-- quote was checked verbatim against the fetched text at extraction time, so its
-- absence now means the page changed — and scoring that as "the evidence does not
-- support the claim" would penalize a claim for a publisher's edit. Same for a
-- source that cannot be reached at all: nothing was learned, and a claim must not
-- lose confidence because a host was down.
--
-- So those leave `grounded` NULL and record what happened here. Without the note a
-- reader sees an unchecked claim and cannot tell it from one nobody tried to check.
--
-- Not sent to the planner. The digest has no field for it, which is the structural
-- half of §9.1: page-derived material cannot reach the planner because there is
-- nowhere to put it.

ALTER TABLE claims ADD COLUMN grounding_note TEXT NOT NULL DEFAULT '';
