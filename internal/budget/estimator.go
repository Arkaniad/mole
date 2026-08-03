package budget

import (
	"sort"
	"sync"

	"github.com/lajosdeme/mole/internal/core"
)

// Estimator predicts what a lead will cost, so Reserve has an amount to hold.
//
// Rev 1's `floorCost` was undefined and, strictly, unknowable before running.
// This replaces it with something observable: a rolling p75 of settled costs
// per (actor type, depth), seeded with conservative constants on a cold start.
//
// p75 rather than the mean because the reservation should usually cover the
// call. A bad estimate costs accuracy, not correctness — the reservation
// mechanism means an under-estimate shows up as a flagged overshoot in
// SettleResult, never as a budget breach.
type Estimator struct {
	mu      sync.RWMutex
	samples map[key][]int64
	window  int
	seeds   map[core.ActorType]int64
	unit    core.BudgetUnit
}

type key struct {
	actor core.ActorType
	depth int
}

// depthBucket collapses deep lead trees so a rarely-visited depth still has
// enough samples to estimate from.
func depthBucket(d int) int {
	switch {
	case d <= 0:
		return 0
	case d <= 2:
		return 1
	default:
		return 2
	}
}

// SeedsUSD are cold-start estimates in micro-dollars, deliberately generous.
// Over-reserving briefly under-utilizes the budget; under-reserving lets work
// start that cannot be paid for.
// PlannerSeed is the reservation for one planning call, per budget unit.
//
// A planning call has no actor, so the actor-keyed seeds do not apply. Reserving
// a token before the fact — which is what the loop used to do — means planner
// spend is never gated at all: a 5000-token decomposition against a 1000-token
// budget went through unopposed.
//
// Sized generously relative to a lead, because being refused a planning call is
// worse than being refused a lead: without a plan there is nothing to research.
func PlannerSeed(unit core.BudgetUnit) int64 {
	if unit == core.BudgetTokens {
		return 4_000
	}
	return 20_000 // $0.02
}

func SeedsUSD() map[core.ActorType]int64 {
	return map[core.ActorType]int64{
		core.ActorWeb:          60_000, // $0.06
		core.ActorAcademic:     40_000, // $0.04
		core.ActorLocalCompute: 25_000, // $0.025
	}
}

// SeedsTokens are cold-start estimates in tokens.
func SeedsTokens() map[core.ActorType]int64 {
	return map[core.ActorType]int64{
		core.ActorWeb:          12_000,
		core.ActorAcademic:     9_000,
		core.ActorLocalCompute: 6_000,
	}
}

func NewEstimator(unit core.BudgetUnit) *Estimator {
	seeds := SeedsUSD()
	if unit == core.BudgetTokens {
		seeds = SeedsTokens()
	}
	return &Estimator{
		samples: map[key][]int64{},
		window:  50,
		seeds:   seeds,
		unit:    unit,
	}
}

// For returns the amount to reserve for a lead.
func (e *Estimator) For(actor core.ActorType, depth int) int64 {
	k := key{actor: actor, depth: depthBucket(depth)}

	e.mu.RLock()
	s := e.samples[k]
	seed := e.seeds[actor]
	e.mu.RUnlock()

	// Fewer than 5 observations is not a distribution; keep the seed until the
	// estimate would mean something.
	if len(s) < 5 {
		if seed == 0 {
			seed = 50_000
		}
		return seed
	}

	sorted := make([]int64, len(s))
	copy(sorted, s)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	idx := (len(sorted) * 75) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	v := sorted[idx]
	if v <= 0 {
		return 1
	}
	return v
}

// Observe records an actual settled cost.
func (e *Estimator) Observe(actor core.ActorType, depth int, amount int64) {
	if amount <= 0 {
		return
	}
	k := key{actor: actor, depth: depthBucket(depth)}

	e.mu.Lock()
	defer e.mu.Unlock()
	s := append(e.samples[k], amount)
	if len(s) > e.window {
		s = s[len(s)-e.window:]
	}
	e.samples[k] = s
}

// Warming the estimator from history is deliberately not implemented yet.
//
// Attributing a settled cost to (actor_type, depth) requires joining tool_calls
// to leads, and leads are not populated until M3. Guessing an actor type here
// would poison the distribution with rows that belong to a different one, which
// is worse than a cold start — the seeds are at least honestly conservative.
