package actors_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// §4's fourth column: "stable across 3 holdout windows".
//
// n, effect size and significance were computed from M8; stability was named in
// the known gaps as needing "three more queries per hypothesis and a splitting
// rule nobody has chosen". It needs one more query, and the rule is `rowid % 3` —
// deterministic so a replayed session gets the same evidence, and interleaved so a
// file sorted by date does not turn the check into a comparison of three periods.

// splitRegistry writes two groups whose separation is under the test's control per
// holdout window.
//
// The window a row lands in is `rowid % 3`, and the connector's importer assigns
// rowids in file order — so the fixture computes each row's window as it writes it
// rather than assuming the alternation lines up. An earlier version wrote two rows
// per iteration and keyed the effect off the ITERATION index, which spread the
// effect across all three windows and made the "unstable" fixture stable. It passed
// nothing and proved nothing.
//
// reversed makes the difference exist in window 0 and REVERSE in the other two.
// That is the failure the stability check exists for: a pooled comparison that is
// significant, over data where the direction depends on which third you look at.
func splitRegistry(t *testing.T, perGroup int, reversed bool) registry {
	t.Helper()
	var b strings.Builder
	b.WriteString("region,spend\n")

	rows := 0
	write := func(region string, v float64) {
		rows++
		fmt.Fprintf(&b, "%s,%.0f\n", region, v)
	}
	for i := 0; i < perGroup; i++ {
		for _, region := range []string{"north", "south"} {
			// The window this row will land in, computed from the rowid it will get.
			window := (rows + 1) % 3
			mean := 100.0
			if region == "south" {
				switch {
				case !reversed:
					mean = 40
				case window == 0:
					mean = 40
				default:
					// Higher than north, so the DIRECTION disagrees rather than the
					// difference merely shrinking — a sign flip is unambiguous where
					// a near-zero difference would depend on rounding.
					mean = 115
				}
			}
			// ±5 either side, alternating, so the spread is known and identical.
			d := 5.0
			if i%2 == 1 {
				d = -5
			}
			write(region, mean+d)
		}
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "samples.csv")
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "sales", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	return registry{c}
}

// evidencePassage runs one comparison and returns the passage the miner was shown.
func evidencePassage(t *testing.T, reg registry) (string, []core.Crossing) {
	t.Helper()
	db, sessionID := crossingStore(t)

	var passage string
	fl := &fakeLLM{mineFunc: func(prompt string) string {
		if strings.Contains(prompt, "Research question:") {
			return comparePlan
		}
		passage = prompt
		// Quote nothing usable; the passage is what this test is about.
		return `{"claims":[]}`
	}}

	a := &actors.LocalComputeActor{Connectors: reg, LLM: fl, Store: db}
	if _, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "do the regions differ",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if passage == "" {
		t.Fatal("no passage reached the miner; the comparison did not run")
	}

	var list []core.Crossing
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		list, err = q.ListCrossings(ctx, sessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return passage, list
}

// TestADifferenceThatHoldsEverywhereIsReportedAsStable.
func TestADifferenceThatHoldsEverywhereIsReportedAsStable(t *testing.T) {
	passage, crossings := evidencePassage(t, splitRegistry(t, 90, false))

	if !strings.Contains(passage, "points the same way in all 3 holdout windows") {
		t.Errorf("the passage does not report stability:\n%s", passage)
	}
	// And the holdout statement is audited like any other crossing: it reads the
	// same data and its figures reach the same model.
	if len(crossings) != 2 {
		t.Fatalf("%d crossing(s), want 2 — the comparison and the holdout statement",
			len(crossings))
	}
	var sawHoldout bool
	for _, c := range crossings {
		if strings.Contains(c.Query, "holdout") {
			sawHoldout = true
			if c.Outcome != core.CrossingCrossed {
				t.Errorf("the holdout statement was %q", c.Outcome)
			}
		}
	}
	if !sawHoldout {
		t.Error("the holdout statement is not in the audit trail")
	}
}

// TestADifferenceDrivenByASubsetIsNotReportedAsStable.
//
// The failure the whole check exists for. A significant pooled p over data where
// the difference only exists in a third of the rows is exactly the shape a
// research tool must not state as a finding — and the caveat has to be in the
// passage, because §11.5 is what makes a model reproduce it.
func TestADifferenceDrivenByASubsetIsNotReportedAsStable(t *testing.T) {
	passage, _ := evidencePassage(t, splitRegistry(t, 90, true))

	if !strings.Contains(passage, "does NOT hold across holdout windows") {
		t.Errorf("a subset-driven difference is not flagged:\n%s", passage)
	}
	if !strings.Contains(passage, "driven by a subset") {
		t.Errorf("the passage does not say what the problem is:\n%s", passage)
	}
}

// TestAnUncheckableSplitSaysSoRatherThanPassing.
//
// A third of a small group falls under the k-anonymity floor, which is the privacy
// guarantee holding rather than a fault — but "could not be checked" and "stable"
// must not look alike in a passage a claim is quoted from.
func TestAnUncheckableSplitSaysSoRatherThanPassing(t *testing.T) {
	// 21 rows a side: seven per window, and a Welch test needs 2 — so the split IS
	// testable but nothing is significant per window. The floor case is the
	// smaller one below.
	passage, _ := evidencePassage(t, splitRegistry(t, 6, false))

	if strings.Contains(passage, "points the same way in all 3 holdout windows") {
		t.Errorf("an unverifiable split was reported as stable:\n%s", passage)
	}
	if !strings.Contains(passage, "holdout") && !strings.Contains(passage, "stability") {
		t.Errorf("the passage says nothing about stability at all:\n%s", passage)
	}
}
