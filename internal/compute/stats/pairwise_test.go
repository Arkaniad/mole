package stats_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/stats"
)

// The k-group comparison and its correction.
//
// The property that matters is not that Holm is implemented — it is that a result
// which would be called significant on its own STOPS being called significant when
// it is one of fifteen. That is the whole reason the gate used to refuse to compare
// more than two groups.

// The fixtures use `repeated` from stats_test.go: n values whose mean and SAMPLE
// variance are exactly what they say, so a p-value here can be checked by hand.

// TestABorderlineResultLosesSignificanceUnderManyComparisons.
//
// Constructed so the raw p sits just under alpha. Alone it is significant; as one
// of several it is not, which is the arithmetic the old two-group restriction was
// protecting against.
func TestABorderlineResultLosesSignificanceUnderManyComparisons(t *testing.T) {
	// A separation chosen to land the raw p just inside 0.05 at these sizes.
	a := repeated("a", 100, 20, 30)
	b := repeated("b", 111, 20, 30)

	alone, err := stats.WelchOrReason("the mean", a, b)
	if err != nil {
		t.Fatal(err)
	}
	if alone.P >= stats.Alpha || alone.P < stats.Alpha/10 {
		t.Fatalf("fixture p = %v; the test needs a p just inside alpha (%v) to be "+
			"meaningful", alone.P, stats.Alpha)
	}
	if alone.Verdict != stats.Significant {
		t.Fatalf("verdict alone = %q, want significant", alone.Verdict)
	}

	// The same pair, plus four groups that differ from nothing. Six groups is
	// fifteen comparisons.
	groups := []stats.Group{a, b}
	for i := 0; i < 4; i++ {
		groups = append(groups, repeated(fmt.Sprintf("null%d", i), 105.5, 20, 30))
	}
	tests, note := stats.Pairwise("the mean", groups)
	if len(tests) != 15 {
		t.Fatalf("%d comparisons, want 15", len(tests))
	}

	var found bool
	for _, tr := range tests {
		if (tr.GroupA == "a" && tr.GroupB == "b") || (tr.GroupA == "b" && tr.GroupB == "a") {
			found = true
			if tr.P != alone.P {
				t.Errorf("the raw p changed: %v vs %v", tr.P, alone.P)
			}
			if tr.PAdjusted <= tr.P {
				t.Errorf("adjusted p %v did not rise above the raw p %v", tr.PAdjusted, tr.P)
			}
			if tr.Verdict == stats.Significant {
				t.Errorf("a borderline result is still significant as one of 15 "+
					"comparisons (raw %v, adjusted %v)", tr.P, tr.PAdjusted)
			}
		}
	}
	if !found {
		t.Fatal("the pair under test was not compared")
	}
	if !strings.Contains(note, "Holm-adjusted") {
		t.Errorf("note = %q, want it to say the p-values were adjusted", note)
	}
}

// TestOneComparisonIsTheIdentity.
//
// This is what makes the two-group and k-group paths the same code: if a single
// comparison were adjusted, every existing two-group result would have shifted.
func TestOneComparisonIsTheIdentity(t *testing.T) {
	a := repeated("a", 100, 5, 40)
	b := repeated("b", 40, 5, 40)

	direct, err := stats.WelchOrReason("the mean", a, b)
	if err != nil {
		t.Fatal(err)
	}
	tests, note := stats.Pairwise("the mean", []stats.Group{a, b})
	if len(tests) != 1 {
		t.Fatalf("%d tests, want 1", len(tests))
	}
	if tests[0].PAdjusted != direct.P {
		t.Errorf("adjusted p = %v, want the raw p %v for a single comparison",
			tests[0].PAdjusted, direct.P)
	}
	if tests[0].Verdict != direct.Verdict {
		t.Errorf("verdict = %q, want %q", tests[0].Verdict, direct.Verdict)
	}
	if strings.Contains(note, "Holm") {
		t.Errorf("a single comparison claims a correction: %q", note)
	}
	// And the sentence does not mention a correction that did not happen.
	if strings.Contains(tests[0].Summary, "pairwise comparisons") {
		t.Errorf("summary mentions multiple comparisons: %q", tests[0].Summary)
	}
}

// TestHolmIsMonotoneAndBounded.
//
// Two properties of the step-down procedure that are easy to get wrong: a larger
// raw p must never produce a smaller adjusted one, and no adjusted value may
// exceed 1 — "adjusted p = 3.4" in a passage a model quotes reads as a number
// rather than as arithmetic.
func TestHolmIsMonotoneAndBounded(t *testing.T) {
	raw := []float64{0.001, 0.008, 0.039, 0.041, 0.6, 0.9}
	tests := make([]stats.Test, len(raw))
	for i, p := range raw {
		tests[i] = stats.Test{P: p, NA: 30, NB: 30}
	}
	stats.Holm(tests)

	var prev float64
	for i, tr := range tests {
		if tr.PAdjusted > 1 {
			t.Errorf("adjusted p %v exceeds 1", tr.PAdjusted)
		}
		if tr.PAdjusted < tr.P {
			t.Errorf("adjusted p %v is below the raw p %v", tr.PAdjusted, tr.P)
		}
		if i > 0 && tr.PAdjusted < prev {
			t.Errorf("adjusted p fell from %v to %v as the raw p rose", prev, tr.PAdjusted)
		}
		prev = tr.PAdjusted
	}
	// The textbook first step: the smallest of m p-values is multiplied by m.
	if got, want := tests[0].PAdjusted, 0.006; math.Abs(got-want) > 1e-12 {
		t.Errorf("smallest adjusted = %v, want %v (m × p)", got, want)
	}
	// And the largest is capped rather than 6 × 0.9.
	if tests[len(tests)-1].PAdjusted != 1 {
		t.Errorf("largest adjusted = %v, want 1", tests[len(tests)-1].PAdjusted)
	}
}

// TestTooManyGroupsAreCappedAndSaidSo.
//
// Past six groups the correction eats the power the extra comparisons would add,
// so the largest are compared and the rest are named as untested. Silently
// dropping them would be the old behaviour without the old honesty.
func TestTooManyGroupsAreCappedAndSaidSo(t *testing.T) {
	var groups []stats.Group
	for i := 0; i < 9; i++ {
		// Descending sizes, so the cap has an order to respect.
		groups = append(groups, repeated(fmt.Sprintf("g%d", i), 100+float64(i), 10, 100-i*5))
	}
	tests, note := stats.Pairwise("the mean", groups)

	want := stats.MaxGroups * (stats.MaxGroups - 1) / 2
	if len(tests) != want {
		t.Fatalf("%d comparisons over 9 groups, want %d (the %d largest)",
			len(tests), want, stats.MaxGroups)
	}
	for _, tr := range tests {
		for _, dropped := range []string{"g6", "g7", "g8"} {
			if tr.GroupA == dropped || tr.GroupB == dropped {
				t.Errorf("%s was compared despite being outside the largest %d",
					dropped, stats.MaxGroups)
			}
		}
	}
	if !strings.Contains(note, "not tested") {
		t.Errorf("note = %q, want it to say which groups were left out", note)
	}
}

// TestAGroupThatCannotSupportATestIsSkippedNotCounted.
//
// Correcting for comparisons that never ran would inflate every p for nothing.
func TestAGroupThatCannotSupportATestIsSkippedNotCounted(t *testing.T) {
	groups := []stats.Group{
		repeated("a", 100, 5, 40),
		repeated("b", 40, 5, 40),
		// One record: no variance, no test, and no business raising the
		// correction for the pair that CAN be tested.
		{Name: "single", N: 1, Sum: 70, SumSq: 4900},
	}
	tests, _ := stats.Pairwise("the mean", groups)
	if len(tests) != 1 {
		t.Fatalf("%d tests, want 1 — the untestable group must not add comparisons: %+v",
			len(tests), tests)
	}
	if tests[0].Comparisons != 1 {
		t.Errorf("comparisons = %d, want 1", tests[0].Comparisons)
	}
}
