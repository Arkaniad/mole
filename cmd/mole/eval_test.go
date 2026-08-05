package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
)

// TestEvalExitsNonZeroOnARegression. CI runs this command directly, so the exit
// code is the whole interface. A scorecard that prints ✗ and returns success is
// a check that does not check anything — the same failure `doctor` had.
func TestEvalExitsNonZeroOnARegression(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, core.MicrosPerUSD)

	// Strand a reservation: budget that is neither spent nor available.
	if _, err := led.Reserve(ctx, sess.ID, 400_000); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		err := cmdEval(ctx, db.Path(), sess.ID, evalOpts{})
		if err == nil {
			t.Error("a session with a stranded hold scored as passing")
			return
		}
		var ec exitCoder
		if !errors.As(err, &ec) || ec.ExitCode() != 1 {
			t.Errorf("err = %v; want an exit code of 1 so CI fails", err)
		}
	})

	if !strings.Contains(out, "✗") {
		t.Errorf("the failing metric was not marked:\n%s", out)
	}
	if !strings.Contains(out, "FAILED") {
		t.Errorf("output does not say the scorecard failed:\n%s", out)
	}
}

// TestEvalExitsZeroOnACleanSession so a non-zero exit keeps meaning something.
func TestEvalExitsZeroOnACleanSession(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, core.MicrosPerUSD)

	r, err := led.Reserve(ctx, sess.ID, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.Settle(ctx, r, []core.ToolCall{{
		Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 5_000},
	}}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := cmdEval(ctx, db.Path(), sess.ID, evalOpts{}); err != nil {
			t.Errorf("a clean session failed: %v", err)
		}
	})
	if !strings.Contains(out, "passed") {
		t.Errorf("output:\n%s", out)
	}
}

// TestEvalNamesWhatIsNotMeasured. A reader who does not know which lines are blocked
// will take the green ticks as full coverage.
//
// It asserts on the missing COMPONENT rather than a milestone label. The first version
// required the string "M4", which was correct until M4 landed and then failed for the
// right reason: contradiction recall and staleness detection are no longer waiting on
// the Verifier, they are waiting on labelled data. A test pinned to a milestone name
// goes stale exactly when the milestone ships.
func TestEvalNamesWhatIsNotMeasured(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, core.MicrosPerUSD)

	out := captureStdout(t, func() {
		_ = cmdEval(ctx, db.Path(), sess.ID, evalOpts{verbose: true})
	})

	for _, want := range []string{
		"not measured yet", "claim precision", "grounding rate",
		"citation accuracy", "contradiction recall", "staleness detection",
		"exfil regression",
		// What each is actually waiting for.
		"§14.2", "aggregation gate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scorecard does not mention %q:\n%s", want, out)
		}
	}
}
