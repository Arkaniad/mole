package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// The estimator's warm start reads this (§8.4). It is a join, and a join is where
// a cost gets attributed to the wrong thing quietly.

func seedLead(t *testing.T, db interface {
	WithTx(context.Context, func(context.Context, store.Tx) error) error
}, sessionID, leadID string, actor core.ActorType, depth int, status core.LeadStatus,
	calls ...core.Cost) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertLead(ctx, &core.Lead{
			ID: leadID, SessionID: sessionID, ActorType: actor, Query: "q",
			Depth: depth, Status: status,
		}); err != nil {
			return err
		}
		for i, c := range calls {
			id := leadID
			if err := tx.InsertToolCall(ctx, &core.ToolCall{
				ID: id + "-" + string(rune('a'+i)), SessionID: sessionID, LeadID: &leadID,
				Role: core.RoleExecutor, Type: core.CallLLM, Model: "m", Cost: c,
				CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func recentCosts(t *testing.T, db interface {
	Read(context.Context, func(context.Context, store.Queries) error) error
}, limit int) []core.LeadCost {
	t.Helper()
	var out []core.LeadCost
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		out, err = q.RecentLeadCosts(ctx, limit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestALeadsCallsAreTotalledUnderItsActorAndDepth.
func TestALeadsCallsAreTotalledUnderItsActorAndDepth(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_costs", 10_000_000)

	seedLead(t, db, "s_costs", "l_web", core.ActorWeb, 2, core.LeadDone,
		core.Cost{USDMicros: 3_000, InputTokens: 100},
		core.Cost{USDMicros: 2_000, OutputTokens: 50})
	seedLead(t, db, "s_costs", "l_local", core.ActorLocalCompute, 0, core.LeadDone,
		core.Cost{USDMicros: 9_000})

	got := recentCosts(t, db, 50)
	if len(got) != 2 {
		t.Fatalf("%d lead(s), want 2: %+v", len(got), got)
	}
	byActor := map[core.ActorType]core.LeadCost{}
	for _, lc := range got {
		byActor[lc.ActorType] = lc
	}
	web := byActor[core.ActorWeb]
	if web.Cost.USDMicros != 5_000 {
		t.Errorf("web usd = %d, want 5000 (both calls)", web.Cost.USDMicros)
	}
	if web.Depth != 2 {
		t.Errorf("web depth = %d, want 2", web.Depth)
	}
	if web.Cost.InputTokens != 100 || web.Cost.OutputTokens != 50 {
		t.Errorf("web tokens = %+v, want both calls' tokens", web.Cost)
	}
	if byActor[core.ActorLocalCompute].Cost.USDMicros != 9_000 {
		t.Errorf("local usd = %d, want 9000", byActor[core.ActorLocalCompute].Cost.USDMicros)
	}
}

// TestAnUnfinishedLeadIsNotASample.
//
// A lead that failed halfway spent real money and is a sample of a FAILURE.
// Mixing those in teaches the estimator to reserve for the average of working and
// broken.
func TestAnUnfinishedLeadIsNotASample(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_partial", 10_000_000)

	seedLead(t, db, "s_partial", "l_ok", core.ActorWeb, 0, core.LeadDone,
		core.Cost{USDMicros: 5_000})
	seedLead(t, db, "s_partial", "l_failed", core.ActorWeb, 0, core.LeadFailed,
		core.Cost{USDMicros: 1_000})
	seedLead(t, db, "s_partial", "l_queued", core.ActorWeb, 0, core.LeadQueued,
		core.Cost{USDMicros: 1_000})

	got := recentCosts(t, db, 50)
	if len(got) != 1 {
		t.Fatalf("%d sample(s), want only the finished lead: %+v", len(got), got)
	}
	if got[0].Cost.USDMicros != 5_000 {
		t.Errorf("usd = %d, want 5000", got[0].Cost.USDMicros)
	}
}

// TestSamplesComeBackOldestFirst.
//
// The estimator keeps a rolling window of the LAST N observations, so a
// newest-first feed would leave it holding the oldest.
func TestSamplesComeBackOldestFirst(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_order", 10_000_000)

	for i, usd := range []int64{1_000, 2_000, 3_000} {
		id := "l" + string(rune('0'+i))
		seedLead(t, db, "s_order", id, core.ActorWeb, 0, core.LeadDone,
			core.Cost{USDMicros: usd})
		// Distinct timestamps: seedLead stamps calls from time.Now, and three
		// leads inserted in the same microsecond would order by nothing.
		time.Sleep(2 * time.Millisecond)
	}

	got := recentCosts(t, db, 50)
	if len(got) != 3 {
		t.Fatalf("%d samples, want 3", len(got))
	}
	if got[0].Cost.USDMicros != 1_000 || got[2].Cost.USDMicros != 3_000 {
		t.Errorf("order = %d, %d, %d; want oldest first",
			got[0].Cost.USDMicros, got[1].Cost.USDMicros, got[2].Cost.USDMicros)
	}
}

// TestTheLimitKeepsTheNEWESTLeads. A bounded read that kept the oldest would seed
// today's reservations with an install's first week.
func TestTheLimitKeepsTheNewestLeads(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_limit", 10_000_000)

	for i := 0; i < 5; i++ {
		id := "l" + string(rune('0'+i))
		seedLead(t, db, "s_limit", id, core.ActorWeb, 0, core.LeadDone,
			core.Cost{USDMicros: int64(1_000 * (i + 1))})
		time.Sleep(2 * time.Millisecond)
	}

	got := recentCosts(t, db, 2)
	if len(got) != 2 {
		t.Fatalf("%d samples, want 2", len(got))
	}
	if got[0].Cost.USDMicros != 4_000 || got[1].Cost.USDMicros != 5_000 {
		t.Errorf("kept %d and %d, want the two newest (4000, 5000)",
			got[0].Cost.USDMicros, got[1].Cost.USDMicros)
	}
}

// TestCostsAreReadAcrossSessions. "What does a web lead cost on this install" is
// not a per-session question, and a fresh session is exactly when a cold estimator
// hurts.
func TestCostsAreReadAcrossSessions(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_a", 10_000_000)
	insertSession(t, db, "s_b", 10_000_000)

	seedLead(t, db, "s_a", "la", core.ActorWeb, 0, core.LeadDone, core.Cost{USDMicros: 1_000})
	seedLead(t, db, "s_b", "lb", core.ActorWeb, 0, core.LeadDone, core.Cost{USDMicros: 2_000})

	if got := recentCosts(t, db, 50); len(got) != 2 {
		t.Errorf("%d samples, want both sessions' leads", len(got))
	}
}
