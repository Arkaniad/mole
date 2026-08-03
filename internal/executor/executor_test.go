package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/cache"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/planner"
	"github.com/lajosdeme/mole/internal/queue"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type scriptedLLM struct {
	mu       sync.Mutex
	replies  []string
	calls    int
	errAfter int // when >0, calls at or past this index fail
}

func (s *scriptedLLM) Name() string               { return "fake" }
func (s *scriptedLLM) ModelFor(t llm.Tier) string { return "fake-model" }
func (s *scriptedLLM) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	resp := &llm.Response{
		Model: "fake-model",
		Usage: llm.Usage{InputTokens: 400, OutputTokens: 60},
	}
	if s.errAfter > 0 && i >= s.errAfter {
		return resp, llm.ErrRateLimited
	}
	resp.Text = `{"done":true}`
	if i < len(s.replies) {
		resp.Text = s.replies[i]
	}
	return resp, nil
}

func planJSON(questions ...string) string {
	var qs []map[string]string
	for i, q := range questions {
		qs = append(qs, map[string]string{"id": fmt.Sprintf("q%d", i+1), "question": q})
	}
	b, _ := json.Marshal(map[string]any{"questions": qs})
	return string(b)
}

// scriptedActor returns a scripted outcome per lead, in order.
type scriptedActor struct {
	mu      sync.Mutex
	runs    int
	outcome func(n int, lead core.Lead) (*actors.Result, error)
}

func (a *scriptedActor) Type() core.ActorType { return core.ActorWeb }
func (a *scriptedActor) Run(ctx context.Context, lead core.Lead) (*actors.Result, error) {
	a.mu.Lock()
	n := a.runs
	a.runs++
	a.mu.Unlock()
	return a.outcome(n, lead)
}

func (a *scriptedActor) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs
}

func okResult(claims int, cost int64) *actors.Result {
	r := &actors.Result{
		Summary: "a summary",
		Costs: []core.ToolCall{{
			Role: core.RoleExecutor, Type: core.CallLLM, Model: "fake-model",
			Cost: core.Cost{USDMicros: cost, InputTokens: 1000, OutputTokens: 100},
		}},
	}
	for i := 0; i < claims; i++ {
		r.Claims = append(r.Claims, core.Claim{
			Text:   fmt.Sprintf("claim %d", i),
			Source: "https://example.com/a",
			Quote:  "a quote long enough to constitute real evidence",
		})
	}
	return r
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type rig struct {
	db    *sqlite.DB
	led   *budget.Ledger
	q     *queue.Queue
	exec  *executor.Executor
	sess  *core.Session
	actor *scriptedActor
	llm   *scriptedLLM
}

func newRig(t *testing.T, budgetAmount int64, replies []string, outcome func(int, core.Lead) (*actors.Result, error)) *rig {
	return newRigWithCeilings(t, budgetAmount, 50, 200, replies, outcome)
}

func newRigWithCeilings(t *testing.T, budgetAmount int64, maxLeads, maxCalls int64, replies []string, outcome func(int, core.Lead) (*actors.Result, error)) *rig {
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

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "the research question", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: budgetAmount,
		MaxLeads: maxLeads, MaxToolCalls: maxCalls,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	fl := &scriptedLLM{replies: replies}
	act := &scriptedActor{outcome: outcome}
	q := queue.New(db, time.Minute)

	return &rig{
		db: db, led: led, q: q, sess: sess, actor: act, llm: fl,
		exec: &executor.Executor{
			Store:   db,
			Ledger:  led,
			Queue:   q,
			Planner: &planner.Planner{LLM: fl, MaxInitialLeads: 3, ReplanEvery: 3, MaxDepth: 2},
			Actors:  map[core.ActorType]actors.Actor{core.ActorWeb: act},
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			Jitter:  func() float64 { return 0 },
			Sleep:   func(context.Context, time.Duration) error { return nil },
		},
	}
}

func (r *rig) reload(t *testing.T) *core.Session {
	t.Helper()
	var s *core.Session
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------------------
// The invariant: every reservation resolved, on every path
// ---------------------------------------------------------------------------

// TestNothingIsLeftHeldOnAnyPath is the invariant the whole loop is arranged
// around. A settle skipped because a lead failed leaves budget held forever —
// neither spent nor available — and the session runs out of money it never
// used. Failure paths outnumber the success path, so this is checked across all
// of them.
func TestNothingIsLeftHeldOnAnyPath(t *testing.T) {
	cases := map[string]func(int, core.Lead) (*actors.Result, error){
		"all succeed": func(int, core.Lead) (*actors.Result, error) {
			return okResult(2, 20_000), nil
		},
		"all fail with no result": func(int, core.Lead) (*actors.Result, error) {
			return nil, errors.New("dead link")
		},
		"fail but report partial cost": func(int, core.Lead) (*actors.Result, error) {
			return okResult(1, 15_000), errors.New("chunk mining failed")
		},
		"transient then success": func(n int, _ core.Lead) (*actors.Result, error) {
			if n%2 == 0 {
				return okResult(0, 5_000), llm.ErrRateLimited
			}
			return okResult(2, 20_000), nil
		},
		"always transient": func(int, core.Lead) (*actors.Result, error) {
			return okResult(0, 5_000), llm.ErrOverloaded
		},
		"empty results": func(int, core.Lead) (*actors.Result, error) {
			return okResult(0, 10_000), nil
		},
	}

	for name, outcome := range cases {
		r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("a", "b", "c")}, outcome)
		if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
			t.Fatalf("%s: run: %v", name, err)
		}

		after := r.reload(t)
		if after.Held != 0 {
			t.Errorf("%s: %d still held after the session", name, after.Held)
		}

		v, err := r.led.Verify(context.Background(), r.sess.ID)
		if err != nil {
			t.Fatalf("%s: verify: %v", name, err)
		}
		if !v.Consistent() {
			t.Errorf("%s: ledger does not reconcile: spent %d/%d held %d/%d",
				name, v.SpentRecorded, v.SpentFromLedger, v.HeldRecorded, v.HeldFromRows)
		}
	}
}

// TestBudgetIsNeverOvershot across a whole session, which is §14.3's headline
// contract and the reason reserve-before-dispatch exists.
func TestBudgetIsNeverOvershot(t *testing.T) {
	// Each lead costs far more than the estimator would guess.
	r := newRig(t, core.MicrosPerUSD, []string{planJSON("a", "b", "c")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 300_000), nil })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	after := r.reload(t)
	if after.Spent > after.Budget {
		t.Errorf("spent %d against a %d budget", after.Spent, after.Budget)
	}
}

// TestEscrowSurvivesTheLoop. §8.3 holds escrow back for the report; a loop that
// spent it would leave nothing to generate output with, which is the failure
// escrow exists to prevent.
func TestEscrowSurvivesTheLoop(t *testing.T) {
	r := newRig(t, 2*core.MicrosPerUSD, []string{planJSON("a", "b", "c")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 400_000), nil })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	after := r.reload(t)
	if after.Escrow <= 0 {
		t.Fatal("no escrow was held at all")
	}
	if after.Spent > after.Budget-after.Escrow {
		t.Errorf("spent %d, which eats into the %d escrow", after.Spent, after.Escrow)
	}
}

// ---------------------------------------------------------------------------
// §9.5 error policy
// ---------------------------------------------------------------------------

// TestTransientErrorsAreRetriedThenDegraded, and the session survives.
func TestTransientErrorsAreRetriedThenDegraded(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("only one")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(0, 1_000), llm.ErrRateLimited })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.actor.count(); got != executor.MaxAttempts {
		t.Errorf("actor ran %d times, want %d attempts", got, executor.MaxAttempts)
	}
	if res.Status == core.StatusFailed {
		t.Error("a transient failure aborted the session; §9.5 says degrade")
	}
	if res.LeadsFailed == 0 {
		t.Error("the exhausted lead was not counted as failed")
	}
}

// TestDegradedErrorsAreNotRetried: a dead link fails identically next time, and
// a retry re-runs the whole lead and is charged again.
func TestDegradedErrorsAreNotRetried(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("only one")},
		func(int, core.Lead) (*actors.Result, error) { return nil, errors.New("extraction failed") })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.actor.count(); got != 1 {
		t.Errorf("a degraded error was retried %d times", got)
	}
}

// TestFatalErrorsAbortTheSession. Continuing spends budget on calls that cannot
// succeed — a bad key fails on every lead — and the escrow is preserved so a
// partial report is still affordable (§9.5).
func TestFatalErrorsAbortTheSession(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("a", "b", "c")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(0, 1_000), llm.ErrUnauthorized })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", res.Status)
	}
	if r.actor.count() != 1 {
		t.Errorf("ran %d leads after a fatal error; it fails identically every time", r.actor.count())
	}
	if after := r.reload(t); after.Escrow <= 0 {
		t.Error("escrow was consumed, so no partial report is affordable")
	}
}

// TestPartialClaimsSurviveAFailedLead. The actor verified every quote it
// returned; discarding them because a later chunk failed throws away evidence
// that was paid for.
func TestPartialClaimsSurviveAFailedLead(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("only one")},
		func(int, core.Lead) (*actors.Result, error) {
			return okResult(3, 10_000), errors.New("the last chunk failed")
		})

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Claims) != 3 {
		t.Errorf("%d claims kept from a partially failed lead, want 3", len(res.Claims))
	}
}

// ---------------------------------------------------------------------------
// Planning behaviour
// ---------------------------------------------------------------------------

// TestPlannerCostIsBoundedByReplansNotLeads is §9.1's whole point. Rev 1 called
// the planner after every lead; if that regressed, planner calls would track
// lead count.
func TestPlannerCostIsBoundedByReplansNotLeads(t *testing.T) {
	replies := []string{
		planJSON("a", "b", "c"), // initial
		planJSON("d", "e", "f"), // replan 1
		planJSON("g", "h", "i"), // replan 2
		`{"done":true}`,         // replan 3 stops
	}
	r := newRig(t, 20*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(2, 10_000), nil })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.LeadsRun < 6 {
		t.Fatalf("only %d leads ran; the test needs several to be meaningful", res.LeadsRun)
	}
	// ReplanEvery is 3, so planner calls must be far below lead count.
	if r.llm.calls > res.LeadsRun {
		t.Errorf("%d planner calls for %d leads — planning is not batched",
			r.llm.calls, res.LeadsRun)
	}
}

// TestSessionStopsWhenThePlannerSaysDone rather than looping until the budget
// runs out.
func TestSessionStopsWhenThePlannerSaysDone(t *testing.T) {
	r := newRig(t, 50*core.MicrosPerUSD, []string{planJSON("a"), `{"done":true}`},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusDone {
		t.Errorf("status = %s, want done", res.Status)
	}

	after := r.reload(t)
	if after.Spent > after.Budget/2 {
		t.Errorf("spent %d of %d after being told to stop", after.Spent, after.Budget)
	}
}

// TestDigestRecordsCoverage so the planner can see which questions produced
// evidence and which did not.
func TestDigestRecordsCoverage(t *testing.T) {
	r := newRig(t, 20*core.MicrosPerUSD, []string{planJSON("productive", "barren"), `{"done":true}`},
		func(n int, lead core.Lead) (*actors.Result, error) {
			if lead.Query == "barren" {
				return okResult(0, 5_000), nil
			}
			return okResult(4, 10_000), nil
		})

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Digest.String()
	if !strings.Contains(out, "4 claim(s) found") {
		t.Errorf("productive question shows no yield:\n%s", out)
	}
	if !strings.Contains(out, "no_evidence") {
		t.Errorf("the barren question was not recorded as a dead end:\n%s", out)
	}
}

// TestPlannerCostIsChargedToThePlannerRole, so §14.3's role breakdown can show
// what planning cost — the number that says whether the digest is working.
func TestPlannerCostIsChargedToThePlannerRole(t *testing.T) {
	r := newRig(t, 20*core.MicrosPerUSD, []string{planJSON("a"), `{"done":true}`},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 10_000), nil })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	var byRole map[core.Role]core.Cost
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		byRole, err = q.SumCostsByRole(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if byRole[core.RolePlanner].TotalTokens() == 0 {
		t.Errorf("no planner cost recorded; role breakdown = %+v", byRole)
	}
	if byRole[core.RoleExecutor].TotalTokens() == 0 {
		t.Error("no executor cost recorded")
	}
}

// ---------------------------------------------------------------------------
// Ceilings
// ---------------------------------------------------------------------------

// TestMaxLeadsCeilingStopsTheLoop. Unit-independent ceilings (§8.5) bind even
// when the cost estimate is wrong, which is the case they exist for: a budget
// large enough to run forever must still stop.
func TestMaxLeadsCeilingStopsTheLoop(t *testing.T) {
	// Budget for hundreds of leads, a ceiling of 2, and a planner that keeps
	// proposing more.
	replies := []string{
		planJSON("a", "b", "c"), planJSON("d", "e", "f"),
		planJSON("g", "h", "i"), planJSON("j", "k", "l"),
	}
	r := newRigWithCeilings(t, 1000*core.MicrosPerUSD, 2, 500, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusExhausted {
		t.Errorf("status = %s, want budget_exhausted (the ceiling stopped it)", res.Status)
	}
	if res.StoppedBecause != "max_leads" {
		t.Errorf("stopped because %q, want max_leads", res.StoppedBecause)
	}

	after := r.reload(t)
	if after.LeadCount > after.MaxLeads {
		t.Errorf("ran %d leads against a ceiling of %d", after.LeadCount, after.MaxLeads)
	}
	if after.Spent > after.Budget/10 {
		t.Errorf("spent %d of %d — the ceiling did not stop the spend", after.Spent, after.Budget)
	}
}

// TestCancellationStopsCleanly and still leaves the ledger consistent.
func TestCancellationStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	r := newRig(t, 20*core.MicrosPerUSD, []string{planJSON("a", "b", "c")},
		func(n int, _ core.Lead) (*actors.Result, error) {
			if n == 1 {
				cancel()
			}
			return okResult(1, 5_000), nil
		})

	res, err := r.exec.Run(ctx, r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusCancelled {
		t.Errorf("status = %s, want cancelled", res.Status)
	}

	v, err := r.led.Verify(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Consistent() {
		t.Error("the ledger does not reconcile after cancellation")
	}
	if after := r.reload(t); after.Held != 0 {
		t.Errorf("%d held after cancellation", after.Held)
	}
}

// TestNoActorRegisteredIsFatal rather than an infinite loop of leads nothing
// can run.
func TestNoActorRegisteredIsFatal(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("a")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })
	r.exec.Actors = map[core.ActorType]actors.Actor{} // none

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", res.Status)
	}
}

// TestTokenModeChargesTokensNotDollars. The two units are not interchangeable:
// in token mode a search call priced only in dollars contributes nothing, which
// is a real property of the mode (§8.5) and not a bug — but the model tokens
// must land, or the ceiling is enforced against zero.
//
// Ported from the single-lead CLI test the executor replaced.
func TestTokenModeChargesTokensNotDollars(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: 200_000,
		MaxLeads: 50, MaxToolCalls: 200,
	})
	if err != nil {
		t.Fatal(err)
	}

	fl := &scriptedLLM{replies: []string{planJSON("a"), `{"done":true}`}}
	act := &scriptedActor{outcome: func(int, core.Lead) (*actors.Result, error) {
		return &actors.Result{
			Costs: []core.ToolCall{
				// Priced in dollars only: contributes nothing in token mode.
				{Role: core.RoleExecutor, Type: core.CallSearch, Cost: core.Cost{USDMicros: 5_000}},
				{Role: core.RoleExecutor, Type: core.CallLLM,
					Cost: core.Cost{USDMicros: 41_000, InputTokens: 12_000, OutputTokens: 900}},
			},
		}, nil
	}}

	e := &executor.Executor{
		Store: db, Ledger: led, Queue: queue.New(db, time.Minute),
		Planner: &planner.Planner{LLM: fl, MaxInitialLeads: 1, MaxDepth: 1},
		Actors:  map[core.ActorType]actors.Actor{core.ActorWeb: act},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:   func(context.Context, time.Duration) error { return nil },
	}
	if _, err := e.Run(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	var after *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		after, err = q.GetSession(ctx, sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// 12900 from the actor's model call, plus the planner's own tokens. The
	// dollar-only search call must contribute none of it.
	if after.Spent < 12_900 {
		t.Errorf("spent %d tokens, want at least the 12900 the model reported", after.Spent)
	}
	if after.Spent > 100_000 {
		t.Errorf("spent %d tokens — dollar amounts leaked into a token budget", after.Spent)
	}
}

// TestReservationIsClampedToWhatRemains. A budget smaller than the estimator's
// seed should buy a small run, not an error. Escrow is already held back, so
// the clamp has to respect Available rather than Budget.
//
// Ported from the single-lead CLI test the executor replaced.
func TestReservationIsClampedToWhatRemains(t *testing.T) {
	// Far below the estimator seed for a web lead.
	r := newRig(t, 30_000, []string{planJSON("a"), `{"done":true}`},
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("a small budget failed instead of buying a small run: %v", err)
	}
	if res.LeadsRun == 0 {
		t.Error("no lead ran at all on a small budget")
	}

	after := r.reload(t)
	if after.Spent > after.Budget {
		t.Errorf("spent %d against a %d budget", after.Spent, after.Budget)
	}
	if after.Held != 0 {
		t.Errorf("%d held after a clamped run", after.Held)
	}
}

// TestCachedLeadReturnsAResultRatherThanSkipping is §9.3's correctness fix, and
// the reason the cache is result-returning rather than a skip list.
//
// Rev 1 did `if cache.SeenRecently(lead) { continue }`. The planner asked for
// something and got nothing back, so the next replan spawned an equivalent lead
// and the loop livelocked. Recording the coverage is what closes it: the planner
// learns the question was answered.
func TestCachedLeadReturnsAResultRatherThanSkipping(t *testing.T) {
	// A planner that keeps proposing the SAME question — exactly the livelock
	// shape. Without a result-returning cache this never converges.
	replies := []string{
		planJSON("the same question"),
		planJSON("the same question"),
		planJSON("the same question"),
		planJSON("the same question"),
		`{"done":true}`,
	}
	r := newRig(t, 50*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(3, 10_000), nil })
	r.exec.Cache = cache.New()

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The actor must run the question once, not once per proposal.
	if got := r.actor.count(); got != 1 {
		t.Errorf("the actor ran %d times for one repeated question, want 1", got)
	}
	if res.LeadsCached == 0 {
		t.Error("no lead was served from cache")
	}
	// The precise assertion: every cached hit must ADD its coverage to the
	// digest. A substring check on "claim(s) found" is vacuous — every open
	// question prints that line whether or not anything was found — so count
	// instead. One real run plus N cached hits at 3 claims each.
	//
	// This is what distinguishes a result-returning cache from a skip list. A
	// skip records nothing, so the total stays at the single real run's 3, the
	// planner never learns the question was answered, and it keeps proposing it.
	wantClaims := 3 * (1 + res.LeadsCached)
	if res.Digest.ClaimsFound != wantClaims {
		t.Errorf("digest recorded %d claims across %d cached lead(s) and 1 real one, want %d — "+
			"a cache hit is not reporting its coverage",
			res.Digest.ClaimsFound, res.LeadsCached, wantClaims)
	}
}

// TestCachedLeadsCostNothing. A hit that charged would make the cache a
// rounding error rather than a saving.
func TestCachedLeadsCostNothing(t *testing.T) {
	replies := []string{planJSON("repeat me"), planJSON("repeat me"), `{"done":true}`}
	r := newRig(t, 50*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(2, 100_000), nil })
	r.exec.Cache = cache.New()

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.LeadsCached == 0 {
		t.Fatal("nothing was cached; the test asserts nothing")
	}

	// One actor run at 100k plus planner calls. A second charged run would
	// double the executor's share.
	var byRole map[core.Role]core.Cost
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		byRole, err = q.SumCostsByRole(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := byRole[core.RoleExecutor].USDMicros; got != 100_000 {
		t.Errorf("executor spend = %d, want exactly one lead's 100000", got)
	}
}

// TestCachedLeadIsMarkedAsCached, so a trace can tell a cache hit from work.
func TestCachedLeadIsMarkedAsCached(t *testing.T) {
	replies := []string{planJSON("same"), planJSON("same"), `{"done":true}`}
	r := newRig(t, 50*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })
	r.exec.Cache = cache.New()

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := r.q.Stats(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Cached == 0 {
		t.Errorf("no lead recorded as skipped_cached: %+v", stats)
	}
}

// TestNoCacheStillWorks: the cache is optional, and a nil one must not change
// behaviour beyond costing more.
func TestNoCacheStillWorks(t *testing.T) {
	replies := []string{planJSON("same"), planJSON("same"), `{"done":true}`}
	r := newRig(t, 50*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(1, 1_000), nil })
	r.exec.Cache = nil

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.LeadsCached != 0 {
		t.Error("a nil cache reported hits")
	}
	if r.actor.count() < 2 {
		t.Error("without a cache the repeated question should be researched twice")
	}
}

// TestCachedDeadEndStaysADeadEnd. A question that found nothing must not read
// as answered on the second ask, or the planner stops probing a real gap.
func TestCachedDeadEndStaysADeadEnd(t *testing.T) {
	replies := []string{planJSON("barren"), planJSON("barren"), `{"done":true}`}
	r := newRig(t, 50*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(0, 1_000), nil })
	r.exec.Cache = cache.New()

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Count, not substring: the first uncached run already records one
	// no_evidence, so its mere presence proves nothing about the cached ones.
	var deadEnds int
	for _, de := range res.Digest.DeadEnds {
		if de.Cause == "no_evidence" {
			deadEnds = de.Count
		}
	}
	if want := 1 + res.LeadsCached; deadEnds != want {
		t.Errorf("no_evidence recorded %d times across %d cached lead(s) and 1 real one, want %d",
			deadEnds, res.LeadsCached, want)
	}
}

// TestOneLeadCannotSpendTheWholeBudget is the bound §8.2 claims and §14.3
// requires ("max overshoot must be ~0").
//
// Before the sub-budget existed, the reservation bounded nothing: the actor's
// ceiling came from config and bore no relation to the money held for it. A
// single lead costing five times the budget spent all of it — measured at 5.0x
// with the escrow consumed — while TestBudgetIsNeverOvershot passed because its
// parameters stopped short of the boundary.
//
// A fake actor cannot be made to honour a sub-budget it does not read, so this
// asserts the OTHER half: the settle-time factor check stops the session
// instead of letting the pattern repeat on every remaining lead.
func TestOneLeadCannotSpendTheWholeBudget(t *testing.T) {
	// $1.00 budget; a lead that ignores its ceiling and spends $5.00.
	r := newRig(t, core.MicrosPerUSD, []string{planJSON("a", "b", "c"), `{"done":true}`},
		func(int, core.Lead) (*actors.Result, error) {
			return okResult(1, 5*core.MicrosPerUSD), nil
		})

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The run must stop on the first offending lead rather than repeating it.
	if r.actor.count() != 1 {
		t.Errorf("the actor ran %d times after blowing its reservation, want 1", r.actor.count())
	}
	if res.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed — an unbounded overshoot is not a clean stop", res.Status)
	}
	if !strings.Contains(res.StoppedBecause, "overshoot") {
		t.Errorf("stopped because %q, want it to name the overshoot", res.StoppedBecause)
	}

	after := r.reload(t)
	if after.Held != 0 {
		t.Errorf("%d held after an overshoot", after.Held)
	}
	v, _ := r.led.Verify(context.Background(), r.sess.ID)
	if !v.Consistent() {
		t.Error("the ledger does not reconcile after an overshoot")
	}
}

// TestSubBudgetIsDerivedFromTheReservation. The actor has to be TOLD what it
// may spend; a ceiling from config cannot know what was reserved.
func TestSubBudgetIsDerivedFromTheReservation(t *testing.T) {
	var seen []actors.Budget

	r := newRig(t, 200_000, []string{planJSON("a"), `{"done":true}`}, nil)
	r.exec.Actors = map[core.ActorType]actors.Actor{
		core.ActorWeb: &budgetSpy{seen: &seen},
	}
	r.exec.CheapModel = "claude-haiku-4-5"

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("the actor never ran")
	}
	for i, b := range seen {
		if b.MaxInputTokens <= 0 {
			t.Errorf("lead %d ran with no input ceiling: %+v", i, b)
		}
	}
}

// TestTokenModeSubBudgetIsExact: in token mode the reservation IS a token
// count, so no conversion is involved and the ceiling must track it directly.
func TestTokenModeSubBudgetIsExact(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: 500_000,
		MaxLeads: 10, MaxToolCalls: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	var seen []actors.Budget
	fl := &scriptedLLM{replies: []string{planJSON("a"), `{"done":true}`}}
	e := &executor.Executor{
		Store: db, Ledger: led, Queue: queue.New(db, time.Minute),
		Planner: &planner.Planner{LLM: fl, MaxInitialLeads: 1, MaxDepth: 1},
		Actors:  map[core.ActorType]actors.Actor{core.ActorWeb: &budgetSpy{seen: &seen}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:   func(context.Context, time.Duration) error { return nil },
	}
	if _, err := e.Run(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("the actor never ran")
	}
	// The estimator seed for a web lead in token mode; the ceiling must be a
	// fraction of it, not unrelated to it.
	got := seen[0].MaxInputTokens
	if got <= 0 || got > 500_000 {
		t.Errorf("sub-budget = %d tokens against a 500000 budget — not derived from the reservation", got)
	}
}

// budgetSpy records the sub-budget it was handed.
type budgetSpy struct{ seen *[]actors.Budget }

func (b *budgetSpy) Type() core.ActorType { return core.ActorWeb }
func (b *budgetSpy) Run(ctx context.Context, lead core.Lead) (*actors.Result, error) {
	sub, _ := actors.SubBudgetFrom(ctx)
	*b.seen = append(*b.seen, sub)
	return okResult(1, 1_000), nil
}

// TestFinalReplanFailureDoesNotFailTheSession. §9.5 classifies 429/5xx/timeout
// as transient — never abort. This path used to return, so a complete run with
// claims collected reported as failed with a non-zero exit because its last
// planner call got throttled.
func TestFinalReplanFailureDoesNotFailTheSession(t *testing.T) {
	// One question, then the drain-time replan errors.
	fl := &scriptedLLM{replies: []string{planJSON("only one")}}
	fl.errAfter = 1

	r := newRig(t, 20*core.MicrosPerUSD, nil,
		func(int, core.Lead) (*actors.Result, error) { return okResult(3, 10_000), nil })
	r.exec.Planner = &planner.Planner{LLM: fl, MaxInitialLeads: 1, ReplanEvery: 99, MaxDepth: 2}

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("a throttled final replan returned an error: %v", err)
	}
	if res.Status == core.StatusFailed {
		t.Errorf("status = failed after collecting %d claims; §9.5 says degrade", len(res.Claims))
	}
	if len(res.Claims) != 3 {
		t.Errorf("%d claims survived, want 3", len(res.Claims))
	}
	// The spend read must still happen — the early return used to skip it.
	if res.Spent == 0 {
		t.Error("Spent is 0 on a run that charged for a lead and two planner calls")
	}
	if !strings.Contains(res.StoppedBecause, "replan") {
		t.Errorf("stopped because %q, want it to name the replan", res.StoppedBecause)
	}
}

// TestLeaseIsHeartbeatedWhileALeadRuns. §9.4's "workers heartbeat their lease".
// Renew used to be called only BETWEEN retries, so a lead that legitimately
// outlasted the TTL — a slow fetch plus a slow model call — lost its lease and
// became sweepable while still running.
//
// The obvious version of this test could not fail: nothing sweeps during a run,
// so an expired lease is simply never observed and deleting the heartbeat left
// it green. A concurrent sweeper is what makes the expiry visible, and it is
// also the real scenario — a second process starting up, or M5's periodic
// recovery.
func TestLeaseIsHeartbeatedWhileALeadRuns(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: 10 * core.MicrosPerUSD,
		MaxLeads: 5, MaxToolCalls: 50,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Short TTL, a lead that takes several TTLs, and a sweeper running
	// throughout — which is what turns a lost lease into an observable
	// re-dispatch.
	q := queue.New(db, 90*time.Millisecond)
	sweeper := queue.New(db, 90*time.Millisecond)

	stop := make(chan struct{})
	var swept atomic.Int64
	go func() {
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if n, err := sweeper.Sweep(context.Background(), sess.ID); err == nil {
					swept.Add(int64(n))
				}
			}
		}
	}()

	fl := &scriptedLLM{replies: []string{planJSON("slow"), `{"done":true}`}}
	slow := &scriptedActor{outcome: func(int, core.Lead) (*actors.Result, error) {
		time.Sleep(400 * time.Millisecond)
		return okResult(1, 1_000), nil
	}}

	e := &executor.Executor{
		Store: db, Ledger: led, Queue: q,
		Planner: &planner.Planner{LLM: fl, MaxInitialLeads: 1, MaxDepth: 1},
		Actors:  map[core.ActorType]actors.Actor{core.ActorWeb: slow},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:   func(context.Context, time.Duration) error { return nil },
	}

	res, err := e.Run(ctx, sess.ID)
	close(stop)
	if err != nil {
		t.Fatal(err)
	}

	if n := swept.Load(); n != 0 {
		t.Errorf("the sweeper reclaimed %d lease(s) from a running lead — it was not heartbeated", n)
	}
	if slow.count() != 1 {
		t.Errorf("the actor ran %d times; a swept lease means the lead is re-dispatched and re-charged", slow.count())
	}
	if res.LeadsFailed != 0 {
		t.Errorf("%d leads failed; a lead outlasting the TTL lost its lease", res.LeadsFailed)
	}
}

// TestSpentAndCacheStatsAreReported. The three-line comment on the final read
// says it exists for cancelled and timed-out runs — "exactly the runs whose
// spend most needs reporting" — and deleting the read left the suite green,
// including the cancellation test.
func TestSpentAndCacheStatsAreReported(t *testing.T) {
	replies := []string{planJSON("same"), planJSON("same"), `{"done":true}`}
	r := newRig(t, 20*core.MicrosPerUSD, replies,
		func(int, core.Lead) (*actors.Result, error) { return okResult(2, 25_000), nil })
	r.exec.Cache = cache.New()

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Spent == 0 {
		t.Error("Spent is 0 on a run that charged for leads and planner calls")
	}
	if after := r.reload(t); res.Spent != after.Spent {
		t.Errorf("Spent = %d but the ledger says %d", res.Spent, after.Spent)
	}
	if res.CacheStats.Hits == 0 && res.LeadsCached > 0 {
		t.Error("leads were served from cache but CacheStats reports no hits")
	}
}

// TestSpentIsReportedOnACancelledRun is the case the comment names.
func TestSpentIsReportedOnACancelledRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	r := newRig(t, 20*core.MicrosPerUSD, []string{planJSON("a", "b", "c")},
		func(n int, _ core.Lead) (*actors.Result, error) {
			if n == 1 {
				cancel()
			}
			return okResult(1, 30_000), nil
		})

	res, err := r.exec.Run(ctx, r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", res.Status)
	}
	if res.Spent == 0 {
		t.Error("Spent is 0 on a cancelled run that charged for two leads")
	}
	if after := r.reload(t); res.Spent != after.Spent {
		t.Errorf("Spent = %d but the ledger says %d", res.Spent, after.Spent)
	}
	// And the leads that ran must be recorded as finished, not left leased.
	stats, err := r.q.Stats(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Leased != 0 {
		t.Errorf("%d leads left leased after cancellation; they ran and were paid for", stats.Leased)
	}
}

// TestInterruptDuringInitialPlanningIsCancelledNotFailed. With no plan there is
// nothing to research either way, but WHY the session ended is not the same
// thing: a deliberate Ctrl-C reported as a failure looks like a crash to
// whoever reads the status or the exit code.
//
// Found on a real run — a local model too slow to finish, interrupted, and the
// session recorded as failed.
func TestInterruptDuringInitialPlanningIsCancelledNotFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	r := newRig(t, 20*core.MicrosPerUSD, nil, nil)
	// A planner whose call is interrupted mid-flight, exactly as an HTTP round
	// trip is when the signal arrives.
	r.exec.Planner = &planner.Planner{LLM: &cancellingLLM{cancel: cancel}, MaxDepth: 1}

	res, err := r.exec.Run(ctx, r.sess.ID)
	if err != nil {
		t.Fatalf("run returned an error rather than a status: %v", err)
	}
	if res.Status != core.StatusCancelled {
		t.Errorf("status = %s, want cancelled — an interrupt is not a failure", res.Status)
	}

	// And nothing may be left held.
	if after := r.reload(t); after.Held != 0 {
		t.Errorf("%d held after an interrupted plan", after.Held)
	}
	v, _ := r.led.Verify(context.Background(), r.sess.ID)
	if !v.Consistent() {
		t.Error("the ledger does not reconcile after an interrupted plan")
	}
}

// cancellingLLM cancels the context and then fails, which is what an
// interrupted HTTP round trip looks like from the caller's side.
type cancellingLLM struct{ cancel context.CancelFunc }

func (c *cancellingLLM) Name() string               { return "cancelling" }
func (c *cancellingLLM) ModelFor(t llm.Tier) string { return "m" }
func (c *cancellingLLM) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	c.cancel()
	return &llm.Response{Model: "m", Usage: llm.Usage{InputTokens: 100}},
		fmt.Errorf("Post \"http://127.0.0.1:11434/v1/chat/completions\": %w", context.Canceled)
}
