package budget_test

import (
	"testing"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
)

// The estimator's warm start (§8.4: "improves with use").
//
// Without it, "with use" meant within one session: every new session began at the
// cold seed and threw away everything the last one measured. The note saying this
// was blocked outlived its blocker — M3 populated leads, which is the join it
// needed — by several milestones.

func leadCost(actor core.ActorType, depth int, usd int64) core.LeadCost {
	return core.LeadCost{ActorType: actor, Depth: depth, Cost: core.Cost{USDMicros: usd}}
}

// TestAWarmedEstimatorUsesHistoryInsteadOfTheSeed.
func TestAWarmedEstimatorUsesHistoryInsteadOfTheSeed(t *testing.T) {
	e := budget.NewEstimator(core.BudgetUSD)
	seed := e.For(core.ActorWeb, 0)
	if seed != budget.SeedsUSD()[core.ActorWeb] {
		t.Fatalf("cold estimate = %d, want the seed %d", seed, budget.SeedsUSD()[core.ActorWeb])
	}

	// Ten cheap web leads: an install where a lead really costs about $0.005.
	var samples []core.LeadCost
	for i := 0; i < 10; i++ {
		samples = append(samples, leadCost(core.ActorWeb, 0, 5_000))
	}
	if n := e.Warm(samples); n != 10 {
		t.Fatalf("warmed %d samples, want 10", n)
	}

	got := e.For(core.ActorWeb, 0)
	if got != 5_000 {
		t.Errorf("warm estimate = %d, want 5000 (the observed p75)", got)
	}
	if got >= seed {
		t.Errorf("the warm estimate %d is no better than the seed %d", got, seed)
	}
}

// TestWarmingKeepsTheActorAndDepthApart.
//
// Guessing an actor type was the stated reason not to build this: it would poison
// the distribution with rows belonging to a different one. Nothing is guessed, so
// the separation has to actually hold.
func TestWarmingKeepsTheActorAndDepthApart(t *testing.T) {
	e := budget.NewEstimator(core.BudgetUSD)

	var samples []core.LeadCost
	for i := 0; i < 10; i++ {
		samples = append(samples, leadCost(core.ActorWeb, 0, 5_000))
		samples = append(samples, leadCost(core.ActorLocalCompute, 0, 90_000))
		samples = append(samples, leadCost(core.ActorWeb, 3, 40_000))
	}
	e.Warm(samples)

	if got := e.For(core.ActorWeb, 0); got != 5_000 {
		t.Errorf("web at depth 0 = %d, want 5000", got)
	}
	if got := e.For(core.ActorLocalCompute, 0); got != 90_000 {
		t.Errorf("local at depth 0 = %d, want 90000 — another actor's costs leaked in", got)
	}
	if got := e.For(core.ActorWeb, 3); got != 40_000 {
		t.Errorf("web at depth 3 = %d, want 40000 — another depth's costs leaked in", got)
	}
}

// TestWarmingRespectsTheBudgetUnit. The same history has to seed a token session
// with tokens, not with micro-dollars.
func TestWarmingRespectsTheBudgetUnit(t *testing.T) {
	sample := core.LeadCost{
		ActorType: core.ActorWeb,
		Cost:      core.Cost{USDMicros: 5_000, InputTokens: 8_000, OutputTokens: 1_000},
	}
	var samples []core.LeadCost
	for i := 0; i < 10; i++ {
		samples = append(samples, sample)
	}

	usd := budget.NewEstimator(core.BudgetUSD)
	usd.Warm(samples)
	if got := usd.For(core.ActorWeb, 0); got != 5_000 {
		t.Errorf("usd estimate = %d, want 5000", got)
	}

	tokens := budget.NewEstimator(core.BudgetTokens)
	tokens.Warm(samples)
	if got := tokens.For(core.ActorWeb, 0); got != 9_000 {
		t.Errorf("token estimate = %d, want 9000 (the tokens, not the micro-dollars)", got)
	}
}

// TestTooFewSamplesKeepTheSeed. Fewer than five observations is not a
// distribution, and warming must not smuggle a sample of one past that rule.
func TestTooFewSamplesKeepTheSeed(t *testing.T) {
	e := budget.NewEstimator(core.BudgetUSD)
	e.Warm([]core.LeadCost{leadCost(core.ActorWeb, 0, 1)})

	if got := e.For(core.ActorWeb, 0); got != budget.SeedsUSD()[core.ActorWeb] {
		t.Errorf("estimate = %d after one sample, want the seed", got)
	}
}

// TestAFreeLeadIsNotASample. A lead whose calls cost nothing in this unit says
// nothing about what the next one will cost, and ten of them would drive every
// reservation to the floor.
func TestAFreeLeadIsNotASample(t *testing.T) {
	e := budget.NewEstimator(core.BudgetUSD)
	var samples []core.LeadCost
	for i := 0; i < 10; i++ {
		samples = append(samples, leadCost(core.ActorWeb, 0, 0))
	}
	if n := e.Warm(samples); n != 0 {
		t.Errorf("warmed %d free leads, want 0", n)
	}
	if got := e.For(core.ActorWeb, 0); got != budget.SeedsUSD()[core.ActorWeb] {
		t.Errorf("estimate = %d, want the seed", got)
	}
}
