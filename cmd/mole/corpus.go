package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/spf13/cobra"
)

// The corpus runner (§14.2, first half).
//
// §14.3 says "every milestone from M3 on reports these numbers; regressions block merge",
// and `mole eval` scores one session — so there has never been anything to block a merge
// with. One session's numbers are an anecdote.
//
// This needs no labelled answers. Under MOLE_RECORD=replay it is free, deterministic and
// offline, and it catches the class of regression that has actually bitten this codebase:
// a change that quietly stops claims being extracted, stops edges being written, or starts
// rejecting every synthesis. The labelled half — claim precision, contradiction recall,
// staleness detection — needs ground truth a person supplies, and is not faked here.

type corpusOpts struct {
	usd         string
	tokens      int64
	timeout     time.Duration
	maxSources  int
	maxDepth    int
	alwaysFetch bool
	asJSON      bool
	baseline    string
	writeBase   string
	dbPath      string
}

func newCorpusCmd() *cobra.Command {
	var o corpusOpts

	c := &cobra.Command{
		Use:   "corpus <file.json>",
		Short: "Run a question set and aggregate the scorecards (§14.2)",
		Long: "Runs every question in a corpus file, scores each session, and reports the\n" +
			"aggregate. Exits non-zero if any question hit a hard regression or failed to\n" +
			"run at all.\n\n" +
			"Record once, then replay for free:\n" +
			"  MOLE_RECORD=record MOLE_CASSETTE_DIR=./testdata/cassettes mole corpus q.json\n" +
			"  MOLE_RECORD=replay MOLE_CASSETTE_DIR=./testdata/cassettes mole corpus q.json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.dbPath = dbPath(cmd)
			return cmdCorpus(cmd.Context(), args[0], o)
		},
	}

	f := c.Flags()
	f.StringVar(&o.usd, "usd", "", "per-question budget in dollars")
	f.Int64Var(&o.tokens, "tokens", 0, "per-question budget in tokens")
	f.DurationVar(&o.timeout, "timeout", 10*time.Minute, "per-question wall-clock ceiling")
	f.IntVar(&o.maxSources, "max-sources", 5, "sources to read per lead")
	f.IntVar(&o.maxDepth, "max-depth", 2, "rounds of follow-up leads the planner may add")
	f.BoolVar(&o.alwaysFetch, "always-fetch", false,
		"fetch every page even when the search provider supplied its text")
	f.BoolVar(&o.asJSON, "json", false, "emit the report as JSON")
	f.StringVar(&o.baseline, "baseline", "", "compare the aggregate against a saved report")
	f.StringVar(&o.writeBase, "write-baseline", "", "write the report to this path for later comparison")
	return c
}

func cmdCorpus(ctx context.Context, path string, o corpusOpts) error {
	corpus, err := eval.LoadCorpus(path)
	if err != nil {
		return err
	}

	rep := eval.CorpusReport{Corpus: corpus.Name}
	if !o.asJSON {
		fmt.Printf("corpus %s — %d question(s)\n\n", corpus.Name, len(corpus.Questions))
	}

	for i, q := range corpus.Questions {
		res := eval.QuestionResult{ID: q.ID, Question: q.Question, Tags: q.Tags}
		if !o.asJSON {
			fmt.Printf(" [%d/%d] %s — %.60s\n", i+1, len(corpus.Questions), q.ID, q.Question)
		}

		// Quiet, and the id captured through the callback: a failed run still has a
		// ledger to reconcile and claims to score, and those are the runs worth looking
		// at.
		ro := researchOpts{
			usd: o.usd, tokens: o.tokens, mode: "report",
			maxSources: o.maxSources, timeout: o.timeout, maxDepth: o.maxDepth,
			// silent: the corpus runner produces the output, and a hundred inlined
			// reports would bury it.
			alwaysFetch: o.alwaysFetch, quiet: true, silent: true, dbPath: o.dbPath,
			onSession: func(id string) { res.SessionID = id },
		}
		if runErr := cmdResearch(ctx, q.Question, ro); runErr != nil {
			res.Err = runErr.Error()
		}

		if res.SessionID != "" {
			if st, n, serr := sessionOutcome(ctx, o.dbPath, res.SessionID); serr == nil {
				res.Status, res.Claims = st, n
			}
			if card, serr := scoreSession(ctx, o.dbPath, res.SessionID); serr != nil {
				// Scoring failing is not the same as research failing, and a corpus that
				// conflated them would report a broken evaluator as a broken pipeline.
				if res.Err == "" {
					res.Err = "scoring failed: " + serr.Error()
				}
			} else {
				res.Card = card
			}
		}
		rep.Results = append(rep.Results, res)

		if !o.asJSON {
			printQuestionLine(res)
		}
		// Keep going. One question that cannot run says nothing about the next, and a
		// corpus that aborts on the first failure reports on whatever ran before it.
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	rep.Aggregate = eval.Aggregate(rep.Results)

	if o.writeBase != "" {
		if err := writeReport(o.writeBase, rep); err != nil {
			return err
		}
	}
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printCorpusReport(rep, o.baseline)
	}

	if rep.Failed() {
		return errors.New("corpus run has regressions")
	}
	return nil
}

// sessionOutcome reads what the session actually ended as.
//
// The scorecard cannot answer this: its hard regressions are budget overshoot, ledger
// drift and stranded holds, and a run that produced nothing has none of them.
func sessionOutcome(ctx context.Context, path, sessionID string) (string, int, error) {
	db, err := openDBRead(ctx, path)
	if err != nil {
		return "", 0, err
	}
	defer db.Close()

	var status string
	var claims int64
	err = db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		s, err := q.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		status = string(s.Status)
		claims, err = q.CountClaims(ctx, sessionID)
		return err
	})
	return status, int(claims), err
}

// scoreSession opens the database read-only and scores one session.
func scoreSession(ctx context.Context, path, sessionID string) (eval.Scorecard, error) {
	db, err := openDBRead(ctx, path)
	if err != nil {
		return eval.Scorecard{}, err
	}
	defer db.Close()
	return eval.Score(ctx, db, sessionID, eval.Options{})
}

func printQuestionLine(res eval.QuestionResult) {
	mark := "✓"
	detail := ""
	switch {
	case res.Err != "":
		mark = "✗"
		detail = res.Err
	case res.Barren():
		mark = "✗"
		detail = fmt.Sprintf("ran but produced nothing (status %s, %d claims)",
			res.Status, res.Claims)
	case res.Card.Failed():
		mark = "✗"
		detail = "hard regression"
	default:
		for _, m := range res.Card.Metrics {
			if m.Name == "claim integrity" && m.Status == eval.Measured {
				detail = m.Detail
			}
		}
	}
	fmt.Printf("      %s %.90s\n\n", mark, detail)
}

func printCorpusReport(rep eval.CorpusReport, baselinePath string) {
	ran, failed := 0, 0
	for _, q := range rep.Results {
		if q.Err == "" {
			ran++
		}
		if q.Err != "" || q.Barren() || q.Card.Failed() {
			failed++
		}
	}
	fmt.Printf("\n%d of %d question(s) ran; %d with problems\n\n", ran, len(rep.Results), failed)

	w := newTabWriter()
	fmt.Fprintln(w, "METRIC\tVALUE\tDETAIL")
	for _, m := range rep.Aggregate {
		switch m.Status {
		case eval.Measured:
			fmt.Fprintf(w, "%s\t%.1f %s\t%s\n", m.Name, m.Value, m.Unit, m.Detail)
		case eval.Blocked:
			fmt.Fprintf(w, "%s\t—\tblocked: %.60s\n", m.Name, m.Reason)
		default:
			fmt.Fprintf(w, "%s\t—\t%s\n", m.Name, m.Detail)
		}
	}
	_ = w.Flush()

	if baselinePath == "" {
		return
	}
	base, err := readReport(baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nwarning: could not read the baseline: %v\n", err)
		return
	}
	deltas, notes := eval.Compare(base.Aggregate, rep.Aggregate)
	if len(deltas) == 0 && len(notes) == 0 {
		fmt.Println("\nno change against the baseline")
		return
	}
	fmt.Println("\nagainst the baseline:")
	for _, d := range deltas {
		// Direction only. Without labelled answers there is no direction of
		// IMPROVEMENT for most of these — a lower disagreement rate is better if the
		// adjudicator was producing false positives and worse if it has stopped finding
		// real ones — and a tool that guesses at that will be believed.
		sign := "+"
		if d.Change < 0 {
			sign = ""
		}
		fmt.Printf("  %-26s %.1f → %.1f  (%s%.1f %s)\n",
			d.Name, d.Baseline, d.Current, sign, d.Change, d.Unit)
	}
	for _, n := range notes {
		fmt.Printf("  · %s\n", n)
	}
}

func writeReport(path string, rep eval.CorpusReport) error {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	fmt.Fprintf(os.Stderr, "baseline written to %s\n", path)
	return nil
}

func readReport(path string) (eval.CorpusReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return eval.CorpusReport{}, err
	}
	var rep eval.CorpusReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return eval.CorpusReport{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return rep, nil
}
