package executor_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/store"
)

// M5 baseline (slice 0).
//
// The executor leases one lead at a time and works it to completion before the
// next, so a session's wall clock is the sum of its leads. That is the number
// M5's pool exists to change, and it has to be measured before the change
// rather than asserted after it.
//
// Measured as PEAK CONCURRENCY, not elapsed time. A timing assertion of the
// "N leads in ~N/K x delay" shape is the obvious thing to write and is flaky on
// a loaded machine — it fails for reasons that have nothing to do with the
// code. Peak in-flight is exact, and it is the property that actually changes:
// 1 today, K once the pool lands. The elapsed time is logged for a human but
// nothing depends on it.

// leadProbe is an Actor that records how many leads run at once.
//
// It persists the claims it returns, because scriptedActor does and for the same
// reason: the real WebActor writes them, and the Verifier reads the store rather
// than a batch (§11.1). A fake that skips it diverges from the thing it stands
// for.
type leadProbe struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	runs        int

	delay time.Duration
	db    store.Store
}

func (p *leadProbe) Type() core.ActorType { return core.ActorWeb }

func (p *leadProbe) Run(ctx context.Context, lead core.Lead) (*actors.Result, error) {
	p.enter()
	defer p.leave()

	// Long enough that overlapping leads would be unmistakable, short enough
	// that nine of them sequentially is under a second.
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	res := okResult(2, 1000)
	for i := range res.Claims {
		res.Claims[i].SessionID = lead.SessionID
		res.Claims[i].LeadID = lead.ID
	}
	if p.db != nil {
		if err := p.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.InsertClaims(ctx, res.Claims)
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (p *leadProbe) enter() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight++
	p.runs++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
}

func (p *leadProbe) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight--
}

func (p *leadProbe) peak() (peak, runs int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight, p.runs
}

// TestLeadsRunOneAtATime pins the DEFAULT, which is still one worker.
//
// Written before the pool existed, to fail when concurrency arrived rather than
// let it arrive silently. The pool landed in slice 4 and this still passes,
// which is correct and not an oversight: Executor.Workers defaults to 1, so
// nothing changed for a caller that did not ask for concurrency. Cassette
// recording depends on that — nondeterministic lead order makes a recording
// unreplayable — so the default cannot move before the guard that refuses
// MOLE_RECORD with a pool.
//
// It is slice 5, flipping the default to 4, that has to change this expectation
// deliberately.
func TestLeadsRunOneAtATime(t *testing.T) {
	const delay = 40 * time.Millisecond

	probe := &leadProbe{delay: delay}
	r := newRig(t, 50*core.MicrosPerUSD, []string{
		planJSON("a", "b", "c"), // initial
		planJSON("d", "e", "f"), // replan 1
		planJSON("g", "h", "i"), // replan 2
	}, nil)
	probe.db = r.db
	r.exec.Actors[core.ActorWeb] = probe

	start := time.Now()
	res, err := r.exec.Run(context.Background(), r.sess.ID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	peak, runs := probe.peak()
	if runs < 2 {
		// Without this the peak assertion is vacuous: one lead can never
		// overlap anything, and a rig that dispatched a single lead would
		// "prove" serial execution.
		t.Fatalf("only %d lead(s) ran; the rig is not exercising the loop", runs)
	}
	if peak != 1 {
		t.Fatalf("peak concurrency %d, want 1 — leads are no longer serial; "+
			"if this is M5's pool landing, update this test deliberately", peak)
	}

	t.Logf("BASELINE: %d leads, peak concurrency %d, %v elapsed (%v of it actor delay)",
		runs, peak, elapsed.Round(time.Millisecond), time.Duration(runs)*delay)
	t.Logf("session reported %d leads run, %d cached, %d failed",
		res.LeadsRun, res.LeadsCached, res.LeadsFailed)
}

// TestTheProbeCanSeeConcurrency checks the instrument, not the executor.
//
// TestLeadsRunOneAtATime asserts a peak of 1. That assertion is worthless if
// leadProbe cannot count past 1 — a probe with a broken counter reports serial
// execution forever, including after the pool lands, and the baseline it
// established would be a measurement of nothing. So the probe is driven
// concurrently here, where the answer is known by construction.
func TestTheProbeCanSeeConcurrency(t *testing.T) {
	const workers = 4

	probe := &leadProbe{delay: 40 * time.Millisecond}
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so the overlap is not a scheduling accident
			_, _ = probe.Run(context.Background(), core.Lead{SessionID: "s", ID: "l"})
		}()
	}
	close(start)
	wg.Wait()

	if peak, runs := probe.peak(); peak != workers || runs != workers {
		t.Fatalf("probe saw peak=%d runs=%d driving %d concurrent calls; "+
			"it cannot measure what the baseline test claims to measure", peak, runs, workers)
	}
}

// TestARetriedLeadCountsOnce guards a regression introduced while making
// MaxLeads bind under concurrency (M5 slice 1).
//
// The lead counter moved into the reservation transaction so the check and the
// increment could not be separated by another worker. But the reservation is
// taken per ATTEMPT, inside the retry loop — so counting every reservation made
// a lead retried through a rate limit count three times against MaxLeads. §8.5's
// lead ceiling bounds research fan-out; a flaky provider must not be able to
// shrink the research plan.
func TestARetriedLeadCountsOnce(t *testing.T) {
	r := newRig(t, 5*core.MicrosPerUSD, []string{planJSON("only one")},
		func(int, core.Lead) (*actors.Result, error) { return okResult(0, 1_000), llm.ErrRateLimited })

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	// Precondition: the retry actually happened. Without it a single-attempt
	// lead would "prove" single counting.
	if got := r.actor.count(); got != executor.MaxAttempts {
		t.Fatalf("actor ran %d times, want %d — the lead did not retry, so this "+
			"test says nothing about per-attempt counting", got, executor.MaxAttempts)
	}

	if got := r.reload(t).LeadCount; got != 1 {
		t.Fatalf("LeadCount=%d after one lead retried %d times, want 1",
			got, executor.MaxAttempts)
	}
}

// orderingProbe finishes leads in the REVERSE of the order they were queued, by
// sleeping longest for the lead it is given first.
//
// The point is to make completion order and queue order disagree. If outcomes
// were collected as they arrived, the digest and the claim list would come back
// reversed — and the replan prompt built from that digest would depend on which
// fetch happened to be quickest.
type orderingProbe struct {
	mu       sync.Mutex
	finished []string // queries, in completion order
	delays   map[string]time.Duration
	db       store.Store
}

func (p *orderingProbe) Type() core.ActorType { return core.ActorWeb }

func (p *orderingProbe) Run(ctx context.Context, lead core.Lead) (*actors.Result, error) {
	select {
	case <-time.After(p.delays[lead.Query]):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	p.mu.Lock()
	p.finished = append(p.finished, lead.Query)
	p.mu.Unlock()

	res := &actors.Result{
		Summary: "s",
		Costs: []core.ToolCall{{
			Role: core.RoleExecutor, Type: core.CallLLM, Model: "fake-model",
			Cost: core.Cost{USDMicros: 1000, InputTokens: 100, OutputTokens: 10},
		}},
		Claims: []core.Claim{{
			Text:   "claim from " + lead.Query,
			Source: "https://example.com/" + lead.Query,
			Quote:  "a quote long enough to constitute real evidence",
		}},
	}
	for i := range res.Claims {
		res.Claims[i].SessionID = lead.SessionID
		res.Claims[i].LeadID = lead.ID
	}
	if p.db != nil {
		if err := p.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.InsertClaims(ctx, res.Claims)
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (p *orderingProbe) order() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.finished...)
}

// TestWorkersRunLeadsConcurrently. The pool does what it says.
func TestWorkersRunLeadsConcurrently(t *testing.T) {
	const delay = 40 * time.Millisecond

	probe := &leadProbe{delay: delay}
	r := newRig(t, 50*core.MicrosPerUSD, []string{
		planJSON("a", "b", "c"),
		planJSON("d", "e", "f"),
		planJSON("g", "h", "i"),
	}, nil)
	probe.db = r.db
	r.exec.Actors[core.ActorWeb] = probe
	r.exec.Workers = 3 // matches ReplanEvery, so a whole batch runs at once

	start := time.Now()
	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	peak, runs := probe.peak()
	if runs < 2 {
		t.Fatalf("only %d lead(s) ran; the rig is not exercising the pool", runs)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency %d with Workers=3 — leads are still serial", peak)
	}
	t.Logf("POOL: %d leads, peak concurrency %d, %v elapsed (serial would be %v)",
		runs, peak, elapsed.Round(time.Millisecond), time.Duration(runs)*delay)
}

// TestOutcomesApplyInQueueOrderNotCompletionOrder is the determinism property
// the batch barrier exists for.
func TestOutcomesApplyInQueueOrderNotCompletionOrder(t *testing.T) {
	probe := &orderingProbe{delays: map[string]time.Duration{
		"a": 120 * time.Millisecond,
		"b": 60 * time.Millisecond,
		"c": 10 * time.Millisecond,
	}}
	r := newRig(t, 50*core.MicrosPerUSD, []string{planJSON("a", "b", "c"), `{"done":true}`}, nil)
	probe.db = r.db
	r.exec.Actors[core.ActorWeb] = probe
	r.exec.Workers = 3

	res, err := r.exec.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Precondition: completion order really did differ from queue order.
	// Without this the assertion below passes on a serial run and proves nothing.
	finished := probe.order()
	if len(finished) != 3 {
		t.Fatalf("%d leads finished, want 3: %v", len(finished), finished)
	}
	if finished[0] == "a" {
		t.Fatalf("leads finished in queue order %v; the probe did not reorder them, "+
			"so this test says nothing about ordering", finished)
	}

	var got []string
	for _, c := range res.Claims {
		got = append(got, strings.TrimPrefix(c.Text, "claim from "))
	}
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("claims applied in %v (completion order was %v), want queue order %v",
			got, finished, want)
	}
}

// TestAPoolDoesNotRunPastAReplan. ReplanEvery is 3; a pool of 8 must still stop
// at 3, or turning on workers silently changes how often the planner is
// consulted — and §9.1's whole argument is about planner cost.
//
// MaxInitialLeads is raised to 6 on purpose. With the rig's default of 3 the
// QUEUE never holds more than 3 leads, so peak concurrency is bounded by the
// queue rather than by the cadence and the test passes even with the cadence
// check removed — measured, after writing exactly that test and falsifying it.
func TestAPoolDoesNotRunPastAReplan(t *testing.T) {
	probe := &leadProbe{delay: 25 * time.Millisecond}
	r := newRig(t, 50*core.MicrosPerUSD, []string{
		planJSON("a", "b", "c", "d", "e", "f"),
		// Not done: the mid-batch replan must let the loop come back for the
		// three leads still queued, or only one batch ever runs.
		planJSON(),
		`{"done":true}`,
	}, nil)
	probe.db = r.db
	r.exec.Actors[core.ActorWeb] = probe
	r.exec.Planner.MaxInitialLeads = 6 // six queued at once, three allowed per batch
	r.exec.Workers = 8

	if _, err := r.exec.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}

	peak, runs := probe.peak()
	// Precondition: more leads were queued than a batch may run, or the cadence
	// is not being exercised at all.
	if runs < 6 {
		t.Fatalf("%d leads ran, want 6 — the queue never held more than one batch, "+
			"so the replan boundary was never tested", runs)
	}
	if peak > 3 {
		t.Fatalf("peak concurrency %d with ReplanEvery=3; the pool ran past a "+
			"replan boundary", peak)
	}
	t.Logf("queued 6, ran %d, peak %d with Workers=8", runs, peak)
}
