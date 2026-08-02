package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
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
// The reserve → run → settle cycle
// ---------------------------------------------------------------------------

// stubActor stands in for WebActor so the ledger path can be tested without a
// network, a key, or a model. runOneLead takes the Actor interface precisely so
// this is possible.
type stubActor struct {
	costs  []core.ToolCall
	claims []core.Claim
	err    error
	gotCtx context.Context
}

func (s *stubActor) Type() core.ActorType { return core.ActorWeb }

func (s *stubActor) Run(ctx context.Context, lead core.Lead) (*actors.Result, error) {
	s.gotCtx = ctx
	return &actors.Result{
		Summary: "A summary of what the sources said.",
		Claims:  s.claims,
		Costs:   s.costs,
		Stats:   actors.RunStats{Fetched: 2, SkippedFetch: 1, Chunks: 3},
	}, s.err
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

// TestSpendIsLedgeredAndReconciles is the M1 deliverable's second half: a
// budget that can be reconciled. Settle has to leave the materialized counters
// agreeing with the append-only rows, or the ceiling is enforced against a
// number nothing checks.
func TestSpendIsLedgeredAndReconciles(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, 3*core.MicrosPerUSD)

	actor := &stubActor{
		costs: []core.ToolCall{
			{Role: core.RoleExecutor, Type: core.CallSearch, Cost: core.Cost{USDMicros: 5_000}},
			{Role: core.RoleExecutor, Type: core.CallLLM, Model: "claude-haiku-4-5",
				Cost: core.Cost{USDMicros: 41_000, InputTokens: 12_000, OutputTokens: 900}},
		},
		claims: []core.Claim{{Text: "A claim.", Source: "https://a.example/x", Quote: "evidence"}},
	}

	out := &researchOutput{}
	settled, err := runOneLead(ctx, db, led, actor, sess, "a question", out, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if settled.Charged != 46_000 {
		t.Errorf("charged %d micros, want 46000", settled.Charged)
	}

	v, err := led.Verify(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Consistent() {
		t.Errorf("ledger does not reconcile: spent recorded=%d ledger=%d, held recorded=%d rows=%d",
			v.SpentRecorded, v.SpentFromLedger, v.HeldRecorded, v.HeldFromRows)
	}
	if v.SpentFromLedger != 46_000 {
		t.Errorf("ledger total = %d, want 46000", v.SpentFromLedger)
	}
}

// TestNothingIsHeldAfterTheRun: a reservation left held is budget that can
// never be spent and never released. It is the leak that makes a long session
// run out of money it never used.
func TestNothingIsHeldAfterTheRun(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, 3*core.MicrosPerUSD)

	actor := &stubActor{costs: []core.ToolCall{
		{Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 1_000}},
	}}
	if _, err := runOneLead(ctx, db, led, actor, sess, "q", &researchOutput{}, true); err != nil {
		t.Fatal(err)
	}

	var after *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		after, err = q.GetSession(ctx, sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.Held != 0 {
		t.Errorf("%d still held after settle", after.Held)
	}
}

// TestFailedRunIsStillCharged. The tokens were billed whether or not the lead
// produced anything, and a ledger that records only successes cannot enforce a
// ceiling — a session that fails repeatedly would spend without limit.
func TestFailedRunIsStillCharged(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, 3*core.MicrosPerUSD)

	actor := &stubActor{
		err: context.DeadlineExceeded,
		costs: []core.ToolCall{
			{Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 22_000}},
		},
	}

	settled, err := runOneLead(ctx, db, led, actor, sess, "q", &researchOutput{}, true)
	if err == nil {
		t.Error("the failure was swallowed")
	}
	if settled == nil || settled.Charged != 22_000 {
		t.Fatalf("failed run charged %v, want 22000", settled)
	}

	v, _ := led.Verify(ctx, sess.ID)
	if !v.Consistent() {
		t.Error("ledger does not reconcile after a failed run")
	}
}

// TestReservationIsClampedToWhatRemains: a budget smaller than the estimator's
// seed should buy a small run, not an error. Escrow is already held back, so
// the clamp has to respect Available rather than Budget.
func TestReservationIsClampedToWhatRemains(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	// Far below the estimator seed for a web lead.
	sess := newTestSession(t, led, core.BudgetUSD, 20_000)

	actor := &stubActor{costs: []core.ToolCall{
		{Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 1_000}},
	}}
	settled, err := runOneLead(ctx, db, led, actor, sess, "q", &researchOutput{}, true)
	if err != nil {
		t.Fatalf("a small budget failed instead of buying a small run: %v", err)
	}
	if settled.Reserved > sess.Available() {
		t.Errorf("reserved %d against %d available", settled.Reserved, sess.Available())
	}
}

// TestTokenModeChargesTokensNotDollars. The two units are not interchangeable:
// in token mode a search call priced only in dollars contributes nothing, which
// is a real property of the mode and not a bug — but the model tokens must land.
func TestTokenModeChargesTokensNotDollars(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetTokens, 100_000)

	actor := &stubActor{costs: []core.ToolCall{
		{Role: core.RoleExecutor, Type: core.CallSearch, Cost: core.Cost{USDMicros: 5_000}},
		{Role: core.RoleExecutor, Type: core.CallLLM,
			Cost: core.Cost{USDMicros: 41_000, InputTokens: 12_000, OutputTokens: 900}},
	}}

	settled, err := runOneLead(ctx, db, led, actor, sess, "q", &researchOutput{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Charged != 12_900 {
		t.Errorf("charged %d tokens, want 12900 (the dollar-only search call contributes none)", settled.Charged)
	}
}

// TestLeadIsPersisted: `mole trace` reads leads from the database, so a lead
// that only ever existed in memory makes the trace view lie about the session.
func TestLeadIsPersisted(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, 3*core.MicrosPerUSD)

	actor := &stubActor{costs: []core.ToolCall{
		{Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 1_000}},
	}}
	if _, err := runOneLead(ctx, db, led, actor, sess, "the original question", &researchOutput{}, true); err != nil {
		t.Fatal(err)
	}

	var leads []*core.Lead
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		leads, err = q.ListLeads(ctx, sess.ID, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(leads) != 1 {
		t.Fatalf("%d leads persisted, want 1", len(leads))
	}
	if leads[0].Query != "the original question" {
		t.Errorf("lead query = %q, want the question verbatim", leads[0].Query)
	}
}

// TestActorSeesTheDeadline: the wall-clock ceiling is unit-independent (§8.5)
// and binds even when the cost estimate is wrong. It only works if it reaches
// the actor.
func TestActorSeesTheDeadline(t *testing.T) {
	db := newTestDB(t)
	led := budget.New(db, budget.DefaultConfig())
	sess := newTestSession(t, led, core.BudgetUSD, 3*core.MicrosPerUSD)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	actor := &stubActor{}
	if _, err := runOneLead(ctx, db, led, actor, sess, "q", &researchOutput{}, true); err != nil {
		t.Fatal(err)
	}
	if actor.gotCtx == nil {
		t.Fatal("the actor was never run")
	}
	if _, ok := actor.gotCtx.Deadline(); !ok {
		t.Error("the actor ran without a deadline")
	}
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// TestSourcesAreNumberedInFirstSeenOrder: citations are only readable if [1] is
// the first source a reader meets and stays [1] everywhere it appears.
func TestSourcesAreNumberedInFirstSeenOrder(t *testing.T) {
	claims := []core.Claim{
		{Text: "a", Source: "https://one.example"},
		{Text: "b", Source: "https://two.example"},
		{Text: "c", Source: "https://one.example"},
	}
	ordered, index := numberSources(claims)

	if len(ordered) != 2 {
		t.Fatalf("%d distinct sources, want 2", len(ordered))
	}
	if index["https://one.example"] != 1 || index["https://two.example"] != 2 {
		t.Errorf("numbering = %v, want one=1 two=2", index)
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

func TestEllipsizeCollapsesAndTruncates(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := ellipsize(long, 40)
	if len(got) > 44 {
		t.Errorf("ellipsize returned %d bytes for a 40-byte limit", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated text is not marked as truncated")
	}
	if strings.Contains(ellipsize("a  \n  b", 40), "\n") {
		t.Error("newlines survived into a single-line quote")
	}
	if got := ellipsize("short", 40); got != "short" {
		t.Errorf("short text was modified: %q", got)
	}
}

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

// TestReportCitesEverySourceItPrints. A claim without a resolvable citation is
// the failure this whole pipeline exists to avoid — §11.5 verifies the quote,
// and the report is where that work becomes visible to a reader.
func TestReportCitesEverySourceItPrints(t *testing.T) {
	out := &researchOutput{
		SessionID: "s_test",
		Status:    string(core.StatusDone),
		Spent:     46_000,
		Summary:   "The sources broadly agree.",
		Claims: []core.Claim{
			{Text: "MambaByte reports 1.31 BPB on PG-19.", Source: "https://arxiv.org/abs/2401.13660",
				Quote: "It achieves 1.31 bits per byte on the PG-19 benchmark"},
			{Text: "Inference is 2.6x faster.", Source: "https://arxiv.org/abs/2401.13660",
				Quote: "roughly 2.6 times faster than a comparable subword model"},
			{Text: "Byte models remove tokenizer bias.", Source: "https://example.com/blog",
				Quote: "byte-level modelling removes tokenizer bias entirely"},
		},
		Stats: actors.RunStats{Fetched: 2, SkippedFetch: 1, Chunks: 4,
			ClaimsProposed: 5, ClaimsRejected: 2},
	}

	got := captureStdout(t, func() {
		printReport(out, core.BudgetUSD, 3*core.MicrosPerUSD, false)
	})

	// Every claim carries a marker, and every marker resolves to a listed URL.
	for i, c := range out.Claims {
		if !strings.Contains(got, c.Text) {
			t.Errorf("claim %d is missing from the report", i)
		}
	}
	for _, want := range []string{
		"[1] https://arxiv.org/abs/2401.13660",
		"[2] https://example.com/blog",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("source list is missing %q", want)
		}
	}
	// Two claims share source [1], so [3] must not exist.
	if strings.Contains(got, "[3]") {
		t.Error("a marker was emitted with no matching source")
	}

	// The counters a reader needs to trust the output.
	if !strings.Contains(got, "$0.0460") {
		t.Errorf("spend is not reported:\n%s", got)
	}
	if !strings.Contains(got, "2 of 5 proposed claims rejected") {
		t.Error("the quote-verification rejection count is not surfaced")
	}
	if !strings.Contains(got, "mole trace s_test") {
		t.Error("the report does not say how to see the cost breakdown")
	}
}

// TestReportSurfacesTruncation: silently dropping half a document and printing
// a confident summary is worse than saying the budget ran out (§4.1).
func TestReportSurfacesTruncation(t *testing.T) {
	out := &researchOutput{SessionID: "s_x", Truncated: true, Summary: "Partial."}
	got := captureStdout(t, func() {
		printReport(out, core.BudgetUSD, core.MicrosPerUSD, true)
	})
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation was not reported:\n%s", got)
	}
}

// TestReportWithNoClaimsSaysSo rather than printing an empty Claims heading.
func TestReportWithNoClaimsSaysSo(t *testing.T) {
	out := &researchOutput{SessionID: "s_y", Summary: "No usable sources found."}
	got := captureStdout(t, func() {
		printReport(out, core.BudgetUSD, core.MicrosPerUSD, true)
	})
	if strings.Contains(got, "Sources\n") {
		t.Error("an empty source list was printed")
	}
	if !strings.Contains(got, "claims 0") {
		t.Errorf("the zero-claim result is not stated:\n%s", got)
	}
}
