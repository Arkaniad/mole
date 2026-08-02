package queue_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/queue"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

func newQ(t *testing.T, ttl time.Duration) (*queue.Queue, *sqlite.DB, string) {
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

	sess, err := budget.New(db, budget.DefaultConfig()).CreateSession(ctx, budget.SessionSpec{
		Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: 10 * core.MicrosPerUSD,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return queue.New(db, ttl), db, sess.ID
}

func leads(sessionID string, n int) []core.Lead {
	out := make([]core.Lead, n)
	for i := range out {
		out[i] = core.Lead{
			SessionID: sessionID, ActorType: core.ActorWeb,
			Query: fmt.Sprintf("query %d", i), Status: core.LeadQueued,
		}
	}
	return out
}

// TestNoLeadIsDispatchedTwice is the property everything else rests on.
//
// Two workers running the same lead pay for it twice, and the ledger cannot
// detect that: both charges are real and both are correctly recorded. The
// budget just drains faster than the work justifies, which looks like an
// expensive model rather than a bug.
//
// What this actually verifies is the property under the CURRENT store. §7.1's
// single-writer pool serializes the enclosing transaction, so a naive
// select-then-update also passes — checked by writing that version. The
// single-statement lease in queries.go is insurance for a backend that does
// not serialize, and no test here can distinguish the two.
func TestNoLeadIsDispatchedTwice(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)

	const n = 60
	if err := q.Push(ctx, leads(sid, n)); err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		seen  = map[string]int{}
		total int
	)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", w)
			for {
				lease, err := q.LeaseNext(ctx, sid, owner)
				if err != nil {
					t.Errorf("lease: %v", err)
					return
				}
				if lease == nil {
					return
				}
				mu.Lock()
				seen[lease.Lead.ID]++
				total++
				mu.Unlock()
				_ = q.Complete(ctx, lease, core.LeadDone)
			}
		}(w)
	}
	wg.Wait()

	if total != n {
		t.Errorf("dispatched %d leads, want %d", total, n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("lead %s dispatched %d times", id, count)
		}
	}
}

// TestPriorityJumpsTheQueue: a verifier follow-up (§11.1) has to be able to
// overtake the original leads, or a contradiction found early waits behind
// every remaining lead and may never run before the budget is gone.
func TestPriorityJumpsTheQueue(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)

	base := leads(sid, 3)
	if err := q.Push(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, []core.Lead{{
		SessionID: sid, ActorType: core.ActorWeb, Query: "urgent follow-up",
		Status: core.LeadQueued, Priority: 10,
	}}); err != nil {
		t.Fatal(err)
	}

	lease, err := q.LeaseNext(ctx, sid, "w")
	if err != nil || lease == nil {
		t.Fatalf("lease: %v %v", lease, err)
	}
	if lease.Lead.Query != "urgent follow-up" {
		t.Errorf("got %q first, want the high-priority lead", lease.Lead.Query)
	}
}

// TestEqualPriorityIsFIFO so the planner's ordering survives.
func TestEqualPriorityIsFIFO(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	if err := q.Push(ctx, leads(sid, 4)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		lease, err := q.LeaseNext(ctx, sid, "w")
		if err != nil || lease == nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		if want := fmt.Sprintf("query %d", i); lease.Lead.Query != want {
			t.Errorf("position %d = %q, want %q", i, lease.Lead.Query, want)
		}
		_ = q.Complete(ctx, lease, core.LeadDone)
	}
}

// TestExpiredLeaseIsRecovered is §9.4. A worker that dies mid-lead leaves a row
// nothing will revisit; rev 1 had no way out of that state.
func TestExpiredLeaseIsRecovered(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	q.SetClock(func() time.Time { return now })

	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	lease, err := q.LeaseNext(ctx, sid, "doomed-worker")
	if err != nil || lease == nil {
		t.Fatal("no lease")
	}

	// The worker dies here. Nothing completes or releases the lead.
	if next, _ := q.LeaseNext(ctx, sid, "other"); next != nil {
		t.Error("a leased lead was handed to a second worker before expiry")
	}

	now = now.Add(2 * time.Minute)
	n, err := q.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d leads, want 1", n)
	}

	recovered, err := q.LeaseNext(ctx, sid, "replacement")
	if err != nil || recovered == nil {
		t.Fatal("the stranded lead was not recoverable")
	}
	if recovered.Lead.ID != lease.Lead.ID {
		t.Error("swept the wrong lead")
	}
}

// TestSweepLeavesLiveLeasesAlone: sweeping a lease that is merely slow would
// hand the lead to a second worker while the first is still running it — the
// double-dispatch this package exists to prevent, caused by the recovery for it.
func TestSweepLeavesLiveLeasesAlone(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Hour)
	now := time.Unix(1_700_000_000, 0)
	q.SetClock(func() time.Time { return now })

	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.LeaseNext(ctx, sid, "w"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(30 * time.Minute) // well inside the hour
	if n, _ := q.Sweep(ctx); n != 0 {
		t.Errorf("swept %d live leases", n)
	}
}

// TestRenewKeepsALongLeadAlive. A lead can legitimately outlast the TTL — a
// slow fetch plus a slow model call — and losing the lease mid-run means paying
// for the same work twice.
func TestRenewKeepsALongLeadAlive(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, 10*time.Minute)
	now := time.Unix(1_700_000_000, 0)
	q.SetClock(func() time.Time { return now })

	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	lease, _ := q.LeaseNext(ctx, sid, "w")

	for i := 0; i < 3; i++ {
		now = now.Add(8 * time.Minute)
		ok, err := q.Renew(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("renewal %d rejected while the lease was still held", i)
		}
		if n, _ := q.Sweep(ctx); n != 0 {
			t.Errorf("renewal %d: lease swept anyway", i)
		}
	}
}

// TestRenewFailsAfterTheSweep. A worker that keeps going after losing its lease
// is racing whoever holds it now, so it has to be told rather than left to
// finish and write claims for a lead someone else is also running.
func TestRenewFailsAfterTheSweep(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	q.SetClock(func() time.Time { return now })

	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	lease, _ := q.LeaseNext(ctx, sid, "stalled")

	now = now.Add(2 * time.Minute)
	if _, err := q.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	ok, err := q.Renew(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a swept lease renewed successfully; the worker would keep running")
	}
}

// TestReleasePutsTheLeadBack, for a worker stopping cleanly on a ceiling.
func TestReleasePutsTheLeadBack(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}

	lease, _ := q.LeaseNext(ctx, sid, "w")
	if err := q.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}

	again, err := q.LeaseNext(ctx, sid, "w2")
	if err != nil || again == nil {
		t.Fatal("a released lead was not re-dispatched")
	}
}

// TestReleaseByTheWrongOwnerIsIgnored: a worker that lost its lease must not be
// able to yank the lead out from under the one that now holds it.
func TestReleaseByTheWrongOwnerIsIgnored(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	lease, _ := q.LeaseNext(ctx, sid, "real-owner")

	impostor := &queue.Lease{Lead: lease.Lead, Owner: "someone-else"}
	if err := q.Release(ctx, impostor); err != nil {
		t.Fatal(err)
	}
	if next, _ := q.LeaseNext(ctx, sid, "w"); next != nil {
		t.Error("a non-owner released someone else's lease")
	}
}

// TestEmptyQueueReturnsNilNotError so the worker loop can use it as a stop
// condition rather than pattern-matching on an error.
func TestEmptyQueueReturnsNilNotError(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	lease, err := q.LeaseNext(ctx, sid, "w")
	if err != nil {
		t.Fatalf("empty queue returned an error: %v", err)
	}
	if lease != nil {
		t.Error("leased something from an empty queue")
	}
}

// TestStatsTrackProgress: the loop uses Pending to decide when it is done.
func TestStatsTrackProgress(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	if err := q.Push(ctx, leads(sid, 3)); err != nil {
		t.Fatal(err)
	}

	s, _ := q.Stats(ctx, sid)
	if s.Queued != 3 || s.Pending() != 3 {
		t.Errorf("after push: %+v", s)
	}

	l1, _ := q.LeaseNext(ctx, sid, "w")
	s, _ = q.Stats(ctx, sid)
	if s.Queued != 2 || s.Leased != 1 || s.Pending() != 3 {
		t.Errorf("while leased: %+v — in-flight work is still pending", s)
	}

	_ = q.Complete(ctx, l1, core.LeadDone)
	l2, _ := q.LeaseNext(ctx, sid, "w")
	_ = q.Complete(ctx, l2, core.LeadFailed)
	l3, _ := q.LeaseNext(ctx, sid, "w")
	_ = q.Complete(ctx, l3, core.LeadSkippedCache)

	s, _ = q.Stats(ctx, sid)
	if s.Pending() != 0 || s.Done != 1 || s.Failed != 1 || s.Cached != 1 || s.Total() != 3 {
		t.Errorf("after completion: %+v", s)
	}
}

// TestCompleteRejectsNonTerminalStatus: marking a lead "queued" as completion
// would leave it dispatchable forever.
func TestCompleteRejectsNonTerminalStatus(t *testing.T) {
	ctx := context.Background()
	q, _, sid := newQ(t, time.Minute)
	if err := q.Push(ctx, leads(sid, 1)); err != nil {
		t.Fatal(err)
	}
	lease, _ := q.LeaseNext(ctx, sid, "w")

	if err := q.Complete(ctx, lease, core.LeadQueued); err == nil {
		t.Error("completing a lead as queued was accepted")
	}
}

// TestSweepIsCrossSession: recovery at boot does not know which sessions were
// in flight, so it has to sweep everything.
func TestSweepIsCrossSession(t *testing.T) {
	ctx := context.Background()
	q, db, sid1 := newQ(t, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	q.SetClock(func() time.Time { return now })

	sess2, err := budget.New(db, budget.DefaultConfig()).CreateSession(ctx, budget.SessionSpec{
		Prompt: "q2", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: core.MicrosPerUSD,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := q.Push(ctx, leads(sid1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, leads(sess2.ID, 1)); err != nil {
		t.Fatal(err)
	}
	_, _ = q.LeaseNext(ctx, sid1, "w")
	_, _ = q.LeaseNext(ctx, sess2.ID, "w")

	now = now.Add(2 * time.Minute)
	n, err := q.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("swept %d, want 2 across both sessions", n)
	}
}
