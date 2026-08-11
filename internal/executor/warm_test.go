package executor_test

import (
	"context"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// §8.4's "improves with use", across sessions.
//
// The executor builds one estimator per run and it started cold every time, so
// what a hundred previous leads on this install cost was thrown away at the start
// of the next session. The comment saying this was blocked on M3 populating leads
// outlived M3.

// pastLeads writes finished leads with known costs, as a previous session would
// have left them.
func pastLeads(t *testing.T, r *rig, n int, usd int64) {
	t.Helper()
	ctx := context.Background()
	if err := r.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := 0; i < n; i++ {
			id := "past-" + string(rune('a'+i))
			if err := tx.InsertLead(ctx, &core.Lead{
				ID: id, SessionID: r.sess.ID, ActorType: core.ActorWeb,
				Query: "an earlier question", Status: core.LeadDone,
			}); err != nil {
				return err
			}
			leadID := id
			if err := tx.InsertToolCall(ctx, &core.ToolCall{
				ID: id + "-call", SessionID: r.sess.ID, LeadID: &leadID,
				Role: core.RoleExecutor, Type: core.CallLLM, Model: "m",
				Cost:      core.Cost{USDMicros: usd},
				CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestARunWarmsItsEstimatorFromPastLeads.
func TestARunWarmsItsEstimatorFromPastLeads(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{`{"questions":[],"rationale":"none"}`},
		func(int, core.Lead) (*actors.Result, error) { return &actors.Result{Summary: "s"}, nil })

	const observed = 3_000 // $0.003 a lead on this install
	pastLeads(t, r, 8, observed)

	seed := budget.SeedsUSD()[core.ActorWeb]
	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := r.exec.Estimator.For(core.ActorWeb, 0)
	if got == seed {
		t.Fatalf("the estimator is still at its cold seed (%d) after 8 past leads", seed)
	}
	if got != observed {
		t.Errorf("estimate = %d, want %d — the observed p75", got, observed)
	}
}

// TestAnInstallWithNoHistoryKeepsItsSeeds. A cold start is not an error.
func TestAnInstallWithNoHistoryKeepsItsSeeds(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{`{"questions":[],"rationale":"none"}`},
		func(int, core.Lead) (*actors.Result, error) { return &actors.Result{Summary: "s"}, nil })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The run itself produced one lead, which is fewer than the five observations
	// the estimator requires before it will use them.
	if got := r.exec.Estimator.For(core.ActorWeb, 0); got != budget.SeedsUSD()[core.ActorWeb] {
		t.Errorf("estimate = %d, want the seed on an install with no history", got)
	}
}
