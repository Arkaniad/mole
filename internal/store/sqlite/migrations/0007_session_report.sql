-- The session's rendered answer, stored (§5.1, §13).
--
-- Until now the report existed only in memory. The CLI generated it, printed it,
-- and dropped it; the daemon generated it, PAID for it out of escrow, and
-- dropped it. That was invisible on the command line, where the answer goes
-- straight to a terminal, and fatal over MCP: §5.1's research.result is defined
-- as returning {report_md, claims[], edges[]}, and the flow it belongs to is
-- report -> poll status -> result. The result step is where the answer arrives,
-- and it could only ever return an empty string.
--
-- A column on sessions rather than a table. One session has one report, it is
-- written once when the session finalizes, and nothing queries reports
-- independently of the session that produced them.
--
-- report_degraded records why the prose is missing or unsynthesized — a failed
-- model call, a refusal, a rejected body, or no budget left. Stored beside the
-- report for the same reason grounding_note is stored beside `grounded`: without
-- it a reader sees an empty answer and cannot tell "nothing was found" from
-- "synthesis failed and the evidence is still here".

ALTER TABLE sessions ADD COLUMN report_md TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN report_degraded TEXT NOT NULL DEFAULT '';
