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
		err := cmdEval(ctx, db.Path(), sess.ID, false, false)
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
		if err := cmdEval(ctx, db.Path(), sess.ID, false, false); err != nil {
			t.Errorf("a clean session failed: %v", err)
		}
	})
	if !strings.Contains(out, "passed") {
		t.Errorf("output:\n%s", out)
	}
}

// TestEvalNamesWhatIsNotMeasured. Six of thirteen lines are blocked, and a
// reader who does not know that will take five green ticks as full coverage.
func TestEvalNamesWhatIsNotMeasured(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, core.MicrosPerUSD)

	out := captureStdout(t, func() {
		_ = cmdEval(ctx, db.Path(), sess.ID, false, true)
	})

	for _, want := range []string{
		"not measured yet", "claim precision", "grounding rate",
		"citation accuracy", "contradiction recall", "staleness detection",
		"exfil regression", "M4", "M8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scorecard does not mention %q:\n%s", want, out)
		}
	}
}
