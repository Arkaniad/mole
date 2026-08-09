package budget_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// M5 slice 1.
//
// TestConcurrentReserveNeverOvershoots proves the MONEY ceiling holds under
// concurrency, because Available() subtracts Held and the hold is written in the
// same transaction as the check. §8.5's unit-independent ceilings are a separate
// counter and did not have that property: LeadCount was incremented by the
// executor after a lead finished running, so K workers could all reserve while
// the counter still read N, all pass HitCeiling, and dispatch up to K-1 leads
// past MaxLeads.

func newSessionWithLeadCap(t *testing.T, l *budget.Ledger, budgetAmount, maxLeads int64) *core.Session {
	t.Helper()
	s, err := l.CreateSession(context.Background(), budget.SessionSpec{
		Prompt:     "test",
		Mode:       core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD,
		Budget:     budgetAmount,
		MaxLeads:   maxLeads,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

// TestConcurrentLeadReservesRespectMaxLeads is the §8.5 twin of the M0 money
// test. Budget is deliberately far larger than the workers can spend, so the
// only thing that can refuse a reservation is the lead ceiling.
func TestConcurrentLeadReservesRespectMaxLeads(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	const (
		total    = int64(100_000_000) // far more than 64 x perLead
		perLead  = int64(1_000)
		maxLeads = int64(10)
		workers  = 64
	)
	s := newSessionWithLeadCap(t, l, total, maxLeads)

	var (
		granted atomic.Int64
		refused atomic.Int64
		wg      sync.WaitGroup
	)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // released together, so they genuinely race
			_, err := l.ReserveFor(ctx, s.ID, fmt.Sprintf("l_%02d", i), perLead)
			switch {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, budget.ErrInsufficientBudget):
				refused.Add(1)
			default:
				t.Errorf("unexpected reserve error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != maxLeads {
		t.Fatalf("granted %d lead reservations, want exactly %d — MaxLeads does not bind under concurrency", got, maxLeads)
	}
	if got := refused.Load(); got != workers-maxLeads {
		t.Fatalf("refused %d, want %d", got, workers-maxLeads)
	}

	// And the persisted counter agrees with what was granted. A ceiling that
	// binds while the counter drifts is a ceiling that stops binding on the
	// next call.
	var reloaded *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		reloaded, err = q.GetSession(ctx, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if reloaded.LeadCount != maxLeads {
		t.Fatalf("session records LeadCount=%d, want %d", reloaded.LeadCount, maxLeads)
	}
	if hit, which := reloaded.HitCeiling(reloaded.CreatedAt); !hit || which != "max_leads" {
		t.Fatalf("session does not report max_leads after exhausting it (hit=%v which=%q)", hit, which)
	}
}

// TestTheLeadAtTheCeilingStillRuns pins the ordering inside the transaction.
//
// The increment has to land AFTER the check, not before. Counting first would
// fail the last permitted lead on its own reservation — "we did the work we were
// allowed" reported as "the last lead errored" — which is exactly why this was
// originally counted after the lead ran instead.
func TestTheLeadAtTheCeilingStillRuns(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	const maxLeads = int64(3)
	s := newSessionWithLeadCap(t, l, 100_000_000, maxLeads)

	for i := int64(0); i < maxLeads; i++ {
		if _, err := l.ReserveFor(ctx, s.ID, fmt.Sprintf("l_%d", i), 1_000); err != nil {
			t.Fatalf("lead %d of %d was refused: %v", i+1, maxLeads, err)
		}
	}
	_, err := l.ReserveFor(ctx, s.ID, "l_over", 1_000)
	if !errors.Is(err, budget.ErrInsufficientBudget) {
		t.Fatalf("lead %d was allowed past MaxLeads=%d: %v", maxLeads+1, maxLeads, err)
	}
}

// TestNonLeadReservesDoNotCountAsLeads. Planner, verifier and output
// reservations share reserveWith; only a hold naming a lead is a lead. If
// planning counted, a session's replans would eat the research allowance §8.5
// is bounding.
func TestNonLeadReservesDoNotCountAsLeads(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	s := newSessionWithLeadCap(t, l, 100_000_000, 2)

	for i := 0; i < 5; i++ {
		if _, err := l.Reserve(ctx, s.ID, 1_000); err != nil {
			t.Fatalf("planner reserve %d refused: %v", i, err)
		}
	}

	var reloaded *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		reloaded, err = q.GetSession(ctx, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if reloaded.LeadCount != 0 {
		t.Fatalf("LeadCount=%d after 5 non-lead reservations, want 0", reloaded.LeadCount)
	}
}
