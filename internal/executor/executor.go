// Package executor runs a research session: plan, dispatch, settle, replan.
//
// This is §9.2's loop. One worker for now — M5 turns on a pool, and the shape
// here is deliberately the shape that survives that, because the parts that
// make concurrency safe are already in place: leases stop double dispatch, and
// reserve-before-dispatch stops the budget overshooting.
//
// The invariant that matters most is unglamorous: every reservation is resolved
// on every path. A settle that is skipped because a lead failed leaves budget
// held forever — neither spent nor available — and the session runs out of
// money it never used. Failure paths outnumber the success path here, which is
// why the settle is arranged so it cannot be missed rather than repeated at
// each exit.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/cache"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/planner"
	"github.com/lajosdeme/mole/internal/queue"
	"github.com/lajosdeme/mole/internal/store"
)

// Executor runs one session to completion.
type Executor struct {
	Store   store.Store
	Ledger  *budget.Ledger
	Queue   *queue.Queue
	Planner *planner.Planner
	Actors  map[core.ActorType]actors.Actor
	Log     *slog.Logger

	// Owner identifies this worker in a lease. M5 gives each worker its own.
	Owner string

	// Cache holds artifacts already researched in this session (§9.3). Shared
	// with the actor, so a lead-level hit and a URL-level hit are the same
	// cache and one lead's fetches serve another's.
	Cache *cache.Cache

	// Estimator sizes reservations. Nil uses a fresh one for the session's
	// budget unit.
	Estimator *budget.Estimator

	// Now and Jitter are injectable for tests.
	Now    func() time.Time
	Jitter func() float64

	// Sleep waits between retries. Injectable so a test does not spend the
	// backoff in real time.
	Sleep func(context.Context, time.Duration) error
}

// Result is what a session produced.
type Result struct {
	SessionID string
	Status    core.SessionStatus

	Digest *planner.Digest
	Claims []core.Claim

	LeadsRun    int
	LeadsFailed int
	LeadsCached int
	Replans     int

	// CacheStats reports whether the cache earned its keep. An unmeasured
	// cache is an assumption.
	CacheStats cache.Stats

	// Spent is the settled total in the session's budget unit.
	Spent int64
	// StoppedBecause names the ceiling or condition that ended the run, for
	// the report and the trace.
	StoppedBecause string
}

func (e *Executor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) jitter() float64 {
	if e.Jitter != nil {
		return e.Jitter()
	}
	return rand.Float64()
}

func (e *Executor) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run executes a session from prompt to exhausted queue.
//
// Escrow is already held from session creation (§8.3), so the loop can spend
// down to Available() without starving the report.
func (e *Executor) Run(ctx context.Context, sessionID string) (*Result, error) {
	sess, err := e.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	res := &Result{SessionID: sessionID, Status: core.StatusDone}
	digest := planner.NewDigest(sess.Prompt, 0)
	res.Digest = digest

	// leadQuestion maps a dispatched lead back to the sub-question it serves,
	// so coverage lands on the right line of the digest.
	//
	// In memory, so a resumed session loses the association and its digest
	// starts empty. Acceptable while M3 has no resume path; M5's pool shares
	// this executor, and a restart is a new Run.
	leadQuestion := map[string]string{}

	// 1. Decompose.
	plan, err := e.Planner.InitialLeads(ctx, sess)
	if err := e.settlePlanner(ctx, sess, plan, err); err != nil {
		res.Status = core.StatusFailed
		res.StoppedBecause = "planning failed: " + err.Error()
		return res, err
	}
	digest.AddQuestions(plan.Questions)
	for i := range plan.Leads {
		if plan.Leads[i].ID == "" {
			plan.Leads[i].ID = core.NewLeadID()
		}
		if i < len(plan.Questions) {
			leadQuestion[plan.Leads[i].ID] = plan.Questions[i].ID
		}
	}
	if err := e.Queue.Push(ctx, plan.Leads); err != nil {
		return res, err
	}

	// 2. Work the queue.
	completedSinceReplan := 0
	depth := 0

	for {
		if err := ctx.Err(); err != nil {
			res.Status = statusForContext(err)
			res.StoppedBecause = err.Error()
			break
		}

		sess, err = e.session(ctx, sessionID)
		if err != nil {
			return res, err
		}
		if hit, which := sess.HitCeiling(e.now()); hit {
			res.Status = core.StatusExhausted
			res.StoppedBecause = which
			break
		}
		if sess.Available() <= 0 {
			res.Status = core.StatusExhausted
			res.StoppedBecause = "budget exhausted"
			break
		}

		lease, err := e.Queue.LeaseNext(ctx, sessionID, e.owner())
		if err != nil {
			return res, err
		}

		if lease == nil {
			// Queue drained. One last replan can add work; if it does not, the
			// session is done.
			if !e.Planner.ShouldReplan(completedSinceReplan, true) {
				break
			}
			added, done, err := e.replan(ctx, sess, digest, depth, res, leadQuestion)
			if err != nil {
				res.Status = core.StatusFailed
				res.StoppedBecause = "replan failed: " + err.Error()
				return res, err
			}
			completedSinceReplan = 0
			if done || added == 0 {
				res.StoppedBecause = "no further leads"
				break
			}
			depth++
			continue
		}

		// A cache hit RETURNS the prior result; it does not skip the lead
		// (§9.3). Rev 1 skipped, so the planner asked for something and got
		// nothing back — and the next replan spawned an equivalent lead,
		// forever. Recording the coverage is what closes that loop.
		if e.completeFromCache(ctx, lease, digest, leadQuestion) {
			res.LeadsCached++
			completedSinceReplan++
			continue
		}

		outcome := e.runLead(ctx, sess, lease, digest, leadQuestion)
		res.LeadsRun++

		// Count the lead AFTER it runs, not before. Nothing else counts them:
		// BudgetDelta.LeadCount and the SQL applying it existed since M0, but
		// no caller ever set it, so MaxLeads — a documented §8.5 ceiling —
		// could never bind. The check was tested; the increment feeding it was
		// not.
		//
		// After, because the ledger enforces the same ceiling on Reserve.
		// Counting first makes the lead that reaches the ceiling fail on its
		// own reservation instead of running and stopping the next one, which
		// turns "we did the work we were allowed" into "the last lead errored".
		if err := e.countLead(ctx, sessionID); err != nil {
			e.logger().WarnContext(ctx, "could not count a lead; the max_leads ceiling may not bind",
				"lead", lease.Lead.ID, "err", err)
		}
		res.Claims = append(res.Claims, outcome.claims...)
		completedSinceReplan++

		if outcome.class == Fatal {
			// A ceiling is not a failure: the session did what it was allowed
			// to do. Reporting it as failed would make every correctly-bounded
			// run look broken.
			if errors.Is(outcome.err, budget.ErrInsufficientBudget) {
				res.Status = core.StatusExhausted
				res.StoppedBecause = ceilingReason(outcome.err)
				break
			}
			res.Status = core.StatusFailed
			res.StoppedBecause = "fatal: " + outcome.err.Error()
			// Escrow survives, so a partial report is still affordable (§9.5).
			return res, nil
		}
		if outcome.err != nil {
			res.LeadsFailed++
		}

		if e.Planner.ShouldReplan(completedSinceReplan, false) {
			added, done, err := e.replan(ctx, sess, digest, depth, res, leadQuestion)
			if err != nil {
				e.logger().WarnContext(ctx, "replan failed; continuing with the current queue", "err", err)
			}
			completedSinceReplan = 0
			if done {
				res.StoppedBecause = "planner reported done"
				break
			}
			if added > 0 {
				depth++
			}
		}
	}

	// Without a live context this read fails on a timed-out or cancelled run —
	// exactly the runs whose spend most needs reporting — and the result says
	// 0 spent while the ledger holds the real figure.
	final, err := e.session(context.WithoutCancel(ctx), sessionID)
	if err == nil {
		res.Spent = final.Spent
	}
	res.CacheStats = e.Cache.Stats()
	return res, nil
}

// completeFromCache satisfies a lead from a prior identical one.
//
// Reports the coverage to the digest before completing, which is the whole
// difference from rev 1: the planner learns the question was answered, so it
// stops re-proposing it. A hit costs nothing — the claims are already in the
// store under the lead that first found them, and re-inserting copies would
// inflate every claim count §14.3 reads without giving a reader anything.
func (e *Executor) completeFromCache(
	ctx context.Context,
	lease *queue.Lease,
	digest *planner.Digest,
	leadQuestion map[string]string,
) bool {
	if e.Cache == nil {
		return false
	}
	key := cache.QueryKey(lease.Lead.Query)
	entry, ok := e.Cache.Get(key)
	if !ok {
		return false
	}

	questionID := leadQuestion[lease.Lead.ID]
	digest.RecordLead(questionID)
	if entry.Claims > 0 {
		digest.RecordClaims(questionID, entry.Claims)
	} else {
		digest.RecordDeadEnd("no_evidence", lease.Lead.Query)
	}

	if err := e.Queue.Complete(ctx, lease, core.LeadSkippedCache); err != nil {
		e.logger().WarnContext(ctx, "could not complete a cached lead", "lead", lease.Lead.ID, "err", err)
	}
	return true
}

type leadOutcome struct {
	claims  []core.Claim
	summary string
	err     error
	class   Class
}

// runLead reserves, runs, settles, and completes one lead.
//
// Retries live here rather than inside the actor because a transient failure
// means the LEAD should be retried — a new search, new fetches — and the actor
// has no notion of being run twice.
func (e *Executor) runLead(
	ctx context.Context,
	sess *core.Session,
	lease *queue.Lease,
	digest *planner.Digest,
	leadQuestion map[string]string,
) leadOutcome {
	lead := *lease.Lead
	questionID := leadQuestion[lead.ID]
	digest.RecordLead(questionID)

	actor, ok := e.Actors[lead.ActorType]
	if !ok {
		err := fmt.Errorf("executor: no actor registered for %q", lead.ActorType)
		digest.RecordDeadEnd("no_actor", lead.Query)
		_ = e.Queue.Complete(ctx, lease, core.LeadFailed)
		return leadOutcome{err: err, class: Fatal}
	}

	var last leadOutcome
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := e.sleep(ctx, Backoff(attempt-1, e.jitter)); err != nil {
				last.err, last.class = err, Degraded
				break
			}
			// A retry re-runs the whole lead and is charged again, so a lost
			// lease has to stop it: another worker may already be running this.
			if ok, err := e.Queue.Renew(ctx, lease); err != nil || !ok {
				e.logger().WarnContext(ctx, "lease lost mid-retry; abandoning the lead",
					"lead", lead.ID, "attempt", attempt)
				return leadOutcome{err: errors.New("executor: lease lost"), class: Degraded}
			}
		}

		out := e.attempt(ctx, sess, lead, actor)
		last = out

		if out.err == nil {
			digest.RecordClaims(questionID, len(out.claims))
			if len(out.claims) == 0 {
				// A lead that ran cleanly and found nothing is still a dead end
				// for the planner: the question was asked and not answered.
				digest.RecordDeadEnd("no_evidence", lead.Query)
			}
			e.Cache.Put(&cache.Entry{
				Key:     cache.QueryKey(lead.Query),
				Claims:  len(out.claims),
				Summary: out.summary,
			})
			_ = e.Queue.Complete(ctx, lease, core.LeadDone)
			return out
		}
		if out.class != Transient {
			break
		}
		e.logger().WarnContext(ctx, "lead failed transiently; retrying",
			"lead", lead.ID, "attempt", attempt, "err", out.err)
	}

	// Out of attempts, or not worth retrying. §9.5: a transient error that
	// exhausts its retries becomes degraded — the session continues.
	digest.RecordDeadEnd(DeadEndCause(last.err), lead.Query)
	// Claims from a partial run are kept: the actor verified every quote it
	// returned, and discarding them because a later chunk failed throws away
	// evidence that was paid for.
	if last.class != Fatal {
		last.class = Degraded
	}
	_ = e.Queue.Complete(ctx, lease, core.LeadFailed)
	return last
}

// attempt is one reserve → run → settle cycle.
func (e *Executor) attempt(ctx context.Context, sess *core.Session, lead core.Lead, actor actors.Actor) leadOutcome {
	est := e.estimate(sess, lead)
	if avail := sess.Available(); est > avail {
		est = avail
	}
	if est <= 0 {
		return leadOutcome{err: budget.ErrInsufficientBudget, class: Fatal}
	}

	reservation, err := e.Ledger.ReserveFor(ctx, sess.ID, lead.ID, est)
	if err != nil {
		return leadOutcome{err: err, class: Classify(err)}
	}

	result, runErr := actor.Run(ctx, lead)

	// Settle unconditionally. The tokens were billed either way, and a
	// reservation left held is budget neither spent nor available.
	var costs []core.ToolCall
	if result != nil {
		costs = result.Costs
	}
	if _, err := e.Ledger.Settle(context.WithoutCancel(ctx), reservation, costs); err != nil {
		e.logger().ErrorContext(ctx, "settle failed; budget accounting is now unreliable",
			"lead", lead.ID, "err", err)
		return leadOutcome{err: err, class: Fatal}
	}

	out := leadOutcome{}
	if result != nil {
		out.claims = result.Claims
		out.summary = result.Summary
	}
	if runErr != nil {
		out.err, out.class = runErr, Classify(runErr)
	}
	return out
}

// replan asks the planner what to do next and queues the answer.
func (e *Executor) replan(
	ctx context.Context,
	sess *core.Session,
	digest *planner.Digest,
	depth int,
	res *Result,
	leadQuestion map[string]string,
) (added int, done bool, err error) {
	plan, perr := e.Planner.Replan(ctx, sess, digest, depth)
	if err := e.settlePlanner(ctx, sess, plan, perr); err != nil {
		return 0, false, err
	}
	res.Replans++

	for _, id := range plan.Answered {
		digest.MarkAnswered(id)
	}
	if plan.Done {
		return 0, true, nil
	}

	digest.AddQuestions(plan.Questions)
	for i := range plan.Leads {
		if plan.Leads[i].ID == "" {
			plan.Leads[i].ID = core.NewLeadID()
		}
		if i < len(plan.Questions) {
			leadQuestion[plan.Leads[i].ID] = plan.Questions[i].ID
		}
	}
	if len(plan.Leads) == 0 {
		return 0, false, nil
	}
	if err := e.Queue.Push(ctx, plan.Leads); err != nil {
		return 0, false, err
	}
	return len(plan.Leads), false, nil
}

// settlePlanner charges a planning call.
//
// Planner tokens are spent whether or not the plan parsed, and they are charged
// to the planner role so §14.3's role breakdown can show what planning cost —
// the number that says whether the digest is doing its job.
func (e *Executor) settlePlanner(ctx context.Context, sess *core.Session, plan *planner.Plan, callErr error) error {
	if plan == nil {
		return callErr
	}
	if plan.Usage.IsZero() {
		return callErr
	}

	reservation, err := e.Ledger.Reserve(ctx, sess.ID, 1)
	if err != nil {
		// No budget left to even record the call. The cost is real, so say so
		// loudly rather than dropping it silently.
		e.logger().ErrorContext(ctx, "could not reserve to settle a planner call; cost unrecorded",
			"session", sess.ID, "err", err)
		return callErr
	}

	tc := core.ToolCall{
		SessionID: sess.ID,
		Role:      core.RolePlanner,
		Type:      core.CallLLM,
		Model:     plan.Model,
		Input:     "plan",
		Cost: core.Cost{
			InputTokens:      plan.Usage.InputTokens,
			OutputTokens:     plan.Usage.OutputTokens,
			CacheReadTokens:  plan.Usage.CacheReadTokens,
			CacheWriteTokens: plan.Usage.CacheWriteTokens,
		},
	}
	if callErr != nil {
		tc.Err = callErr.Error()
	}
	if _, err := e.Ledger.Settle(context.WithoutCancel(ctx), reservation, []core.ToolCall{tc}); err != nil {
		e.logger().ErrorContext(ctx, "settling a planner call failed", "err", err)
	}
	return callErr
}

// countLead increments the session's dispatched-lead counter, which is what
// makes MaxLeads enforceable.
func (e *Executor) countLead(ctx context.Context, sessionID string) error {
	return e.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.ApplyBudgetDelta(ctx, sessionID, store.BudgetDelta{LeadCount: 1})
	})
}

func (e *Executor) estimate(sess *core.Session, lead core.Lead) int64 {
	est := e.Estimator
	if est == nil {
		est = budget.NewEstimator(sess.BudgetUnit)
	}
	return est.For(lead.ActorType, lead.Depth)
}

func (e *Executor) owner() string {
	if e.Owner != "" {
		return e.Owner
	}
	return "executor"
}

func (e *Executor) session(ctx context.Context, id string) (*core.Session, error) {
	var s *core.Session
	err := e.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, id)
		return err
	})
	return s, err
}

// ceilingReason pulls the ceiling name out of the ledger's error, so the
// report says "max_leads" rather than quoting a sentence at the user.
func ceilingReason(err error) string {
	msg := err.Error()
	for _, name := range []string{"max_tool_calls", "max_leads", "max_wallclock"} {
		if strings.Contains(msg, name) {
			return name
		}
	}
	return "budget exhausted"
}

func statusForContext(err error) core.SessionStatus {
	if errors.Is(err, context.DeadlineExceeded) {
		return core.StatusExhausted
	}
	return core.StatusCancelled
}
