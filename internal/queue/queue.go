// Package queue is the lead queue: dispatch, leases, and recovery.
//
// Leads are LEASED rather than marked running (§9.4). The difference only shows
// up when something goes wrong, which is when it matters: a worker that dies
// mid-lead leaves a `running` row nothing will ever revisit, and rev 1 had no
// way out of that. A lease expires, and an expired lease is a lead the next
// sweep puts back.
//
// The single property everything else rests on: a lead is dispatched to at most
// one worker. Two workers running the same lead pay for it twice, and the
// ledger cannot detect that — both charges are real, both are correctly
// recorded, and the budget simply drains faster than the work justifies.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// DefaultLeaseTTL is how long a worker may hold a lead without a heartbeat.
//
// Long enough that a slow fetch plus a slow model call does not lose the lease,
// short enough that a crash does not strand work for the length of a session.
const DefaultLeaseTTL = 5 * time.Minute

// Queue dispatches leads for one session.
type Queue struct {
	st  store.Store
	ttl time.Duration
	now func() time.Time
}

// New creates a queue. A zero ttl uses DefaultLeaseTTL.
func New(st store.Store, ttl time.Duration) *Queue {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	return &Queue{st: st, ttl: ttl, now: time.Now}
}

// SetClock overrides time, for tests.
func (q *Queue) SetClock(now func() time.Time) { q.now = now }

// Lease is a worker's claim on one lead.
type Lease struct {
	Lead    *core.Lead
	Owner   string
	Expires time.Time
}

// Push adds leads to the queue.
//
// Depth and parent are carried from the caller: the lead tree is what caps
// recursion (§9.1), and a follow-up that forgot its parent would restart the
// depth count and make the cap unenforceable.
func (q *Queue) Push(ctx context.Context, leads []core.Lead) error {
	if len(leads) == 0 {
		return nil
	}
	return q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := range leads {
			l := &leads[i]
			if l.ID == "" {
				l.ID = core.NewLeadID()
			}
			if l.Status == "" {
				l.Status = core.LeadQueued
			}
			if err := tx.InsertLead(ctx, l); err != nil {
				return err
			}
		}
		return nil
	})
}

// LeaseNext claims the highest-priority queued lead, or returns nil when the
// queue is empty.
func (q *Queue) LeaseNext(ctx context.Context, sessionID, owner string) (*Lease, error) {
	var lease *Lease
	expires := q.now().Add(q.ttl)

	err := q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		lead, err := tx.LeaseNextLead(ctx, sessionID, owner, expires)
		if err != nil || lead == nil {
			return err
		}
		lease = &Lease{Lead: lead, Owner: owner, Expires: expires}
		return nil
	})
	return lease, err
}

// Renew extends a lease.
//
// Reports false when the lease is gone — swept as expired, and possibly already
// re-leased to another worker. A worker that keeps going after that is racing
// the one that now owns the lead, so the caller must stop rather than finish.
func (q *Queue) Renew(ctx context.Context, l *Lease) (bool, error) {
	expires := q.now().Add(q.ttl)
	var ok bool
	err := q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		ok, err = tx.RenewLease(ctx, l.Lead.ID, l.Owner, expires)
		return err
	})
	if ok {
		l.Expires = expires
	}
	return ok, err
}

// Complete marks a lead finished.
func (q *Queue) Complete(ctx context.Context, l *Lease, status core.LeadStatus) error {
	switch status {
	case core.LeadDone, core.LeadFailed, core.LeadSkippedCache:
	default:
		return fmt.Errorf("queue: %q is not a terminal lead status", status)
	}
	return q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.SetLeadStatus(ctx, l.Lead.ID, status)
	})
}

// Release returns a lead to the queue without completing it.
func (q *Queue) Release(ctx context.Context, l *Lease) error {
	return q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.ReleaseLease(ctx, l.Lead.ID, l.Owner)
	})
}

// Sweep requeues every lead whose lease has expired, across all sessions.
//
// Run at boot and periodically. At boot it is the recovery step §9.4 describes:
// after a crash, every lead the dead process held is leased with an expiry in
// the past, and without this they stay that way.
func (q *Queue) Sweep(ctx context.Context) (int, error) {
	var n int
	err := q.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = tx.SweepExpiredLeases(ctx, q.now())
		return err
	})
	return n, err
}

// Stats reports the lead counts by status.
type Stats struct {
	Queued, Leased, Done, Failed, Cached int
}

// Pending reports work that is not finished — queued plus in flight.
func (s Stats) Pending() int { return s.Queued + s.Leased }

// Total is every lead ever created for the session.
func (s Stats) Total() int { return s.Queued + s.Leased + s.Done + s.Failed + s.Cached }

// Stats counts the session's leads by status.
func (q *Queue) Stats(ctx context.Context, sessionID string) (Stats, error) {
	var out Stats
	err := q.st.Read(ctx, func(ctx context.Context, qs store.Queries) error {
		counts, err := qs.CountLeadsByStatus(ctx, sessionID)
		if err != nil {
			return err
		}
		out = Stats{
			Queued: counts[core.LeadQueued],
			Leased: counts[core.LeadLeased],
			Done:   counts[core.LeadDone],
			Failed: counts[core.LeadFailed],
			Cached: counts[core.LeadSkippedCache],
		}
		return nil
	})
	return out, err
}
