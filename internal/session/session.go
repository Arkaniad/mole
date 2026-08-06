// Package session runs one research session end to end.
//
// Everything from creating the session row to finalizing its status: boot
// recovery, the planner–executor–verifier loop, §11.5.2's grounding pass, and
// the report paid from released escrow (§8.3).
//
// It exists because two callers need that sequence and only one of them has a
// terminal. `mole research` had all of it inline, interleaved with flag handling
// and fmt.Printf, which made the daemon's options "duplicate 240 lines" or
// "import a CLI". Nothing here prints, opens a database, reads config, or knows
// what a flag is — callers inject what they built and receive what happened.
//
// The split into Recover / Create / Run is not cosmetic. Recovery is per
// PROCESS, so a daemon sweeps once at boot rather than once per session. And a
// daemon must return a session id to its caller before the work finishes, so
// creating the row and running the loop cannot be one call.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/cache"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/output"
	"github.com/lajosdeme/mole/internal/planner"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/queue"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/verifier"
)

// Spec is one research request: what to ask and what it may spend.
//
// Separate from Runner because a Runner's collaborators outlive any single
// session, while these values arrive with the request.
type Spec struct {
	Question   string
	Mode       core.Mode
	BudgetUnit core.BudgetUnit
	Budget     int64

	// MaxSources is sources read per lead; MaxDepth is rounds of follow-up leads
	// the planner may add; MaxLeads bounds the whole session (§8.5).
	MaxSources int
	MaxDepth   int
	MaxLeads   int

	// Timeout is the session's wall-clock ceiling (§8.5). The caller owns the
	// context deadline; this is the ceiling the loop checks itself against, and
	// the two should not be equal — see Runner.Run.
	Timeout time.Duration
}

// Result is what happened, in the shape a caller needs to render or return.
type Result struct {
	Session *core.Session

	// Run is the loop's own result. Nil when the loop never started.
	Run *executor.Result
	// Report is the synthesized answer. Nil when generation failed — never fatal,
	// since a failed synthesis costs the prose and not the evidence.
	Report *output.Report
	// Ground is §11.5.2's pass. Nil when it did not run.
	Ground *verifier.GroundReport

	// Status is what the session was finalized as.
	Status core.SessionStatus
	// Err is the loop's error. Returned in the struct rather than as the
	// function's error because a failed loop still has a session, a spend, and
	// usually claims — a caller that gets only an error has to go and find them.
	Err error
}

// Runner holds the collaborators a session needs.
//
// The store and actor are built by the caller: the CLI from flags and config,
// the daemon from its own configuration. Neither is constructed here, because
// choosing a search provider or a database path is not this package's decision.
type Runner struct {
	Store store.Store
	Actor *actors.WebActor

	// VerifierModel overrides the cheap model for adjudication. Empty leaves the
	// cheap model in place; set it when the cheap model cannot tell a
	// contradiction from two unrelated statements.
	VerifierModel string
	// VerifierBatchSize caps pairs per adjudication call. Zero takes the default.
	VerifierBatchSize int

	// Owner names what is running the loop, for lead leases (§9.4). A daemon and
	// a CLI must not claim each other's leads.
	Owner string

	Log *slog.Logger

	// Progress receives executor events as they happen.
	Progress func(executor.Event)
	// OnLoop fires when the research loop finishes, before any escrow is released.
	// OnGround fires when the grounding pass finishes, before the report is
	// generated.
	//
	// Callbacks rather than fields on Result because the sequence is observable
	// and Result is not: the CLI prints the run summary, then grounding, then the
	// report, in that order. Returning all three at the end collapses that into
	// one moment and the caller has to guess the order — which is exactly what
	// went wrong when this package was first extracted, and the grounding line
	// started printing ahead of the summary it follows.
	//
	// A daemon uses the same two points to publish status transitions without
	// waiting for the report.
	OnLoop   func(*executor.Result)
	OnGround func(*verifier.GroundReport)
	// Notice receives non-fatal problems: a sweep that failed, a hold that could
	// not be released, a report that would not generate. None of these stops a
	// run, and all of them need saying. The CLI writes them to stderr; a daemon
	// logs them.
	Notice func(string)
}

func (r *Runner) notice(format string, args ...any) {
	if r.Notice != nil {
		r.Notice(fmt.Sprintf(format, args...))
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	if r.Actor != nil && r.Actor.Log != nil {
		return r.Actor.Log
	}
	return slog.Default()
}

func (r *Runner) ledger() *budget.Ledger {
	return budget.New(r.Store, budget.DefaultConfig())
}

// Recovery reports what boot recovery reclaimed.
type Recovery struct {
	Leads        int
	Reservations int
	Sessions     int
}

// Any reports whether anything was recovered.
func (rc Recovery) Any() bool { return rc.Leads+rc.Reservations+rc.Sessions > 0 }

// Recover reclaims what a previous process left behind (§9.4).
//
// Per PROCESS, not per session — which is why it is not inside Run. It is
// deliberately unscoped: boot recovery does not know which sessions were in
// flight, and this is the one caller for which that is correct. A daemon calls
// it once at startup; running it per session would sweep leases belonging to
// sessions currently running alongside.
//
// Never returns an error. Every sweep is best-effort recovery of someone else's
// mess, and refusing to start because a sweep failed helps nobody.
func (r *Runner) Recover(ctx context.Context) Recovery {
	var rc Recovery
	led := r.ledger()

	if n, err := queue.New(r.Store, 0).Sweep(ctx, ""); err != nil {
		r.notice("lease sweep failed: %v", err)
	} else {
		rc.Leads = n
	}
	// A stale hold is budget neither spent nor available. SweepExpired had no
	// caller at all for a while, so a crash mid-lead understated a session's
	// Available() permanently.
	if n, err := led.SweepExpired(ctx); err != nil {
		r.notice("reservation sweep failed: %v", err)
	} else {
		rc.Reservations = n
	}
	if n, err := led.SweepAbandonedSessions(ctx); err != nil {
		r.notice("session sweep failed: %v", err)
	} else {
		rc.Sessions = n
	}
	return rc
}

// Create writes the session row and returns it.
//
// Separate from Run so a daemon can hand a session id back to its caller before
// any research happens. Nothing is spent here beyond the escrow reserved at
// creation (§8.3).
func (r *Runner) Create(ctx context.Context, spec Spec) (*core.Session, error) {
	if spec.Question == "" {
		return nil, errors.New("session: no question given")
	}
	if !spec.Mode.Valid() {
		return nil, fmt.Errorf("session: unknown mode %q", spec.Mode)
	}

	maxLeads := spec.MaxLeads
	if maxLeads <= 0 {
		maxLeads = 1
	}
	sources := spec.MaxSources
	if sources <= 0 {
		sources = 1
	}

	return r.ledger().CreateSession(ctx, budget.SessionSpec{
		Prompt:     spec.Question,
		Mode:       spec.Mode,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: spec.BudgetUnit,
		Budget:     spec.Budget,
		// Unit-independent ceilings (§8.5). They bind even when the money estimate
		// is wrong, which is the case they exist for. Sized from what the planner
		// can actually produce: an initial fan-out plus one round of follow-ups per
		// depth level, with headroom.
		MaxToolCalls: int64(maxLeads) * int64(sources) * 4,
		MaxLeads:     int64(maxLeads),
		MaxWallClock: spec.Timeout,
	})
}

// Run drives the loop for an already-created session, then writes the report and
// finalizes the status.
//
// The caller owns the context. Give it headroom over spec.Timeout: when the
// context deadline and the wall-clock ceiling fire together the loop is killed
// mid-lead instead of stopping at its own check, and the run ends in a cascade
// of "context deadline exceeded" from whatever was in flight — a persist, a
// settle, a count — rather than a clean "stopped: max_wallclock".
//
// A loop error is returned inside Result, not as the error, so a caller still
// gets the session, the spend and the claims. The error return is for failures
// that leave nothing worth reporting.
func (r *Runner) Run(ctx context.Context, sess *core.Session, spec Spec) (*Result, error) {
	if sess == nil {
		return nil, errors.New("session: nil session")
	}
	if r.Store == nil || r.Actor == nil {
		return nil, errors.New("session: runner needs a store and an actor")
	}

	res := &Result{Session: sess, Status: core.StatusDone}
	led := r.ledger()

	r.Actor.SessionID = sess.ID
	r.Actor.Store = r.Store

	// One cache shared between the loop and the actor, so a lead-level hit and a
	// URL-level hit are the same cache and one lead's fetches serve another's.
	shared := cache.New()
	r.Actor.Cache = shared

	// The Verifier shares the actor's provider, store and fetcher. §11.1 wants it
	// over the session's whole claim set, which is why it reads the store rather
	// than being handed each lead's batch — and the fetcher is the actor's own, so
	// §11.5's re-read goes through the same robots handling, rate limiter and SSRF
	// guard as the fetch that produced the claim.
	vf := &verifier.Verifier{
		Store:     r.Store,
		Ledger:    led,
		LLM:       r.Actor.LLM,
		Log:       r.logger(),
		Model:     r.VerifierModel,
		BatchSize: r.VerifierBatchSize,
		Grounder:  &verifier.Grounder{Fetch: r.Actor.Fetch, Extract: r.Actor.Extract},
	}

	owner := r.Owner
	if owner == "" {
		owner = "session"
	}
	exec := &executor.Executor{
		Store:      r.Store,
		Ledger:     led,
		Queue:      queue.New(r.Store, 0),
		Cache:      shared,
		Pricing:    r.Actor.Pricing,
		CheapModel: r.Actor.LLM.ModelFor(llm.TierCheap),
		Planner:    &planner.Planner{LLM: r.Actor.LLM, MaxDepth: spec.MaxDepth},
		Verifier:   vf,
		Actors:     map[core.ActorType]actors.Actor{core.ActorWeb: r.Actor},
		Log:        r.logger(),
		Owner:      owner,
		Progress:   r.Progress,
	}

	res.Run, res.Err = exec.Run(ctx, sess.ID)
	if r.OnLoop != nil && res.Run != nil {
		r.OnLoop(res.Run)
	}

	// The report is paid from escrow (§8.3). Releasing it here is what makes the
	// money set aside at session creation spendable — a run that produced good
	// claims and could not afford to write them up would have wasted the whole
	// budget, not just the last call.
	res.Report, res.Ground = r.finish(ctx, led, vf, sess)

	if res.Run != nil {
		res.Status = res.Run.Status
	}
	if res.Err != nil && res.Status == core.StatusDone {
		res.Status = core.StatusFailed
	}
	if err := led.Finish(context.WithoutCancel(ctx), sess.ID, res.Status); err != nil {
		r.notice("could not finalize session: %v", err)
	}
	return res, nil
}

// finish releases escrow, grounds, and writes the report.
//
// Runs on a context detached from the caller's. Everything here is the payoff
// for money already spent, and a cancelled research context must not also
// discard the answer it paid for.
func (r *Runner) finish(
	ctx context.Context,
	led *budget.Ledger,
	vf *verifier.Verifier,
	sess *core.Session,
) (*output.Report, *verifier.GroundReport) {
	// Whether the run was stopped, read BEFORE detaching. Everything below runs
	// on a live context by design — the payoff for money already spent must not
	// be discarded because research was cancelled — which also means ctx.Err()
	// stops being readable one line down.
	cancelled := ctx.Err() != nil
	ctx = context.WithoutCancel(ctx)

	released, err := led.ReleaseEscrow(ctx, sess.ID)
	if err != nil {
		r.notice("could not release escrow: %v", err)
	}

	// §11.5.2's grounding pass, between the escrow release and the report.
	//
	// Here rather than inside the loop for two reasons. Its candidates are the
	// claims the REPORT will lean on, which is only knowable once research has
	// stopped and the graph is complete. And §11.5 puts its spend on the escrow —
	// exactly the money just released, so it takes a bounded share and leaves the
	// rest for the answer. A grounding pass that spent the escrow would produce a
	// well-checked set of claims and no report to put them in.
	var ground *verifier.GroundReport
	if !cancelled {
		ground = r.ground(ctx, vf, sess.ID, released)
	}
	if r.OnGround != nil && ground != nil {
		r.OnGround(ground)
	}

	// Reserve BEFORE generating. The order used to be release → generate →
	// reserve, which inverts §8.2 and had a concrete failure: after any research
	// overshoot the reserve was refused, and the report tokens — already spent —
	// were never written to the ledger. §8.1 says every tool call writes a cost
	// row, and Verify() still reconciled because the row never existed.
	//
	// Bounded by what is actually available, not by what escrow nominally
	// released: an overshoot may already have eaten into it.
	amount := released
	if ground != nil {
		amount -= ground.Spent
	}
	if cur, err := r.loadSession(ctx, sess.ID); err == nil {
		if avail := cur.Available(); amount > avail {
			amount = avail
		}
	}

	gen := &output.Generator{LLM: r.Actor.LLM}
	var reservation *core.Reservation
	if cancelled {
		// Cancel means stop spending. Escrow is still released and every hold
		// still settles — leaving budget held would be the worst of both — but no
		// model call is made, so the session emits the evidence it already paid
		// to collect and no prose. Reuses the same path as "nothing to spend".
		amount = 0
		r.notice("session cancelled: writing the evidence without a synthesized answer")
	}
	if amount > 0 {
		reservation, err = led.ReserveOutput(ctx, sess.ID, amount)
		if err != nil {
			r.notice("could not reserve for the report: %v", err)
		}
	}
	if reservation == nil {
		// Nothing to spend. Emit the evidence without prose rather than making a
		// call whose cost cannot be recorded.
		gen.LLM = nil
	}

	report, err := gen.Generate(ctx, r.Store, sess.ID)
	if err != nil {
		r.notice("report generation failed: %v", err)
		if reservation != nil {
			if rerr := led.Release(ctx, reservation); rerr != nil {
				r.notice("could not release the report hold: %v", rerr)
			}
		}
		return nil, ground
	}

	if reservation != nil {
		// Settle unconditionally, including a zero cost: an unresolved hold is
		// budget neither spent nor available.
		var calls []core.ToolCall
		if !report.Cost.IsZero() {
			calls = append(calls, core.ToolCall{
				SessionID: sess.ID,
				Role:      core.RoleOutput,
				Type:      core.CallLLM,
				Model:     report.Model,
				Input:     "report",
				Cost:      r.priceReport(report),
			})
		}
		if _, serr := led.Settle(ctx, reservation, calls); serr != nil {
			r.notice("settling the report failed: %v", serr)
		}
	}
	return report, ground
}

// ground runs §11.5.2's re-read within a bounded share of the released escrow.
func (r *Runner) ground(
	ctx context.Context,
	vf *verifier.Verifier,
	sessionID string,
	releasedEscrow int64,
) *verifier.GroundReport {
	if vf == nil || vf.Grounder == nil || releasedEscrow <= 0 {
		return nil
	}
	allowance := int64(float64(releasedEscrow) * verifier.DefaultGroundShareOfEscrow)
	if allowance <= 0 {
		return nil
	}
	rep, err := vf.Ground(ctx, sessionID, allowance)
	if err != nil {
		r.notice("grounding pass failed: %v", err)
		return nil
	}
	return rep
}

// priceReport turns the generator's token usage into a ledger cost.
func (r *Runner) priceReport(rep *output.Report) core.Cost {
	table := r.Actor.Pricing
	if table == nil {
		table = pricing.NewTable()
	}
	cost, err := table.Cost(rep.Model, pricing.Usage{
		InputTokens:  rep.Cost.InputTokens,
		OutputTokens: rep.Cost.OutputTokens,
	})
	if err != nil {
		// Unknown model: record the tokens, price them at zero. A row with real
		// token counts and no money still reconciles; a missing row does not.
		return rep.Cost
	}
	return cost
}

func (r *Runner) loadSession(ctx context.Context, id string) (*core.Session, error) {
	var sess *core.Session
	err := r.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		sess, err = q.GetSession(ctx, id)
		return err
	})
	return sess, err
}
