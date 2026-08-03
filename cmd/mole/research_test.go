package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// ---------------------------------------------------------------------------
// Budget resolution
// ---------------------------------------------------------------------------

// TestBudgetFlagsAreMutuallyExclusive. §8 makes the unit semantically
// load-bearing — USD mode cannot bound an unpriced model, token mode cannot
// price a search call — so silently picking one produces a ceiling that does
// not bind.
func TestBudgetFlagsAreMutuallyExclusive(t *testing.T) {
	if _, _, err := resolveBudget("3.00", 50_000, &config.Config{}); err == nil {
		t.Error("--usd and --tokens together were accepted")
	}
}

func TestBudgetFlagsParse(t *testing.T) {
	unit, amount, err := resolveBudget("3.00", 0, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if unit != core.BudgetUSD || amount != 3*core.MicrosPerUSD {
		t.Errorf("got %s/%d, want usd/%d", unit, amount, 3*core.MicrosPerUSD)
	}

	unit, amount, err = resolveBudget("", 50_000, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if unit != core.BudgetTokens || amount != 50_000 {
		t.Errorf("got %s/%d, want tokens/50000", unit, amount)
	}
}

// TestBareResearchCannotRunAway: with no flag and no configured default there
// is no budget, and a research command with no ceiling is the failure mode §8.5
// exists to prevent. Refusing is the only safe answer — a built-in default
// would be a number nobody chose being spent on someone's card.
func TestBareResearchCannotRunAway(t *testing.T) {
	if _, _, err := resolveBudget("", 0, &config.Config{}); err == nil {
		t.Error("a research run with no budget at all was accepted")
	}
}

func TestConfiguredDefaultBudgetIsUsed(t *testing.T) {
	cfg := &config.Config{DefaultBudgetUnit: "usd", DefaultBudgetUSD: "2.50"}
	unit, amount, err := resolveBudget("", 0, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if unit != core.BudgetUSD || amount != 2_500_000 {
		t.Errorf("got %s/%d, want usd/2500000", unit, amount)
	}

	cfg = &config.Config{DefaultBudgetUnit: "tokens", DefaultBudgetToks: 12_000}
	unit, amount, err = resolveBudget("", 0, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if unit != core.BudgetTokens || amount != 12_000 {
		t.Errorf("got %s/%d, want tokens/12000", unit, amount)
	}
}

// TestIncompleteDefaultIsRefusedRatherThanGuessed: a unit set without an amount
// is a half-finished config, and inferring the missing half would produce a
// ceiling the user never chose.
func TestIncompleteDefaultIsRefusedRatherThanGuessed(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"unit without amount":  {DefaultBudgetUnit: "usd"},
		"tokens without count": {DefaultBudgetUnit: "tokens"},
		"unparseable amount":   {DefaultBudgetUnit: "usd", DefaultBudgetUSD: "three dollars"},
		"zero amount":          {DefaultBudgetUnit: "usd", DefaultBudgetUSD: "0"},
	} {
		if _, _, err := resolveBudget("", 0, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNegativeBudgetRefused(t *testing.T) {
	if _, _, err := resolveBudget("-1.00", 0, &config.Config{}); err == nil {
		t.Error("a negative dollar budget was accepted")
	}
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------
//
// The reserve → run → settle tests that lived here moved to internal/executor
// when `mole research` stopped running a single hardcoded lead: the loop owns
// that cycle now, and testing it through the CLI would be testing it through a
// layer that no longer decides anything about it.

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()
	w.Close()
	return <-done
}

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newTestSession(t *testing.T, led *budget.Ledger, unit core.BudgetUnit, amount int64) *core.Session {
	t.Helper()
	sess, err := led.CreateSession(context.Background(), budget.SessionSpec{
		Prompt:     "a question",
		Mode:       core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: unit,
		Budget:     amount,
		MaxLeads:   1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

// TestReportShowsWhatTheRunCostAndWhereItStopped. A reader deciding whether to
// trust the answer needs both: an answer produced by two of nine planned leads
// before the budget ran out is a different thing from a complete one.
func TestReportShowsWhatTheRunCostAndWhereItStopped(t *testing.T) {
	out := &researchOutput{
		SessionID:      "s_test",
		Status:         string(core.StatusExhausted),
		Spent:          184_000,
		LeadsRun:       7,
		LeadsFailed:    2,
		Replans:        2,
		OpenQuestions:  3,
		StoppedBecause: "max_leads",
		Claims: []core.Claim{
			{Text: "A claim.", Source: "https://a.example", Quote: "a quote"},
		},
	}

	got := captureStdout(t, func() {
		printReport(out, nil, core.BudgetUSD, 3*core.MicrosPerUSD, false)
	})

	for _, want := range []string{
		"$0.1840", "7 lead(s), 2 failed", "2 replan(s)",
		"3 sub-question(s) still open", "stopped: max_leads",
		"mole eval s_test", "mole trace s_test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

// TestCompleteRunDoesNotClaimOpenQuestions, so the warning keeps meaning
// something.
func TestCompleteRunDoesNotClaimOpenQuestions(t *testing.T) {
	out := &researchOutput{SessionID: "s_c", Status: string(core.StatusDone), LeadsRun: 3}
	got := captureStdout(t, func() {
		printReport(out, nil, core.BudgetUSD, core.MicrosPerUSD, true)
	})
	if strings.Contains(got, "still open") {
		t.Errorf("a complete run reported open questions:\n%s", got)
	}
}

// TestProgressMarksADegradedRun. More than half the leads failing means the run
// produced something, but not the something it was asked for (§9.5).
func TestProgressMarksADegradedRun(t *testing.T) {
	degraded := captureStdout(t, func() {
		printProgress(&executor.Result{LeadsRun: 6, LeadsFailed: 5})
	})
	if !strings.Contains(degraded, "~") {
		t.Errorf("a mostly-failed run was not marked degraded:\n%s", degraded)
	}

	clean := captureStdout(t, func() {
		printProgress(&executor.Result{LeadsRun: 6, LeadsFailed: 0})
	})
	if !strings.Contains(clean, "✓") {
		t.Errorf("a clean run was not marked clean:\n%s", clean)
	}
}

func TestAmountFormattingMatchesTheUnit(t *testing.T) {
	if got := fmtAmount(core.BudgetUSD, 3*core.MicrosPerUSD); !strings.HasPrefix(got, "$3.") {
		t.Errorf("USD formatted as %q", got)
	}
	if got := fmtAmount(core.BudgetTokens, 50_000); got != "50000 tok" {
		t.Errorf("tokens formatted as %q, want \"50000 tok\"", got)
	}
}
