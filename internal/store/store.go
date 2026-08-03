// Package store defines the persistence boundary.
//
// Everything above this package is dialect-agnostic. The interface is split
// into Queries (reads) and Tx (reads + writes inside one transaction) so that
// the budget invariants — check available, insert hold, update session — happen
// atomically without the ledger logic being duplicated per backend.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/lajosdeme/mole/internal/core"
)

var (
	// ErrNotFound is returned by every getter when the row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a write violates an invariant the schema
	// enforces (e.g. a settle that would drive held negative).
	ErrConflict = errors.New("store: conflict")
)

// Store is the entry point. Reads and writes are deliberately separate calls:
// on SQLite the write path is a single serialized connection while reads run
// concurrently under WAL, and callers should not have to know that.
type Store interface {
	// WithTx runs fn inside a write transaction, committing on nil and rolling
	// back on error. fn must be idempotent under retry.
	WithTx(ctx context.Context, fn func(context.Context, Tx) error) error

	// Read runs fn against a read-only view.
	Read(ctx context.Context, fn func(context.Context, Queries) error) error

	// Migrate applies any pending schema migrations.
	Migrate(ctx context.Context) error

	Close() error
}

// BudgetDelta is an atomic adjustment to a session's counters. Every field is
// a delta, never an absolute, so concurrent settles compose correctly.
type BudgetDelta struct {
	Spent         int64
	Held          int64
	Escrow        int64
	ToolCallCount int64
	LeadCount     int64
}

// FetchOutcome is one recorded fetch attempt (§10.4). It lives in this package
// rather than in tools/fetch so the store does not depend on the fetcher.
type FetchOutcome struct {
	ID         string
	SessionID  *string
	LeadID     *string
	URL        string
	Domain     string
	Outcome    string
	StatusCode int
	Bytes      int64
	Duration   time.Duration
	Err        string
	CreatedAt  time.Time
}

// FetchStat is one row of the outcome mix: how often a cause occurred and
// which domains it concentrated in.
type FetchStat struct {
	Outcome string
	Count   int64
	Domains []DomainCount
}

type DomainCount struct {
	Domain string
	Count  int64
}

// Queries is the read surface.
type Queries interface {
	GetSession(ctx context.Context, id string) (*core.Session, error)
	ListSessions(ctx context.Context, limit int) ([]*core.Session, error)

	GetReservation(ctx context.Context, id string) (*core.Reservation, error)
	SumHeldReservations(ctx context.Context, sessionID string) (int64, error)

	SumCosts(ctx context.Context, sessionID string) (core.Cost, error)
	SumCostsByRole(ctx context.Context, sessionID string) (map[core.Role]core.Cost, error)
	CountToolCalls(ctx context.Context, sessionID string) (int64, error)
	ListToolCalls(ctx context.Context, sessionID string, limit int) ([]*core.ToolCall, error)

	ListSpans(ctx context.Context, sessionID string) ([]*core.Span, error)

	// FetchOutcomeStats returns the outcome mix, most frequent first, with the
	// top domains per cause. topDomains <= 0 omits the domain breakdown.
	FetchOutcomeStats(ctx context.Context, since time.Time, topDomains int) ([]FetchStat, error)

	// ListFetchOutcomes returns one session's rows. The aggregate above cannot
	// answer per-URL questions, and citation verification needs to know which
	// sources were never fetched — re-reading those compares against a
	// different extraction and manufactures mismatches.
	ListFetchOutcomes(ctx context.Context, sessionID string, limit int) ([]*FetchOutcome, error)

	GetLead(ctx context.Context, id string) (*core.Lead, error)
	ListLeads(ctx context.Context, sessionID string, limit int) ([]*core.Lead, error)

	// CountLeadsByStatus is how the loop knows whether work remains. A read, so
	// a progress display never contends with the writer (§7.1).
	CountLeadsByStatus(ctx context.Context, sessionID string) (map[core.LeadStatus]int, error)

	ListClaims(ctx context.Context, sessionID string, limit int) ([]*core.Claim, error)
	CountClaims(ctx context.Context, sessionID string) (int64, error)
}

// Tx is the write surface. It embeds Queries so a transaction can read its own
// uncommitted state — required for check-then-write budget operations.
type Tx interface {
	Queries

	InsertSession(ctx context.Context, s *core.Session) error
	SetSessionStatus(ctx context.Context, id string, status core.SessionStatus) error
	ApplyBudgetDelta(ctx context.Context, sessionID string, d BudgetDelta) error

	InsertReservation(ctx context.Context, r *core.Reservation) error
	ResolveReservation(ctx context.Context, id string, status core.ReservationStatus, at time.Time) error
	ExpireStaleReservations(ctx context.Context, now time.Time) (int, error)

	InsertToolCall(ctx context.Context, tc *core.ToolCall) error

	RecordFetchOutcome(ctx context.Context, o *FetchOutcome) error

	InsertLead(ctx context.Context, l *core.Lead) error
	// SetLeadStatus moves a lead to a terminal state. owner must match the
	// lease holder; an empty owner skips the check, for callers that legitimately
	// have no lease. Without it a worker that lost its lease could terminalize a
	// lead another worker now holds.
	SetLeadStatus(ctx context.Context, id, owner string, status core.LeadStatus) error

	// LeaseNextLead atomically claims the highest-priority queued lead.
	//
	// Dequeue and lease must be one statement. Two workers that SELECT then
	// UPDATE will both see the same row and both run it — paying twice for one
	// lead, which the budget cannot detect because both charges are real.
	// Returns nil when the queue is empty.
	LeaseNextLead(ctx context.Context, sessionID, owner string, expires time.Time) (*core.Lead, error)

	// RenewLease extends a lease the worker still holds. Reports false when the
	// lease was taken by the recovery sweep, which is how a worker learns it
	// stalled long enough to be presumed dead.
	RenewLease(ctx context.Context, leadID, owner string, expires time.Time) (bool, error)

	// ReleaseLease returns a lead to the queue without completing it.
	ReleaseLease(ctx context.Context, leadID, owner string) error

	// SweepExpiredLeases requeues leads whose worker died. Without it a crash
	// strands them as leased forever, with no way out (§9.4).
	//
	// sessionID scopes the sweep. Empty means every session, which is what boot
	// recovery needs; a running executor must pass its own, or it requeues
	// another live process's in-flight leads and both end up running the lead.
	SweepExpiredLeases(ctx context.Context, sessionID string, now time.Time) (int, error)

	// InsertClaims writes a batch in one transaction. Claims from one actor
	// run land together or not at all: a partial batch would leave the graph
	// citing a lead that reported failure.
	InsertClaims(ctx context.Context, claims []core.Claim) error

	StartSpan(ctx context.Context, s *core.Span) error
	EndSpan(ctx context.Context, id string, endedAt time.Time, status string) error
}
