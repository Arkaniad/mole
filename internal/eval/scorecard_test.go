package eval_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

type fixture struct {
	db   *sqlite.DB
	led  *budget.Ledger
	sess *core.Session
	lead core.Lead
}

func newFixture(t *testing.T, budgetAmount int64) *fixture {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "a question", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: budgetAmount,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	lead := core.Lead{
		ID: core.NewLeadID(), SessionID: sess.ID,
		ActorType: core.ActorWeb, Query: "a question", Status: core.LeadQueued,
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertLead(ctx, &lead)
	}); err != nil {
		t.Fatalf("insert lead: %v", err)
	}

	return &fixture{db: db, led: led, sess: sess, lead: lead}
}

// spend runs a reservation through to settle, so the ledger reflects it the way
// a real run would.
func (f *fixture) spend(t *testing.T, reserve, actual int64) {
	t.Helper()
	ctx := context.Background()
	r, err := f.led.ReserveFor(ctx, f.sess.ID, f.lead.ID, reserve)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := f.led.Settle(ctx, r, []core.ToolCall{{
		Role: core.RoleExecutor, Type: core.CallLLM, Model: "m",
		Cost: core.Cost{USDMicros: actual, InputTokens: 100, OutputTokens: 10},
	}}); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func (f *fixture) addClaims(t *testing.T, claims ...core.Claim) {
	t.Helper()
	for i := range claims {
		claims[i].SessionID = f.sess.ID
		claims[i].LeadID = f.lead.ID
		if claims[i].RetrievedAt.IsZero() {
			claims[i].RetrievedAt = time.Now().UTC()
		}
	}
	if err := f.db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, claims)
	}); err != nil {
		t.Fatalf("insert claims: %v", err)
	}
}

func (f *fixture) score(t *testing.T) eval.Scorecard {
	t.Helper()
	card, err := eval.Score(context.Background(), f.db, f.sess.ID, eval.Options{})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	return card
}

func metric(t *testing.T, card eval.Scorecard, name string) eval.Metric {
	t.Helper()
	for _, m := range card.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("scorecard has no metric %q", name)
	return eval.Metric{}
}

func goodClaim(text, source string) core.Claim {
	return core.Claim{
		Text:        text,
		Source:      source,
		Quote:       "a quote long enough to constitute actual evidence",
		QuoteOffset: 10,
		Confidence:  0.8,
	}
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestCleanSessionPasses(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 40_000)
	f.addClaims(t,
		goodClaim("First claim.", "https://a.example/x"),
		goodClaim("Second claim.", "https://b.example/y"),
	)

	card := f.score(t)
	if card.Failed() {
		for _, m := range card.Metrics {
			if m.Regression {
				t.Errorf("unexpected regression: %s — %s", m.Name, m.Detail)
			}
		}
	}
	if got := metric(t, card, "cost per claim"); got.Value != 20_000 {
		t.Errorf("cost per claim = %v, want 20000 (40000 over 2 claims)", got.Value)
	}
}

// ---------------------------------------------------------------------------
// Each regression must actually fire
// ---------------------------------------------------------------------------

// TestBudgetOvershootIsARegression. §14.3 says max overshoot "must be ~0", and
// the reserve/settle design exists to make it impossible. A non-zero value
// means the design is not holding, not that a limit was slightly loose — so it
// fails the build rather than lowering a score.
func TestBudgetOvershootIsARegression(t *testing.T) {
	f := newFixture(t, 100_000)
	// Reserve modestly, then settle for far more: the actual cost exceeding the
	// estimate is exactly how an overshoot happens in practice.
	f.spend(t, 10_000, 250_000)

	card := f.score(t)
	m := metric(t, card, "budget adherence")
	if !m.Regression {
		t.Errorf("a 150%% overshoot was not flagged: %s", m.Detail)
	}
	if m.Value <= 0 {
		t.Errorf("overshoot reported as %.1f%%", m.Value)
	}
	if !card.Failed() {
		t.Error("scorecard did not fail")
	}
}

// TestStrandedHoldIsARegression. Budget in an unsettled reservation is neither
// spent nor available; it is invisible in a spend total, and it is how a long
// session runs out of money it never used.
func TestStrandedHoldIsARegression(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	if _, err := f.led.ReserveFor(context.Background(), f.sess.ID, f.lead.ID, 500_000); err != nil {
		t.Fatal(err)
	}
	// Deliberately never settled or released.

	card := f.score(t)
	m := metric(t, card, "holds released")
	if !m.Regression {
		t.Errorf("a stranded hold was not flagged: %s", m.Detail)
	}
	if !card.Failed() {
		t.Error("scorecard did not fail")
	}
}

// TestMalformedClaimsAreARegression. Each of these is something §11.5 promises
// cannot reach the store, so one appearing means the pipeline let it through.
func TestMalformedClaimsAreARegression(t *testing.T) {
	cases := map[string]core.Claim{
		"empty text":          {Text: "  ", Source: "https://a.example", Quote: "a quote long enough to be real evidence", Confidence: 0.5},
		"quote too short":     {Text: "A claim.", Source: "https://a.example", Quote: "too short", Confidence: 0.5},
		"negative offset":     {Text: "A claim.", Source: "https://a.example", Quote: "a quote long enough to be real evidence", QuoteOffset: -5, Confidence: 0.5},
		"unresolvable source": {Text: "A claim.", Source: "not a url", Quote: "a quote long enough to be real evidence", Confidence: 0.5},
		"confidence over 1":   {Text: "A claim.", Source: "https://a.example", Quote: "a quote long enough to be real evidence", Confidence: 4},
	}

	for name, bad := range cases {
		f := newFixture(t, 3*core.MicrosPerUSD)
		f.spend(t, 100_000, 10_000)
		f.addClaims(t, goodClaim("A fine claim.", "https://ok.example"), bad)

		card := f.score(t)
		m := metric(t, card, "claim integrity")
		if !m.Regression {
			t.Errorf("%s: not flagged — %s", name, m.Detail)
		}
		if m.Value >= 100 {
			t.Errorf("%s: integrity reported as %.1f%%", name, m.Value)
		}
	}
}

// ---------------------------------------------------------------------------
// Diagnostics must NOT fail the build
// ---------------------------------------------------------------------------

// TestSourceConcentrationIsDiagnosticNotFatal. A question answered well by one
// good source is legitimate. Failing on it would train people to ignore the
// scorecard, which is worse than not reporting it.
func TestSourceConcentrationIsDiagnosticNotFatal(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)
	f.addClaims(t,
		goodClaim("One.", "https://only.example/a"),
		goodClaim("Two.", "https://only.example/a"),
		goodClaim("Three.", "https://only.example/a"),
	)

	card := f.score(t)
	m := metric(t, card, "source concentration")
	if m.Value != 100 {
		t.Errorf("concentration = %.1f%%, want 100%% (every claim from one source)", m.Value)
	}
	if m.Regression || card.Failed() {
		t.Error("total source concentration failed the build; it is diagnostic")
	}
}

// TestNoClaimsIsNotApplicableRatherThanZero. A run that found nothing has no
// cost-per-claim, and reporting 0 would look like the best possible score.
func TestNoClaimsIsNotApplicableRatherThanZero(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 50_000)

	card := f.score(t)
	for _, name := range []string{"cost per claim", "claim integrity", "source concentration"} {
		m := metric(t, card, name)
		if m.Status != eval.NotApplicable {
			t.Errorf("%s: status = %s, want n/a", name, m.Status)
		}
		if m.Regression {
			t.Errorf("%s: an empty run was reported as a regression", name)
		}
	}
	if card.Failed() {
		t.Error("a run that found nothing failed the mechanical checks; it is a quality issue, not a contract violation")
	}
}

// ---------------------------------------------------------------------------
// Honesty about what is not measured
// ---------------------------------------------------------------------------

// TestBlockedMetricsAreNamedNotOmitted. This is the design decision the package
// exists to make: four of nine §14.3 metrics will read zero until M4 and M8,
// and a scorecard that omitted them would read as complete — so a genuine
// future regression would look normal.
func TestBlockedMetricsAreNamedNotOmitted(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)
	f.addClaims(t, goodClaim("A claim.", "https://a.example"))

	card := f.score(t)
	want := map[string]bool{
		"claim precision": false, "grounding rate": false, "citation accuracy": false,
		"contradiction recall": false, "staleness detection": false, "exfil regression": false,
	}
	for _, m := range card.Metrics {
		if _, expected := want[m.Name]; !expected {
			continue
		}
		want[m.Name] = true
		if m.Status != eval.Blocked {
			t.Errorf("%s: status = %s, want blocked", m.Name, m.Status)
		}
		if m.Reason == "" {
			t.Errorf("%s: blocked with no reason given", m.Name)
		}
		if m.Regression {
			t.Errorf("%s: an unmeasurable metric was reported as a regression", m.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("§14.3 metric %q is missing from the scorecard entirely", name)
		}
	}
}

// TestBlockedMetricsCarryNoValue: a blocked metric with a number attached is
// exactly the confusion the status exists to prevent.
func TestBlockedMetricsCarryNoValue(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	card := f.score(t)
	for _, m := range card.Metrics {
		if m.Status == eval.Blocked && m.Value != 0 {
			t.Errorf("%s is blocked but carries value %v", m.Name, m.Value)
		}
	}
}
