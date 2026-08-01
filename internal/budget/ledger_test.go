package budget_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	// A real on-disk database, not :memory:, so the tests exercise the actual
	// WAL + single-writer configuration the daemon runs.
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newSession(t *testing.T, l *budget.Ledger, budgetAmount int64) *core.Session {
	t.Helper()
	s, err := l.CreateSession(context.Background(), budget.SessionSpec{
		Prompt:     "test",
		Mode:       core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD,
		Budget:     budgetAmount,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func noEscrow() budget.Config {
	c := budget.DefaultConfig()
	c.EscrowFraction = 0
	return c
}

func TestReserveSettleUpdatesLedgerAndCounters(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())
	s := newSession(t, l, 1_000_000)

	r, err := l.Reserve(ctx, s.ID, 100_000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// While held, the amount is unavailable but not yet spent.
	avail, err := l.Available(ctx, s.ID)
	if err != nil {
		t.Fatalf("available: %v", err)
	}
	if want := int64(900_000); avail != want {
		t.Fatalf("available after reserve = %d, want %d", avail, want)
	}

	res, err := l.Settle(ctx, r, []core.ToolCall{{
		Role:  core.RoleExecutor,
		Type:  core.CallLLM,
		Model: "claude-opus-5",
		Cost:  core.Cost{USDMicros: 80_000, InputTokens: 1000, OutputTokens: 200},
	}})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Charged != 80_000 {
		t.Errorf("charged = %d, want 80000", res.Charged)
	}
	if res.Overshoot != 0 {
		t.Errorf("overshoot = %d, want 0", res.Overshoot)
	}

	// The hold is gone and only the actual cost was charged.
	avail, err = l.Available(ctx, s.ID)
	if err != nil {
		t.Fatalf("available: %v", err)
	}
	if want := int64(920_000); avail != want {
		t.Fatalf("available after settle = %d, want %d", avail, want)
	}

	v, err := l.Verify(ctx, s.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Consistent() {
		t.Fatalf("ledger inconsistent: %+v", v)
	}
}

func TestReleaseReturnsHoldWithoutCharging(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())
	s := newSession(t, l, 500_000)

	r, err := l.Reserve(ctx, s.ID, 200_000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Release(ctx, r); err != nil {
		t.Fatalf("release: %v", err)
	}

	avail, err := l.Available(ctx, s.ID)
	if err != nil {
		t.Fatalf("available: %v", err)
	}
	if avail != 500_000 {
		t.Fatalf("available after release = %d, want 500000", avail)
	}

	v, _ := l.Verify(ctx, s.ID)
	if v.SpentFromLedger != 0 {
		t.Fatalf("release charged %d, want 0", v.SpentFromLedger)
	}
}

func TestReserveRefusesBeyondBudget(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())
	s := newSession(t, l, 100_000)

	if _, err := l.Reserve(ctx, s.ID, 100_001); !errors.Is(err, budget.ErrInsufficientBudget) {
		t.Fatalf("over-budget reserve error = %v, want ErrInsufficientBudget", err)
	}
	// Exactly the budget is allowed.
	if _, err := l.Reserve(ctx, s.ID, 100_000); err != nil {
		t.Fatalf("exact-budget reserve: %v", err)
	}
}

// TestConcurrentReserveNeverOvershoots is the headline M0 test.
//
// With post-hoc charging, N workers all pass the affordability check before any
// of them pays, and the session overshoots by up to N lead-costs. Reserving
// inside the transaction makes the ceiling hard: exactly budget/amount holds
// can exist at once, no matter how many goroutines race for them.
func TestConcurrentReserveNeverOvershoots(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	const (
		total   = int64(1_000_000)
		perUnit = int64(100_000)
		workers = 64
	)
	s := newSession(t, l, total)

	var (
		granted atomic.Int64
		refused atomic.Int64
		wg      sync.WaitGroup
	)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := l.Reserve(ctx, s.ID, perUnit)
			switch {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, budget.ErrInsufficientBudget):
				refused.Add(1)
			default:
				t.Errorf("unexpected reserve error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	wantGranted := total / perUnit
	if got := granted.Load(); got != wantGranted {
		t.Fatalf("granted %d reservations, want exactly %d", got, wantGranted)
	}
	if got := refused.Load(); got != workers-wantGranted {
		t.Fatalf("refused %d, want %d", got, workers-wantGranted)
	}

	// The invariant that matters: held never exceeds the budget.
	var sess *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		sess, err = q.GetSession(ctx, s.ID)
		return err
	}); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.Held != total {
		t.Fatalf("held = %d, want %d", sess.Held, total)
	}
	if sess.Available() != 0 {
		t.Fatalf("available = %d, want 0", sess.Available())
	}

	v, err := l.Verify(ctx, s.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Consistent() {
		t.Fatalf("ledger inconsistent after concurrent reserves: %+v", v)
	}
}

// TestConcurrentSettleKeepsSpentExact checks the other half: many workers
// settling at once must produce a Spent that equals the sum of the ledger rows,
// with no lost updates.
func TestConcurrentSettleKeepsSpentExact(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	const (
		workers = 32
		perCall = int64(10_000)
	)
	s := newSession(t, l, workers*perCall)

	reservations := make([]*core.Reservation, workers)
	for i := range reservations {
		r, err := l.Reserve(ctx, s.ID, perCall)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		reservations[i] = r
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range reservations {
		wg.Add(1)
		go func(r *core.Reservation) {
			defer wg.Done()
			<-start
			_, err := l.Settle(ctx, r, []core.ToolCall{{
				Role: core.RoleExecutor,
				Type: core.CallFetch,
				Cost: core.Cost{USDMicros: perCall},
			}})
			if err != nil {
				t.Errorf("settle: %v", err)
			}
		}(reservations[i])
	}
	close(start)
	wg.Wait()

	v, err := l.Verify(ctx, s.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Consistent() {
		t.Fatalf("ledger inconsistent: %+v", v)
	}
	if want := int64(workers) * perCall; v.SpentRecorded != want {
		t.Fatalf("spent = %d, want %d", v.SpentRecorded, want)
	}
	if v.HeldRecorded != 0 {
		t.Fatalf("held = %d after all settles, want 0", v.HeldRecorded)
	}
}

func TestDoubleSettleIsRejected(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())
	s := newSession(t, l, 500_000)

	r, err := l.Reserve(ctx, s.ID, 50_000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	call := []core.ToolCall{{Role: core.RoleExecutor, Type: core.CallFetch, Cost: core.Cost{USDMicros: 40_000}}}

	if _, err := l.Settle(ctx, r, call); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if _, err := l.Settle(ctx, r, call); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second settle error = %v, want ErrConflict", err)
	}

	v, _ := l.Verify(ctx, s.ID)
	if v.SpentRecorded != 40_000 {
		t.Fatalf("spent = %d after rejected double settle, want 40000", v.SpentRecorded)
	}
}

// TestEscrowProtectsOutputBudget is the regression test for rev 1's starvation
// bug: research spending everything, then output generation having nothing left.
func TestEscrowProtectsOutputBudget(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	cfg := budget.DefaultConfig()
	cfg.EscrowFraction = 0.2
	l := budget.New(db, cfg)

	s := newSession(t, l, 1_000_000)
	if s.Escrow != 200_000 {
		t.Fatalf("escrow = %d, want 200000", s.Escrow)
	}

	// Research cannot touch the escrow.
	if _, err := l.Reserve(ctx, s.ID, 800_001); !errors.Is(err, budget.ErrInsufficientBudget) {
		t.Fatalf("reserve into escrow error = %v, want ErrInsufficientBudget", err)
	}

	// Drain everything research is allowed to spend.
	r, err := l.Reserve(ctx, s.ID, 800_000)
	if err != nil {
		t.Fatalf("reserve research budget: %v", err)
	}
	if _, err := l.Settle(ctx, r, []core.ToolCall{{
		Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 800_000},
	}}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	if avail, _ := l.Available(ctx, s.ID); avail != 0 {
		t.Fatalf("available before escrow release = %d, want 0", avail)
	}

	// Output generation releases the escrow and can now pay for itself.
	released, err := l.ReleaseEscrow(ctx, s.ID)
	if err != nil {
		t.Fatalf("release escrow: %v", err)
	}
	if released != 200_000 {
		t.Fatalf("released = %d, want 200000", released)
	}
	if _, err := l.Reserve(ctx, s.ID, 200_000); err != nil {
		t.Fatalf("reserve output budget after escrow release: %v", err)
	}
}

// TestSweepExpiredReleasesStrandedHolds covers the reservation half of crash
// recovery: a worker that died mid-run must not hold budget forever.
func TestSweepExpiredReleasesStrandedHolds(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	cfg := noEscrow()
	cfg.ReservationTTL = time.Minute
	l := budget.New(db, cfg)

	now := time.Now().UTC()
	l.SetClock(func() time.Time { return now })

	s := newSession(t, l, 1_000_000)
	if _, err := l.Reserve(ctx, s.ID, 300_000); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if avail, _ := l.Available(ctx, s.ID); avail != 700_000 {
		t.Fatalf("available while held = %d, want 700000", avail)
	}

	// Simulate the daemon restarting well after the lease expired.
	now = now.Add(10 * time.Minute)

	n, err := l.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d reservations, want 1", n)
	}
	if avail, _ := l.Available(ctx, s.ID); avail != 1_000_000 {
		t.Fatalf("available after sweep = %d, want 1000000", avail)
	}
}

func TestSettleFlagsOvershoot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	cfg := noEscrow()
	cfg.MaxOvershootFactor = 1.5
	l := budget.New(db, cfg)
	s := newSession(t, l, 1_000_000)

	r, err := l.Reserve(ctx, s.ID, 10_000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// The estimate was badly wrong. The ledger records the truth and flags it;
	// it does not pretend the money was not spent.
	res, err := l.Settle(ctx, r, []core.ToolCall{{
		Role: core.RoleExecutor, Type: core.CallLLM, Cost: core.Cost{USDMicros: 40_000},
	}})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Overshoot != 30_000 {
		t.Errorf("overshoot = %d, want 30000", res.Overshoot)
	}
	if !res.Flagged {
		t.Error("overshoot beyond MaxOvershootFactor was not flagged")
	}

	v, _ := l.Verify(ctx, s.ID)
	if !v.Consistent() {
		t.Fatalf("ledger inconsistent after overshoot: %+v", v)
	}
}

func TestTokenModeCountsCacheTokens(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	s, err := l.CreateSession(ctx, budget.SessionSpec{
		Prompt:     "token mode",
		Mode:       core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens,
		Budget:     50_000,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	r, err := l.Reserve(ctx, s.ID, 20_000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Cache reads are cheaper, not free. A token budget that ignored them
	// would not bound anything.
	cost := core.Cost{
		USDMicros:        12_345,
		InputTokens:      5_000,
		OutputTokens:     1_000,
		CacheReadTokens:  8_000,
		CacheWriteTokens: 2_000,
	}
	res, err := l.Settle(ctx, r, []core.ToolCall{{Role: core.RoleExecutor, Type: core.CallLLM, Cost: cost}})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if want := int64(16_000); res.Charged != want {
		t.Fatalf("charged %d tokens, want %d", res.Charged, want)
	}

	v, _ := l.Verify(ctx, s.ID)
	if !v.Consistent() {
		t.Fatalf("ledger inconsistent: %+v", v)
	}
	// The USD figure is still recorded, so switching the session's unit is a
	// display choice rather than a data migration.
	if v.Cost.USDMicros != 12_345 {
		t.Fatalf("usd not recorded in token mode: %d", v.Cost.USDMicros)
	}
}

func TestCeilingsAreUnitIndependent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	l := budget.New(db, noEscrow())

	// Token mode with a huge budget but a tight call ceiling: this is the hole
	// that makes an unbounded fetch loop free otherwise.
	s, err := l.CreateSession(ctx, budget.SessionSpec{
		Prompt:       "ceiling",
		Mode:         core.ModeReport,
		ActorTypes:   []core.ActorType{core.ActorWeb},
		BudgetUnit:   core.BudgetTokens,
		Budget:       1_000_000,
		MaxToolCalls: 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 2; i++ {
		r, err := l.Reserve(ctx, s.ID, 10)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		// A fetch costs no tokens at all — exactly the case the budget cannot see.
		if _, err := l.Settle(ctx, r, []core.ToolCall{{Role: core.RoleExecutor, Type: core.CallFetch}}); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}

	if _, err := l.Reserve(ctx, s.ID, 10); !errors.Is(err, budget.ErrInsufficientBudget) {
		t.Fatalf("reserve past MaxToolCalls error = %v, want ErrInsufficientBudget", err)
	}
}
