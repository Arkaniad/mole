package executor_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
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

// TestLeadsRunOneAtATime pins the pre-M5 contract.
//
// It is meant to FAIL when the pool lands. That is the point of writing it now:
// slice 4 has to change this expectation deliberately, in a commit that says so,
// rather than concurrency arriving as a silent change in behaviour.
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
