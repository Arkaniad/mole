// Package budget enforces spend.
//
// The central idea is that budget is *reserved before dispatch and settled
// after*, not charged after the fact. Post-hoc charging lets a pool of N
// workers overshoot the ceiling by up to N lead-costs, because every worker
// passes the "can I afford one more?" check before any of them has paid.
// Holding first bounds the overshoot to estimate error on a single lead.
//
// A second rule: the append-only tool_calls ledger is the source of truth for
// spend. Session.Spent is a materialized sum written in the same transaction as
// each cost row, so a crash can never desynchronize the two — and Verify can
// always prove it.
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// ErrInsufficientBudget is returned by Reserve when the requested amount
// exceeds what is available. It is an expected control-flow signal, not a
// failure: the executor treats it as "stop taking new work".
var ErrInsufficientBudget = errors.New("budget: insufficient")

// Config tunes the ledger.
type Config struct {
	// EscrowFraction is the share of the budget held back at session start for
	// output generation and the final verification pass. Report generation is
	// one LLM pass over the whole claim graph — plausibly the most expensive
	// single call in the session — so without escrow a session can spend 100%
	// on research and have nothing left to write the answer.
	//
	// 0.15 is a starting guess, to be calibrated from real ledgers.
	EscrowFraction float64

	// MinEscrow is a floor in the session's budget unit, applied when a
	// percentage of a small budget would not cover one report.
	MinEscrow int64

	// ReservationTTL bounds how long a hold survives without being settled.
	// The recovery sweep releases anything older; without it, a worker that
	// died mid-run holds budget forever.
	ReservationTTL time.Duration

	// MaxOvershootFactor caps how far actual cost may exceed the reservation
	// before Settle flags it. Exceeding it does not reject the settle — the
	// money was spent and the ledger records what happened — but it is the
	// signal that the estimator needs recalibrating.
	MaxOvershootFactor float64
}

func DefaultConfig() Config {
	return Config{
		EscrowFraction:     0.15,
		MinEscrow:          0,
		ReservationTTL:     15 * time.Minute,
		MaxOvershootFactor: 2.0,
	}
}

// Ledger owns all budget mutations.
type Ledger struct {
	st  store.Store
	cfg Config
	now func() time.Time
}

func New(st store.Store, cfg Config) *Ledger {
	if cfg.ReservationTTL <= 0 {
		cfg.ReservationTTL = DefaultConfig().ReservationTTL
	}
	if cfg.MaxOvershootFactor <= 0 {
		cfg.MaxOvershootFactor = DefaultConfig().MaxOvershootFactor
	}
	return &Ledger{st: st, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides time, for tests.
func (l *Ledger) SetClock(fn func() time.Time) { l.now = fn }

func (l *Ledger) Config() Config { return l.cfg }

// ---------------------------------------------------------------------------
// Session creation
// ---------------------------------------------------------------------------

// SessionSpec is the input to CreateSession.
type SessionSpec struct {
	Prompt       string
	Mode         core.Mode
	ActorTypes   []core.ActorType
	BudgetUnit   core.BudgetUnit
	Budget       int64
	MaxToolCalls int64
	MaxLeads     int64
	MaxWallClock time.Duration
}

// CreateSession persists a session with its escrow already held back, so no
// code path can spend the output reserve by forgetting to call something.
func (l *Ledger) CreateSession(ctx context.Context, spec SessionSpec) (*core.Session, error) {
	s := &core.Session{
		ID:           core.NewSessionID(),
		Prompt:       spec.Prompt,
		Mode:         spec.Mode,
		ActorTypes:   spec.ActorTypes,
		BudgetUnit:   spec.BudgetUnit,
		Budget:       spec.Budget,
		Escrow:       l.escrowFor(spec.Budget),
		MaxToolCalls: spec.MaxToolCalls,
		MaxLeads:     spec.MaxLeads,
		MaxWallClock: spec.MaxWallClock,
		Status:       core.StatusRunning,
		CreatedAt:    l.now(),
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertSession(ctx, s)
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (l *Ledger) escrowFor(budget int64) int64 {
	e := int64(float64(budget) * l.cfg.EscrowFraction)
	if e < l.cfg.MinEscrow {
		e = l.cfg.MinEscrow
	}
	if e > budget {
		e = budget
	}
	if e < 0 {
		e = 0
	}
	return e
}

// ---------------------------------------------------------------------------
// Reserve / Settle / Release
// ---------------------------------------------------------------------------

// Reserve places a hold. It is the hard ceiling: the availability check and
// the hold happen in one transaction, so two concurrent workers cannot both
// pass a check that only one of them can afford.
//
// Rev-1's `Remaining() > floorCost(leads.Peek())` followed by `leads.Pop()` was
// also a TOCTOU bug — the peeked lead is not necessarily the popped one. Here
// the amount reserved is always for the lead actually dequeued.
func (l *Ledger) Reserve(ctx context.Context, sessionID string, amount int64) (*core.Reservation, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("budget: reserve amount must be positive, got %d", amount)
	}

	now := l.now()
	r := &core.Reservation{
		ID:        core.NewReservationID(),
		SessionID: sessionID,
		Amount:    amount,
		Status:    core.ReservationHeld,
		CreatedAt: now,
		ExpiresAt: now.Add(l.cfg.ReservationTTL),
	}

	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		s, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if s.Status.Terminal() {
			return fmt.Errorf("budget: session %s is %s", sessionID, s.Status)
		}
		if hit, which := s.HitCeiling(now); hit {
			return fmt.Errorf("%w: session %s hit %s", ErrInsufficientBudget, sessionID, which)
		}
		if avail := s.Available(); amount > avail {
			return fmt.Errorf("%w: need %s, have %s",
				ErrInsufficientBudget,
				core.FormatAmount(amount, s.BudgetUnit),
				core.FormatAmount(avail, s.BudgetUnit))
		}
		if err := tx.InsertReservation(ctx, r); err != nil {
			return err
		}
		return tx.ApplyBudgetDelta(ctx, sessionID, store.BudgetDelta{Held: amount})
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ReserveFor is Reserve with the lead recorded on the hold, so a stranded
// reservation can be traced back to the work that took it.
func (l *Ledger) ReserveFor(ctx context.Context, sessionID, leadID string, amount int64) (*core.Reservation, error) {
	r, err := l.Reserve(ctx, sessionID, amount)
	if err != nil {
		return nil, err
	}
	r.LeadID = &leadID
	return r, nil
}

// SettleResult reports what a settle actually cost against what was held.
type SettleResult struct {
	Reserved  int64
	Charged   int64
	Overshoot int64 // Charged - Reserved, when positive
	Flagged   bool  // Charged exceeded Reserved * MaxOvershootFactor
	Cost      core.Cost
}

// Settle writes the actual cost rows and releases the hold, in one transaction.
//
// Partial cost is settled on failure too — the money was spent whether or not
// the call succeeded, and a ledger that only records successes cannot enforce a
// ceiling.
func (l *Ledger) Settle(ctx context.Context, r *core.Reservation, calls []core.ToolCall) (SettleResult, error) {
	var res SettleResult
	now := l.now()

	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res = SettleResult{}

		s, err := tx.GetSession(ctx, r.SessionID)
		if err != nil {
			return err
		}

		stored, err := tx.GetReservation(ctx, r.ID)
		if err != nil {
			return err
		}
		if stored.Status != core.ReservationHeld {
			return fmt.Errorf("%w: reservation %s already %s", store.ErrConflict, r.ID, stored.Status)
		}

		var total core.Cost
		for i := range calls {
			tc := &calls[i]
			tc.SessionID = r.SessionID
			if tc.LeadID == nil {
				tc.LeadID = r.LeadID
			}
			if err := tx.InsertToolCall(ctx, tc); err != nil {
				return err
			}
			total = total.Add(tc.Cost)
		}

		charged := total.BudgetAmount(s.BudgetUnit)

		if err := tx.ResolveReservation(ctx, r.ID, core.ReservationSettled, now); err != nil {
			return err
		}
		if err := tx.ApplyBudgetDelta(ctx, r.SessionID, store.BudgetDelta{
			Spent:         charged,
			Held:          -stored.Amount,
			ToolCallCount: int64(len(calls)),
		}); err != nil {
			return err
		}

		res = SettleResult{
			Reserved: stored.Amount,
			Charged:  charged,
			Cost:     total,
		}
		if charged > stored.Amount {
			res.Overshoot = charged - stored.Amount
		}
		if float64(charged) > float64(stored.Amount)*l.cfg.MaxOvershootFactor {
			res.Flagged = true
		}
		return nil
	})
	if err != nil {
		return SettleResult{}, err
	}

	r.Status = core.ReservationSettled
	r.ResolvedAt = &now
	return res, nil
}

// Release drops a hold without charging — used when a lead is cancelled before
// dispatch, or when a cache hit means no work was done.
func (l *Ledger) Release(ctx context.Context, r *core.Reservation) error {
	now := l.now()
	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		stored, err := tx.GetReservation(ctx, r.ID)
		if err != nil {
			return err
		}
		if stored.Status != core.ReservationHeld {
			return fmt.Errorf("%w: reservation %s already %s", store.ErrConflict, r.ID, stored.Status)
		}
		if err := tx.ResolveReservation(ctx, r.ID, core.ReservationReleased, now); err != nil {
			return err
		}
		return tx.ApplyBudgetDelta(ctx, r.SessionID, store.BudgetDelta{Held: -stored.Amount})
	})
	if err != nil {
		return err
	}
	r.Status = core.ReservationReleased
	r.ResolvedAt = &now
	return nil
}

// ---------------------------------------------------------------------------
// Escrow
// ---------------------------------------------------------------------------

// ReleaseEscrow makes the output reserve spendable. Call it once research is
// finished and the session is about to generate its report — after this, the
// escrowed amount is ordinary available budget.
func (l *Ledger) ReleaseEscrow(ctx context.Context, sessionID string) (int64, error) {
	var released int64
	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		s, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		released = s.Escrow
		if released == 0 {
			return nil
		}
		return tx.ApplyBudgetDelta(ctx, sessionID, store.BudgetDelta{Escrow: -released})
	})
	return released, err
}

// ---------------------------------------------------------------------------
// Inspection and recovery
// ---------------------------------------------------------------------------

// Available reports the spendable balance.
func (l *Ledger) Available(ctx context.Context, sessionID string) (int64, error) {
	var out int64
	err := l.st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		s, err := q.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		out = s.Available()
		return nil
	})
	return out, err
}

// VerifyResult compares the materialized counters against the ledger itself.
type VerifyResult struct {
	SpentRecorded   int64
	SpentFromLedger int64
	HeldRecorded    int64
	HeldFromRows    int64
	CallsRecorded   int64
	CallsFromRows   int64
	Cost            core.Cost
}

func (v VerifyResult) Consistent() bool {
	return v.SpentRecorded == v.SpentFromLedger &&
		v.HeldRecorded == v.HeldFromRows &&
		v.CallsRecorded == v.CallsFromRows
}

// Verify recomputes the session counters from the append-only rows.
//
// This is the payoff of treating the ledger as the source of truth: the
// materialized sums are always provable, and a divergence is a bug that can be
// detected in a test rather than a slow drift discovered in a bill.
func (l *Ledger) Verify(ctx context.Context, sessionID string) (VerifyResult, error) {
	var v VerifyResult
	err := l.st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		s, err := q.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		cost, err := q.SumCosts(ctx, sessionID)
		if err != nil {
			return err
		}
		held, err := q.SumHeldReservations(ctx, sessionID)
		if err != nil {
			return err
		}
		calls, err := q.CountToolCalls(ctx, sessionID)
		if err != nil {
			return err
		}
		v = VerifyResult{
			SpentRecorded:   s.Spent,
			SpentFromLedger: cost.BudgetAmount(s.BudgetUnit),
			HeldRecorded:    s.Held,
			HeldFromRows:    held,
			CallsRecorded:   s.ToolCallCount,
			CallsFromRows:   calls,
			Cost:            cost,
		}
		return nil
	})
	return v, err
}

// SweepExpired releases holds whose TTL has passed. Run it on daemon boot and
// periodically thereafter; it is the reservation half of crash recovery.
func (l *Ledger) SweepExpired(ctx context.Context) (int, error) {
	var n int
	err := l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = tx.ExpireStaleReservations(ctx, l.now())
		return err
	})
	return n, err
}

// Finish marks a session terminal.
func (l *Ledger) Finish(ctx context.Context, sessionID string, status core.SessionStatus) error {
	return l.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.SetSessionStatus(ctx, sessionID, status)
	})
}
