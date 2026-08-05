package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/verifier"
	"github.com/spf13/cobra"
)

// Adjudicator test sets (§11.2).
//
// Everything else mole measures about the claim graph counts what was FOUND. Whether it was
// RIGHT needs someone to say what the right answer was, and this is the smallest possible
// way to ask: dump the pairs a real run judged, with both claim texts and the model's own
// rationale, and let a person label them.
//
// Cheaper than §14.2's question corpus and sharper for this purpose. The pairs already
// exist, retrieval is deterministic so they regenerate exactly, and once labelled they
// score any prompt or model change in seconds with no tokens and no network.

func newPairsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pairs",
		Short: "Dump and score adjudicator judgements (§11.2)",
		Long: "Measures whether the claim-graph adjudicator is RIGHT, which no other metric\n" +
			"does — they all count what it found.\n\n" +
			"  mole pairs dump <session-id> -o pairs.json    # then fill in each \"label\"\n" +
			"  mole pairs score pairs.json",
	}
	c.AddCommand(newPairsDumpCmd(), newPairsScoreCmd())
	return c
}

func newPairsDumpCmd() *cobra.Command {
	var (
		out   string
		kinds []string
		all   bool
		cap   int
	)
	c := &cobra.Command{
		Use:   "dump <session-id>",
		Short: "Write a session's judged pairs for labelling",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			db, err := openDBRead(ctx, dbPath(cmd))
			if err != nil {
				return err
			}
			defer db.Close()

			ps, err := eval.DumpPairs(ctx, db, args[0], eval.DumpOptions{
				Kinds: kinds, All: all, MaxCandidatesPerClaim: cap,
			})
			if err != nil {
				return err
			}
			if len(ps.Pairs) == 0 {
				return fmt.Errorf("no pairs matched (session %s)", args[0])
			}

			if out == "" {
				return eval.WritePairs("/dev/stdout", ps)
			}
			if err := eval.WritePairs(out, ps); err != nil {
				return err
			}
			byKind := map[string]int{}
			for _, p := range ps.Pairs {
				byKind[p.Model]++
			}
			fmt.Printf("%d pair(s) written to %s\n", len(ps.Pairs), out)
			for _, k := range sortedKeys(byKind) {
				fmt.Printf("  %-14s %d\n", k, byKind[k])
			}
			fmt.Printf("\nFill in \"label\" on each: %s\n", strings.Join(relationNames(), ", "))
			fmt.Println("Leave it empty to skip a pair; scoring counts those rather than guessing.")
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&out, "out", "o", "", "write here instead of stdout")
	f.StringSliceVar(&kinds, "kind", nil,
		"only these verdicts, e.g. --kind contradicts (default: every pair with an edge)")
	f.BoolVar(&all, "all", false,
		"include pairs the model called unrelated; needed to measure RECALL, and there are many more of them")
	f.IntVar(&cap, "max-candidates", 0, "retrieval cap; 0 uses the Verifier's default")
	return c
}

func newPairsScoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "score <file.json>",
		Short: "Score a labelled pair set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ps, err := eval.LoadPairs(args[0])
			if err != nil {
				return err
			}
			s := eval.ScorePairs(ps)
			printPairScore(s)
			if s.Labelled == 0 {
				return fmt.Errorf("nothing labelled in %s", args[0])
			}
			return nil
		},
	}
	return c
}

func printPairScore(s eval.PairScore) {
	fmt.Printf("adjudicator accuracy: %.0f%%  (%d of %d labelled pair(s))\n",
		100*s.Accuracy(), s.Correct, s.Labelled)
	if s.Unlabelled > 0 {
		// Counted, never guessed at: a score over whichever pairs someone got round to
		// labelling, reported as the score, is how a test set starts lying.
		fmt.Printf("  %d pair(s) unlabelled and excluded\n", s.Unlabelled)
	}
	if s.Skipped > 0 {
		fmt.Printf("  %d pair(s) were never put to the model and say nothing about it\n", s.Skipped)
	}
	if s.Labelled == 0 {
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "\nRELATION\tPRECISION\tRECALL")
	for _, k := range relationNames() {
		p, hasP := s.Precision[k]
		r, hasR := s.Recall[k]
		if !hasP && !hasR {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", k, pctOrDash(p, hasP), pctOrDash(r, hasR))
	}
	_ = w.Flush()

	// The confusion, but only where the model was wrong: what it says instead is the
	// thing that tells you whether to change the prompt or the model.
	var lines []string
	for model, truths := range s.Confusion {
		for truth, n := range truths {
			if model != truth {
				lines = append(lines, fmt.Sprintf("  said %-13s was really %-13s ×%d", model, truth, n))
			}
		}
	}
	if len(lines) > 0 {
		sort.Strings(lines)
		fmt.Println("\nwhere it went wrong:")
		for _, l := range lines {
			fmt.Println(l)
		}
	}
}

func pctOrDash(v float64, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*v)
}

func relationNames() []string {
	return []string{
		string(verifier.RelSupports), string(verifier.RelContradicts),
		string(verifier.RelDuplicate), string(verifier.RelRefines),
		string(verifier.RelUnrelated),
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
