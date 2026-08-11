package eval_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// §14.3's exfil number, per session.
//
// It was blocked twice over in blockedMetrics — once with the reason "needs the
// aggregation gate (M8)", long after M8 landed, and once with "per-session
// reporting needs the LocalComputeActor to have run", which stayed true because
// the gate's audit trail was a log line and a scorecard cannot read a log.

func crossingSession(t *testing.T, list []core.Crossing) (*sqlite.DB, string) {
	t.Helper()
	db, err := sqlite.Open(":memory:", sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess := &core.Session{
		ID: core.NewSessionID(), Prompt: "local question", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorLocalCompute},
		BudgetUnit: core.BudgetTokens, Budget: 1000, Status: core.StatusDone,
		CreatedAt: time.Now().UTC(),
	}
	for i := range list {
		list[i].SessionID = sess.ID
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertSession(ctx, sess); err != nil {
			return err
		}
		return tx.InsertCrossings(ctx, list)
	}); err != nil {
		t.Fatal(err)
	}
	return db, sess.ID
}

func crossingMetric(t *testing.T, card eval.Scorecard, name string) eval.Metric {
	t.Helper()
	for _, m := range card.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no metric named %q", name)
	return eval.Metric{}
}

func crossing(outcome core.CrossingOutcome, rows int64) core.Crossing {
	return core.Crossing{
		Connector: "sales", Query: `SELECT "region", COUNT(*) FROM "t" GROUP BY 1`,
		QueryHash: "abc123", Outcome: outcome, RowsDescribed: rows, Buckets: 2,
	}
}

// TestTheExfilNumberIsMeasuredForALocalSession, and reads zero.
func TestTheExfilNumberIsMeasuredForALocalSession(t *testing.T) {
	db, id := crossingSession(t, []core.Crossing{
		crossing(core.CrossingCrossed, 40),
		crossing(core.CrossingCrossed, 12),
		crossing(core.CrossingRefused, 0),
	})

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := crossingMetric(t, card, "exfil regression")
	if m.Status != eval.Measured {
		t.Fatalf("status = %q, want measured — the crossings are there to measure", m.Status)
	}
	if m.Value != 0 {
		t.Errorf("exfil = %v, want 0", m.Value)
	}
	if m.Regression {
		t.Error("a clean session was marked as a regression")
	}
	if !strings.Contains(m.Detail, "0 of 3") {
		t.Errorf("detail = %q, want the arithmetic", m.Detail)
	}
	if r := crossingMetric(t, card, "gate refusal rate"); r.Value == 0 {
		t.Errorf("refusal rate = %v, want the one refusal counted", r.Value)
	}
	if card.Failed() {
		t.Error("a clean local session fails the scorecard")
	}
}

// TestAWithheldEnvelopeIsAHardRegression.
//
// The exfil check is a backstop for structural rules that refuse first, so it
// firing means one of those rules broke. Zero is the only acceptable value.
func TestAWithheldEnvelopeIsAHardRegression(t *testing.T) {
	db, id := crossingSession(t, []core.Crossing{
		crossing(core.CrossingCrossed, 40),
		crossing(core.CrossingWithheld, 0),
	})

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := crossingMetric(t, card, "exfil regression")
	if m.Value != 50 {
		t.Errorf("exfil = %v, want 50", m.Value)
	}
	if !m.Regression {
		t.Error("an envelope carrying row-level data is not a hard regression")
	}
	if !card.Failed() {
		t.Error("the scorecard does not fail")
	}
}

// TestASessionWithNoLocalDataSaysBlockedNotZero.
//
// "0% of envelopes leaked" over no envelopes reads as a clean bill of health for a
// check that never ran.
func TestASessionWithNoLocalDataSaysBlockedNotZero(t *testing.T) {
	db, id := crossingSession(t, nil)

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := crossingMetric(t, card, "exfil regression")
	if m.Status != eval.Blocked {
		t.Errorf("status = %q, want blocked for a session that crossed nothing", m.Status)
	}
	if !strings.Contains(m.Reason, "enforced at the gate") {
		t.Errorf("the reason does not say where it IS enforced: %q", m.Reason)
	}
}

// TestTheExfilMetricAppearsOnce. It was listed twice in blockedMetrics, with two
// different reasons, one of them years out of date.
func TestTheExfilMetricAppearsOnce(t *testing.T) {
	db, id := crossingSession(t, []core.Crossing{crossing(core.CrossingCrossed, 5)})

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, m := range card.Metrics {
		if m.Name == "exfil regression" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the scorecard reports %d metrics called \"exfil regression\", want 1", n)
	}
}

// TestALocalClaimsCitationIsWellFormed.
//
// Found by scoring a live local session: five well-formed claims scored 0% on
// claim integrity because the check parsed every source as a URL, and a local
// claim cites "connector:<name>#<hash>" — the data never left the machine, so
// there is no URL to cite. A false regression in the one metric that exists to
// catch real ones.
func TestALocalClaimsCitationIsWellFormed(t *testing.T) {
	db, id := crossingSession(t, []core.Crossing{crossing(core.CrossingCrossed, 40)})

	quote := "north — 40 records, mean 118.42 (the passage a claim quotes)"
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, []core.Claim{{
			ID: core.NewClaimID(), SessionID: id, LeadID: "l1",
			Text:   "Support spend in the north region averages 118.42.",
			Quote:  quote,
			Source: "connector:support#46c9d1a6362293f7",
		}})
	}); err != nil {
		t.Fatal(err)
	}

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := crossingMetric(t, card, "claim integrity")
	if m.Value != 100 {
		t.Errorf("claim integrity = %v — a local citation is read as malformed: %s",
			m.Value, m.Detail)
	}
	if m.Regression {
		t.Error("a well-formed local claim is scored as a regression")
	}
}

// TestAMalformedLocalCitationIsStillCaught. The relaxation must not become "any
// string starting with connector:".
func TestAMalformedLocalCitationIsStillCaught(t *testing.T) {
	db, id := crossingSession(t, []core.Crossing{crossing(core.CrossingCrossed, 40)})

	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, []core.Claim{{
			ID: core.NewClaimID(), SessionID: id, LeadID: "l1",
			Text:  "Support spend in the north region averages 118.42.",
			Quote: "north — 40 records, mean 118.42 (the passage a claim quotes)",
			// No query hash: nothing to trace the claim back to.
			Source: "connector:support",
		}})
	}); err != nil {
		t.Fatal(err)
	}

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if m := crossingMetric(t, card, "claim integrity"); m.Value != 0 {
		t.Errorf("claim integrity = %v, want 0 — a citation with no query hash "+
			"traces to nothing", m.Value)
	}
}
