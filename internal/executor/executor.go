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
	"github.com/lajosdeme/mole/internal/pricing"
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

	// Progress reports phase transitions as they happen. Optional.
	//
	// Not decoration. Planning is a single model call with no output until it
	// returns, and on a local model that is minutes of silence — three runs were
	// killed by hand because the CLI printed a header and then nothing, which
	// reads as a hang rather than as work.
	Progress func(Event)

	// Cache holds artifacts already researched in this session (§9.3). Shared
	// with the actor, so a lead-level hit and a URL-level hit are the same
	// cache and one lead's fetches serve another's.
	Cache *cache.Cache

	// Pricing and CheapModel convert a USD reservation into the token ceiling
	// an actor can act on. CheapModel names the tier chunk mining actually
	// uses; pricing the ceiling off the strong model would set it several
	// times too low. Both empty leaves the actor's configured budget in place.
	Pricing    *pricing.Table
	CheapModel string

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

// Event is a step the caller may want to show.
type Event struct {
	Phase string // "planning", "executing", "replanning", "lead", "cached"
	// Detail is a short human-readable note, already formatted.
	Detail string
}

func (e *Executor) emit(phase, detail string) {
	if e.Progress != nil {
		e.Progress(Event{Phase: phase, Detail: detail})
	}
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

	// One estimator for the whole session, so Observe actually accumulates.
	// It had no callers at all, and e.estimate built a fresh one per call, so
	// every reservation was the cold seed forever and §8.4's "improves with
	// use" never happened. That mattered more once the sub-budget started
	// deriving the actor's ceiling from the reservation: a permanently wrong
	// estimate now caps how much work each lead may do.
	if e.Estimator == nil {
		e.Estimator = budget.NewEstimator(sess.BudgetUnit)
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
	e.emit("planning", "decomposing the question")
	plan, err := e.planWithBudget(ctx, sess, func() (*planner.Plan, error) {
		return e.Planner.InitialLeads(ctx, sess)
	})
	if err != nil {
		// With no initial plan there is nothing to research, so this ends the
		// session either way — but WHY it ended is not the same thing. A user
		// pressing Ctrl-C during the first planner call is a cancellation, and
		// reporting it as a failure makes a deliberate interrupt look like a
		// crash to whoever reads the status or the exit code. The main loop
		// already distinguishes these; this path did not.
		res.Status = core.StatusFailed
		if ctxErr := ctx.Err(); ctxErr != nil {
			res.Status = statusForContext(ctxErr)
		}
		res.StoppedBecause = "planning failed: " + err.Error()
		return res, nil
	}
	// Map leads to the IDs the DIGEST assigned, not the ones the model supplied
	// — those collide across replans, and a positional mapping onto the input
	// credits a new question's coverage to whatever it collided with.
	assigned := digest.AddQuestions(plan.Questions)
	for i := range plan.Leads {
		if plan.Leads[i].ID == "" {
			plan.Leads[i].ID = core.NewLeadID()
		}
		if i < len(assigned) {
			leadQuestion[plan.Leads[i].ID] = assigned[i].ID
		}
	}
	if err := e.Queue.Push(ctx, plan.Leads); err != nil {
		return res, err
	}
	e.emit("executing", fmt.Sprintf("%d sub-question(s) queued", len(plan.Leads)))

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
				// A failed replan at drain time is the END of the research, not
				// a failed session. §9.5 classifies 429/5xx/timeout as transient
				// — never abort — and this used to return, so a complete run
				// with claims collected reported as failed with a non-zero exit
				// because its last planner call got throttled. The mid-loop
				// replan below already only warns; the two now agree.
				e.logger().WarnContext(ctx, "final replan failed; ending with what was found", "err", err)
				res.StoppedBecause = "final replan failed: " + err.Error()
				break
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
			e.emit("cached", truncateQuery(lease.Lead.Query))
			res.LeadsCached++
			completedSinceReplan++
			continue
		}

		e.emit("lead", truncateQuery(lease.Lead.Query))
		outcome := e.runLead(ctx, sess, lease, digest, leadQuestion)
		res.LeadsRun++
		e.emit("lead-done", fmt.Sprintf("%d claim(s)%s",
			len(outcome.claims), leadNote(outcome)))

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
		if err := e.countLead(context.WithoutCancel(ctx), sessionID); err != nil {
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

	e.complete(ctx, lease, core.LeadSkippedCache)
	return true
}

type leadOutcome struct {
	claims []core.Claim
	err    error
	class  Class
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
		e.complete(ctx, lease, core.LeadFailed)
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

		out := e.attempt(ctx, sess, lead, actor, lease)
		last = out

		if out.err == nil {
			digest.RecordClaims(questionID, len(out.claims))
			if len(out.claims) == 0 {
				// A lead that ran cleanly and found nothing is still a dead end
				// for the planner: the question was asked and not answered.
				digest.RecordDeadEnd("no_evidence", lead.Query)
			}
			e.Cache.Put(&cache.Entry{
				Key:    cache.QueryKey(lead.Query),
				Claims: len(out.claims),
			})
			e.complete(ctx, lease, core.LeadDone)
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
	e.complete(ctx, lease, core.LeadFailed)
	return last
}

// attempt is one reserve → run → settle cycle.
func (e *Executor) attempt(ctx context.Context, sess *core.Session, lead core.Lead, actor actors.Actor, lease *queue.Lease) leadOutcome {
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

	// §9.2's withSubBudget. The reservation is the only ceiling that knows what
	// this lead may cost; without passing it the actor's configured budget bore
	// no relation to the money held for it.
	// Heartbeat while the lead runs. Renew used to be called only BETWEEN
	// retries, so a lead that legitimately outlasted the lease TTL — a slow
	// fetch plus a slow model call — silently lost its lease and became eligible
	// for a sweep, which is §9.4's "workers heartbeat their lease" unimplemented.
	stopHeartbeat := e.heartbeat(ctx, lease)
	result, runErr := actor.Run(actors.WithSubBudget(ctx, e.subBudget(sess, est)), lead)
	stopHeartbeat()

	// Settle unconditionally. The tokens were billed either way, and a
	// reservation left held is budget neither spent nor available.
	var costs []core.ToolCall
	if result != nil {
		costs = result.Costs
	}
	settled, err := e.Ledger.Settle(context.WithoutCancel(ctx), reservation, costs)
	if err != nil {
		e.logger().ErrorContext(ctx, "settle failed; budget accounting is now unreliable",
			"lead", lead.ID, "err", err)
		return leadOutcome{err: err, class: Fatal}
	}
	// Feed the estimate back before anything else. Even an overshoot is a
	// sample — arguably the most valuable one, since it is the case the seed
	// got wrong.
	e.Estimator.Observe(lead.ActorType, lead.Depth, settled.Charged)

	// §8.2 bounds overshoot to estimate error on a single lead. Flagged means
	// the actual cost cleared the reserved amount by more than the configured
	// factor, which is not estimate error — it is the sub-budget failing to
	// bind. Continuing would repeat it on every remaining lead, so stop.
	if settled.Flagged {
		e.logger().ErrorContext(ctx, "lead overshot its reservation past the allowed factor",
			"lead", lead.ID, "reserved", settled.Reserved, "charged", settled.Charged)
		out := leadOutcome{err: fmt.Errorf(
			"%w: lead charged %d against a %d reservation", budget.ErrOvershoot, settled.Charged, settled.Reserved),
			class: Fatal}
		if result != nil {
			out.claims = result.Claims
		}
		return out
	}

	out := leadOutcome{}
	if result != nil {
		out.claims = result.Claims
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
	e.emit("replanning", fmt.Sprintf("%d open sub-question(s)", len(digest.Open())))
	plan, perr := e.planWithBudget(ctx, sess, func() (*planner.Plan, error) {
		return e.Planner.Replan(ctx, sess, digest, depth)
	})
	if perr != nil {
		return 0, false, perr
	}
	res.Replans++

	for _, id := range plan.Answered {
		digest.MarkAnswered(id)
	}
	if plan.Done {
		return 0, true, nil
	}

	assigned := digest.AddQuestions(plan.Questions)
	for i := range plan.Leads {
		if plan.Leads[i].ID == "" {
			plan.Leads[i].ID = core.NewLeadID()
		}
		if i < len(assigned) {
			leadQuestion[plan.Leads[i].ID] = assigned[i].ID
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

// planWithBudget reserves, runs a planning call, and settles it.
//
// Reserve BEFORE the call, which is §8.2's order and was inverted here: the call
// happened first and a reservation of exactly 1 was taken afterwards purely to
// have something to settle against. Two consequences, both measured. Planner
// spend was never gated — a 5000-token decomposition ran against a 1000-token
// budget — and when the after-the-fact reserve was refused because a ceiling had
// just fired, the cost was logged and dropped: 920 tokens spent, 460 recorded.
//
// Planner cost is charged to the planner role, so §14.3's breakdown can show
// what planning cost — the number that says whether the digest is doing its job.
func (e *Executor) planWithBudget(ctx context.Context, sess *core.Session, call func() (*planner.Plan, error)) (*planner.Plan, error) {
	est := budget.PlannerSeed(sess.BudgetUnit)
	if avail := sess.Available(); est > avail {
		est = avail
	}
	if est <= 0 {
		return nil, fmt.Errorf("%w: nothing left to plan with", budget.ErrInsufficientBudget)
	}

	reservation, err := e.Ledger.Reserve(ctx, sess.ID, est)
	if err != nil {
		return nil, err
	}

	plan, callErr := call()

	// Settle unconditionally: the tokens were spent whether or not the plan
	// parsed, and a reservation left held is budget neither spent nor available.
	var calls []core.ToolCall
	if plan != nil && !plan.Usage.IsZero() {
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
		calls = append(calls, tc)
	}
	if _, err := e.Ledger.Settle(context.WithoutCancel(ctx), reservation, calls); err != nil {
		e.logger().ErrorContext(ctx, "settling a planner call failed; accounting is now unreliable", "err", err)
	}
	return plan, callErr
}

// heartbeat renews a lease in the background until the returned function is
// called.
//
// Renews at a third of the TTL, so two consecutive failures still leave time to
// recover before the lease expires.
func (e *Executor) heartbeat(ctx context.Context, lease *queue.Lease) func() {
	interval := e.Queue.TTL() / 3
	if interval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// A lost lease is not recoverable from here: another worker may
				// hold the lead. Log and stop renewing; the run's own checks
				// surface it.
				if ok, err := e.Queue.Renew(context.WithoutCancel(ctx), lease); err != nil || !ok {
					e.logger().WarnContext(ctx, "lease renewal failed mid-lead",
						"lead", lease.Lead.ID, "ok", ok, "err", err)
					return
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// subBudget derives the actor's per-lead ceiling from the reserved amount.
//
// In token mode the reservation IS a token count, so the mapping is exact. In
// USD mode it has to be converted, and the rate comes from the cheap tier —
// chunk mining is where an actor's input tokens actually go, and pricing the
// ceiling off the strong model would set it several times too low.
//
// inputShare leaves room for output tokens and for EstimateTokens running
// short. Overshooting is a hard failure; underspending is a smaller run.
func (e *Executor) subBudget(sess *core.Session, reserved int64) actors.Budget {
	const inputShare = 70 // percent of the reservation available for input

	tokens := reserved
	if sess.BudgetUnit == core.BudgetUSD {
		tokens = e.tokensForMicros(reserved)
	}
	tokens = tokens * inputShare / 100
	if tokens <= 0 {
		// Nothing usable to derive from — leave the configured budget alone
		// rather than pinning the actor to zero and guaranteeing an empty run.
		return actors.Budget{}
	}
	return actors.Budget{MaxInputTokens: tokens}
}

// tokensForMicros converts a USD amount into an input-token count.
func (e *Executor) tokensForMicros(micros int64) int64 {
	if e.CheapModel == "" {
		return 0
	}
	table := e.Pricing
	if table == nil {
		table = pricing.NewTable()
	}
	rate, ok := table.Lookup(e.CheapModel)
	if !ok || rate.Input <= 0 {
		// No price for this model, so USD cannot bound it — the CLI already
		// refuses --usd in that case (checkUSDIsEnforceable). Returning zero
		// leaves the configured ceiling in place rather than inventing a rate.
		return 0
	}
	// Rates are nano-dollars per token; micros are 1000 nano.
	return micros * pricing.NanoPerMicro / rate.Input
}

// complete terminalizes a lead, surviving cancellation.
//
// On the same context as the run, a cancelled session settled the charge (Settle
// uses WithoutCancel) and then failed to complete the lead — leaving a lead that
// ran and was paid for recorded as still leased, for a later sweep to requeue
// and re-run.
func (e *Executor) complete(ctx context.Context, lease *queue.Lease, status core.LeadStatus) {
	if err := e.Queue.Complete(context.WithoutCancel(ctx), lease, status); err != nil {
		e.logger().ErrorContext(ctx, "could not complete a lead; it may be re-run and re-charged",
			"lead", lease.Lead.ID, "status", status, "err", err)
	}
}

// countLead increments the session's dispatched-lead counter, which is what
// makes MaxLeads enforceable.
func (e *Executor) countLead(ctx context.Context, sessionID string) error {
	return e.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.ApplyBudgetDelta(ctx, sessionID, store.BudgetDelta{LeadCount: 1})
	})
}

func (e *Executor) estimate(sess *core.Session, lead core.Lead) int64 {
	if e.Estimator == nil {
		e.Estimator = budget.NewEstimator(sess.BudgetUnit)
	}
	return e.Estimator.For(lead.ActorType, lead.Depth)
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

func truncateQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) <= 52 {
		return q
	}
	return q[:51] + "…"
}

func leadNote(o leadOutcome) string {
	if o.err == nil {
		return ""
	}
	return ", " + o.class.String() + ": " + truncateQuery(o.err.Error())
}

func statusForContext(err error) core.SessionStatus {
	if errors.Is(err, context.DeadlineExceeded) {
		return core.StatusExhausted
	}
	return core.StatusCancelled
}
