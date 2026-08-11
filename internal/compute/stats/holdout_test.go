package stats_test

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/stats"
)

// §4's stability rule, at the unit level.
//
// The rule is DIRECTION, not significance per window: a third of the rows has a
// third of the power, so requiring each window to clear alpha would mark almost
// every real effect unstable and would report a statement about sample size as one
// about the data.

func pooled(t *testing.T, a, b stats.Group) stats.Test {
	t.Helper()
	test, err := stats.WelchOrReason("the mean", a, b)
	if err != nil {
		t.Fatal(err)
	}
	return test
}

func windows(mk func(i int) stats.Group) map[string]stats.Group {
	out := map[string]stats.Group{}
	for i := 0; i < stats.HoldoutWindows; i++ {
		out[string(rune('0'+i))] = mk(i)
	}
	return out
}

// TestADifferenceInEveryWindowIsStable, even where no single window clears alpha.
func TestADifferenceInEveryWindowIsStable(t *testing.T) {
	a := repeated("north", 100, 20, 60)
	b := repeated("south", 88, 20, 60)
	p := pooled(t, a, b)

	// A twelfth of the separation per window, at a fifth of the sample: the
	// direction holds and no window is individually significant.
	h := stats.CheckStability("the mean",
		p,
		windows(func(int) stats.Group { return repeated("north", 100, 20, 12) }),
		windows(func(int) stats.Group { return repeated("south", 92, 20, 12) }))

	if !h.Stable {
		t.Errorf("stable = false over three agreeing windows: %+v", h)
	}
	if h.Windows != stats.HoldoutWindows || h.Agreed != stats.HoldoutWindows {
		t.Errorf("windows/agreed = %d/%d, want 3/3", h.Windows, h.Agreed)
	}
	if h.Significant != 0 {
		t.Errorf("significant = %d; the fixture is not exercising the direction rule",
			h.Significant)
	}
	if !strings.Contains(h.Clause, "all 3 holdout windows") {
		t.Errorf("clause = %q", h.Clause)
	}
}

// TestASignFlipInOneWindowIsNotStable.
func TestASignFlipInOneWindowIsNotStable(t *testing.T) {
	a := repeated("north", 100, 10, 60)
	b := repeated("south", 60, 10, 60)
	p := pooled(t, a, b)

	i := 0
	h := stats.CheckStability("the mean", p,
		windows(func(int) stats.Group { return repeated("north", 100, 10, 20) }),
		windows(func(int) stats.Group {
			i++
			if i == 2 {
				// Higher than north in one window.
				return repeated("south", 130, 10, 20)
			}
			return repeated("south", 60, 10, 20)
		}))

	if h.Stable {
		t.Error("a sign flip in one window was reported as stable")
	}
	if h.Agreed != 2 || h.Windows != 3 {
		t.Errorf("agreed/windows = %d/%d, want 2/3", h.Agreed, h.Windows)
	}
	if !strings.Contains(h.Clause, "does NOT hold") {
		t.Errorf("clause = %q, want it to say the difference does not hold", h.Clause)
	}
	if !strings.Contains(h.Clause, "driven by a subset") {
		t.Errorf("clause = %q, want it to say what that means", h.Clause)
	}
}

// TestAnUntestableWindowIsNotAPass.
//
// "Could not be checked" and "stable" must not look alike in a passage a claim is
// quoted from — a missing check read as a passed one is the failure mode that makes
// a caveat worse than none.
func TestAnUntestableWindowIsNotAPass(t *testing.T) {
	a := repeated("north", 100, 10, 60)
	b := repeated("south", 60, 10, 60)
	p := pooled(t, a, b)

	// Only two windows have both sides.
	as := map[string]stats.Group{
		"0": repeated("north", 100, 10, 20),
		"1": repeated("north", 100, 10, 20),
		"2": repeated("north", 100, 10, 20),
	}
	bs := map[string]stats.Group{
		"0": repeated("south", 60, 10, 20),
		"1": repeated("south", 60, 10, 20),
	}
	h := stats.CheckStability("the mean", p, as, bs)

	if h.Stable {
		t.Error("two windows out of three was reported as stable")
	}
	if h.Windows != 2 {
		t.Errorf("windows = %d, want 2", h.Windows)
	}
	if !strings.Contains(h.Clause, "only 2 of 3") {
		t.Errorf("clause = %q, want it to say how many were testable", h.Clause)
	}

	// And nothing testable at all.
	none := stats.CheckStability("the mean", p, as, map[string]stats.Group{})
	if none.Stable || !strings.Contains(none.Clause, "could NOT be checked") {
		t.Errorf("clause = %q, want an explicit could-not-check", none.Clause)
	}
}

// TestTheClauseReachesTheSentence. §11.5 makes a claim reproduce the passage
// verbatim, which is the only lever that stops a model reporting a subset-driven
// difference as a finding.
func TestTheClauseReachesTheSentence(t *testing.T) {
	a := repeated("north", 100, 10, 60)
	b := repeated("south", 60, 10, 60)
	p := pooled(t, a, b)
	before := p.Summary

	h := stats.CheckStability("the mean", p,
		windows(func(int) stats.Group { return repeated("north", 100, 10, 20) }),
		windows(func(int) stats.Group { return repeated("south", 60, 10, 20) }))
	got := stats.WithHoldout(p, h)

	if got.Holdout == nil {
		t.Fatal("the result is not attached")
	}
	if got.Summary == before {
		t.Error("the sentence a claim must quote does not mention stability")
	}
	if !strings.Contains(got.Summary, h.Clause) {
		t.Errorf("summary = %q, want it to carry %q", got.Summary, h.Clause)
	}
}
