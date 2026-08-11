package actors_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/compute/coderunner"
	"github.com/lajosdeme/mole/internal/compute/stats"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// §12.1's audit trail, made durable (M8's last known gap).
//
// "Every crossing is logged, so a user can audit exactly what left their
// machine." The gate emitted a log line, which cannot be queried per session and
// rotates away — so this records a row per crossing, including the refusals.

func crossingStore(t *testing.T) (*sqlite.DB, string) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess := &core.Session{
		ID: "sess-1", Prompt: "how do the regions compare", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorLocalCompute},
		BudgetUnit: core.BudgetUSD, Budget: 1_000_000, Status: core.StatusRunning,
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertSession(ctx, sess)
	}); err != nil {
		t.Fatal(err)
	}
	return db, sess.ID
}

func listCrossings(t *testing.T, db *sqlite.DB, sessionID string) []core.Crossing {
	t.Helper()
	var out []core.Crossing
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		out, err = q.ListCrossings(ctx, sessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestACrossingIsRecordedWithNoValueFromTheData.
//
// The whole point of the table: an audit trail that is another copy of the thing
// the user was worried about is worse than none.
func TestACrossingIsRecordedWithNoValueFromTheData(t *testing.T) {
	db, sessionID := crossingStore(t)
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	a := &actors.LocalComputeActor{Connectors: reg, LLM: fl, Store: db}
	if _, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "how do the regions compare",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	list := listCrossings(t, db, sessionID)
	if len(list) != 1 {
		t.Fatalf("%d crossing(s) recorded, want 1", len(list))
	}
	c := list[0]
	if c.Outcome != core.CrossingCrossed {
		t.Errorf("outcome = %q, want crossed", c.Outcome)
	}
	if c.Connector != "sales" || c.LeadID != "lead-1" {
		t.Errorf("crossing = %+v, want it attributed to the connector and the lead", c)
	}
	if c.QueryHash == "" || !strings.Contains(strings.ToUpper(c.Query), "SELECT") {
		t.Errorf("the statement is not recorded: %+v", c)
	}
	if c.RowsDescribed == 0 {
		t.Error("rows_described is zero for a crossing that described rows")
	}
	if c.CreatedAt.IsZero() {
		t.Error("no timestamp")
	}

	// The data's own values must not appear anywhere in the record. "north" and
	// "south" are the region names in the fixture, and a bucket key that survived
	// the floor is allowed to cross in the ENVELOPE — but the audit row is not the
	// envelope, and a user reading their trail should not be re-reading their data.
	joined := c.Query + " " + c.Detail
	for _, value := range []string{"Shipment held at the depot", "Straightforward renewal"} {
		if strings.Contains(joined, value) {
			t.Errorf("a value from the data is in the audit record: %q", value)
		}
	}
}

// TestARefusedHypothesisIsRecordedToo.
//
// The refusals are the more interesting half: they are the gate doing what the
// user is trusting it to do. A trail of successes only would let a reader conclude
// the questions mole answered are all it tried.
func TestARefusedHypothesisIsRecordedToo(t *testing.T) {
	db, sessionID := crossingStore(t)
	reg := localRegistry(t)
	// group_comparison over a free-text column: the profile flags `note` as free
	// text, so the gate refuses rather than bucketing sentences.
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"group_comparison",
	   "columns":{"group":"note","measure":"amount"},"question":"Do notes differ?"},
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	a := &actors.LocalComputeActor{Connectors: reg, LLM: fl, Store: db}
	if _, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "how do the regions compare",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	list := listCrossings(t, db, sessionID)
	var refused, crossed int
	for _, c := range list {
		switch c.Outcome {
		case core.CrossingRefused:
			refused++
			if c.Detail == "" {
				t.Error("a refusal was recorded with no reason")
			}
		case core.CrossingCrossed:
			crossed++
		case core.CrossingWithheld:
			t.Errorf("an envelope was withheld for carrying row-level data: %+v", c)
		}
	}
	if refused == 0 {
		t.Errorf("no refusal recorded; the trail shows only what succeeded: %+v", list)
	}
	if crossed == 0 {
		t.Error("the hypothesis that should have crossed did not")
	}
}

// TestTheTrailSurvivesACancelledRun.
//
// The data has already left the machine by the time the trail is written, so a
// cancelled write loses the record of a crossing that HAPPENED. Claims are
// persisted on an uncancellable context for a weaker version of this reason.
func TestTheTrailSurvivesACancelledRun(t *testing.T) {
	db, sessionID := crossingStore(t)
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	ctx, cancel := context.WithCancel(context.Background())
	a := &actors.LocalComputeActor{Connectors: reg, LLM: &fakeLLM{mineFunc: func(prompt string) string {
		if strings.Contains(prompt, "Research question:") {
			return fl.mineFunc(prompt)
		}
		// The gate has run by now: the evidence crossed and the miner is next.
		// Cancelling here is the sibling-lead-failed case.
		cancel()
		return fl.mineFunc(prompt)
	}}, Store: db}

	if _, err := a.Run(ctx, core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "how do the regions compare",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if list := listCrossings(t, db, sessionID); len(list) == 0 {
		t.Fatal("the audit trail was lost with the cancelled run; §12.1's record of " +
			"what left this machine is incomplete")
	}
}

// TestNoStoreIsNotAFailedRun. A run without a store — a test, a dry probe —
// should not lose its findings over an audit table.
func TestNoStoreIsNotAFailedRun(t *testing.T) {
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	res := runLocal(t, fl, reg)
	if len(res.Claims) == 0 {
		t.Error("a run with no store produced no claims")
	}
}

// TestACodeRunCrossesAndIsRecorded.
//
// §12.1 puts the control in the container rather than the gate for this route, and
// "the container held it" is not the same as "nothing left the machine": the
// declared metrics did. A trail that recorded only the SQL route would understate
// what crossed.
func TestACodeRunCrossesAndIsRecorded(t *testing.T) {
	db, sessionID := crossingStore(t)
	fl := scriptedModel(codePlan, quoteAMetricLine)

	runner := &fakeRunner{out: coderunner.Output{
		Metrics: map[string]float64{"amplitude": 12.5},
		Findings: []coderunner.Finding{{
			Name: "seasonality", N: 8760, Statistic: 4.2, P: 0.0003,
			EffectSize: 0.6, Verdict: stats.Significant,
		}},
		Undeclared: 2,
	}}
	a := &actors.LocalComputeActor{
		Connectors: samplesRegistry(t, 40), LLM: fl, Store: db, Code: runner,
	}
	if _, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "what is the spread",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	list := listCrossings(t, db, sessionID)
	if len(list) != 1 {
		t.Fatalf("%d crossing(s), want 1: %+v", len(list), list)
	}
	c := list[0]
	if c.Outcome != core.CrossingCrossed {
		t.Errorf("outcome = %q, want crossed", c.Outcome)
	}
	if !strings.HasPrefix(c.QueryHash, "code:") {
		t.Errorf("hash = %q, want it to name the script rather than a statement", c.QueryHash)
	}
	if c.Columns != 1 {
		t.Errorf("columns = %d, want the one declared metric", c.Columns)
	}
	if c.Tests != 1 {
		t.Errorf("tests = %d, want the one declared test", c.Tests)
	}
	if c.ColumnsWithheld != 2 {
		t.Errorf("withheld = %d, want the two undeclared outputs", c.ColumnsWithheld)
	}
}
