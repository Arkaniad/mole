package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/record"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/spf13/cobra"
)

// mole eval
//
// Scores a finished session against the mechanical half of §14.3. Everything it
// reports is arithmetic over persisted state — no model calls, no network, no
// judgement — which is what makes these the numbers that can block a merge
// without anyone arguing about them.
//
// It exits non-zero on a regression, so CI can run it directly.

const evalUsage = `Score a finished session against the §14.3 metrics.

Reports the mechanical metrics — budget adherence, ledger consistency, claim
integrity — and explicitly names the ones that cannot be computed yet, with
what each is waiting on. Four of the nine need the Verifier (M4), the
aggregation gate (M8), or a labelled corpus, and will read "blocked" until
then. A scorecard that silently omitted them would read as complete.

Exits non-zero when a metric is a hard regression: budget overshoot, ledger
drift, a stranded reservation, or a malformed claim. Quality is never a
regression here — only things that are objectively wrong.
`

func newEvalCmd() *cobra.Command {
	var (
		asJSON    bool
		last      bool
		verbose   bool
		citations bool
	)

	c := &cobra.Command{
		Use:   "eval [session-id]",
		Short: "Score a finished session",
		Long:  evalUsage,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			if len(args) == 1 {
				id = args[0]
			} else if !last {
				return errors.New("usage: mole eval <session-id>   (or --last)")
			}
			return cmdEval(cmd.Context(), dbPath(cmd), id, evalOpts{
				asJSON: asJSON, verbose: verbose, citations: citations,
			})
		},
	}

	f := c.Flags()
	f.BoolVar(&last, "last", false, "score the most recent session")
	f.BoolVar(&asJSON, "json", false, "emit the scorecard as JSON")
	f.BoolVar(&verbose, "verbose", false, "show blocked metrics and their reasons")
	f.BoolVar(&citations, "citations", false,
		"re-read every cited source and check the quote is in it (costs a fetch per source; free under MOLE_RECORD=replay)")
	return c
}

type evalOpts struct {
	asJSON    bool
	verbose   bool
	citations bool
}

func cmdEval(ctx context.Context, path, sessionID string, o evalOpts) error {
	db, err := openDBRead(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()

	var prompt string
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		if sessionID == "" {
			list, err := q.ListSessions(ctx, 1)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return errors.New("no sessions to score")
			}
			sessionID = list[0].ID
			prompt = list[0].Prompt
			return nil
		}
		s, err := q.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		prompt = s.Prompt
		return nil
	}); err != nil {
		return err
	}

	var opts eval.Options
	if o.citations {
		// The same fetch and extract path the actor used. A different
		// extractor would disagree on whitespace and report mismatches that
		// are artefacts of the harness rather than faults in the claim.
		//
		// The cassette is named from the session's prompt, which is what
		// `mole research` recorded under. Opening a separate eval-scoped one
		// would miss every interaction and report the whole run unreachable.
		rec, err := record.FromEnv(prompt)
		if err != nil {
			return err
		}
		defer func() { _ = rec.Close() }()

		opts.Citations = eval.NewPipelineReader(
			fetch.NewHTTP(fetch.Config{UserAgent: userAgent()}, fetch.Options{
				Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
				Transport: rec.Wrap,
			}),
			extract.New(),
		)
	}

	card, err := eval.Score(ctx, db, sessionID, opts)
	if err != nil {
		return err
	}

	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(card); err != nil {
			return err
		}
	} else {
		printScorecard(card, o.verbose)
	}

	if card.Failed() {
		// Non-zero so CI fails on it. This is a real regression, not a low
		// score: budget was exceeded, the ledger disagrees with itself, a hold
		// was stranded, or a malformed claim was persisted.
		return exitWith(1, errors.New("scorecard has regressions"))
	}
	return nil
}

func printScorecard(card eval.Scorecard, verbose bool) {
	fmt.Printf("eval %s\n\n", card.SessionID)

	var blocked []eval.Metric
	for _, m := range card.Metrics {
		if m.Status == eval.Blocked {
			blocked = append(blocked, m)
			continue
		}

		mark := " "
		value := "—"
		if m.Status == eval.Measured {
			mark = "✓"
			value = formatMetric(m)
		}
		if m.Regression {
			mark = "✗"
		}
		fmt.Printf(" %s %-22s %10s   %s\n", mark, m.Name, value, m.Detail)
	}

	if len(blocked) > 0 {
		fmt.Printf("\n %d of %d metrics not measured yet:\n", len(blocked), len(card.Metrics))
		for _, m := range blocked {
			if verbose {
				fmt.Printf("   · %-22s %s\n", m.Name, wrapAt(m.Reason, 60, strings.Repeat(" ", 28)))
			} else {
				fmt.Printf("   · %s\n", m.Name)
			}
		}
		if !verbose {
			fmt.Println("   (--verbose for what each is waiting on)")
		}
	}

	fmt.Println()
	if card.Failed() {
		fmt.Println(" FAILED — the ✗ lines above are contract violations, not low scores.")
		return
	}
	fmt.Println(" passed — every mechanical check holds.")
}

func formatMetric(m eval.Metric) string {
	switch m.Unit {
	case "%":
		return fmt.Sprintf("%.1f%%", m.Value)
	case "ok":
		if m.Value == 1 {
			return "ok"
		}
		return "DRIFT"
	case "calls", "held":
		return fmt.Sprintf("%.0f", m.Value)
	default:
		// A budget unit: render it the way the rest of the CLI does.
		return fmt.Sprintf("%.0f %s", m.Value, m.Unit)
	}
}
