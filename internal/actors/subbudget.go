package actors

import "context"

// Sub-budget plumbing (§9.2's withSubBudget).
//
// The executor reserves an amount before dispatching a lead, and until now the
// actor had no idea what that amount was: its ceiling came from config and bore
// no relation to the reservation. So the reservation bounded nothing, and one
// lead could spend a multiple of the entire session budget — measured at 5x,
// with the escrow consumed — while §8.2 claimed overshoot was "bounded by
// estimate error on a single lead".
//
// Passed through the context rather than the Actor interface because it is
// per-call scope, exactly like a deadline, and because §9.2 describes it that
// way. An actor that ignores it still works; it is a ceiling, not a protocol.

type subBudgetKey struct{}

// WithSubBudget attaches a per-lead ceiling to ctx.
func WithSubBudget(ctx context.Context, b Budget) context.Context {
	return context.WithValue(ctx, subBudgetKey{}, b)
}

// SubBudgetFrom reads the ceiling, reporting false when none was set.
func SubBudgetFrom(ctx context.Context) (Budget, bool) {
	b, ok := ctx.Value(subBudgetKey{}).(Budget)
	return b, ok
}

// tighten returns the more restrictive of two budgets, field by field.
//
// Tighter of the two rather than a replacement: the configured budget carries
// caps the reservation says nothing about (MaxSources, MaxClaimsPerSource,
// MaxChunkTokens for a provider's request limit), and the reservation carries a
// spend ceiling config cannot know. Losing either would trade one overshoot for
// another.
func (b Budget) tighten(other Budget) Budget {
	out := b
	if other.MaxInputTokens > 0 && (out.MaxInputTokens <= 0 || other.MaxInputTokens < out.MaxInputTokens) {
		out.MaxInputTokens = other.MaxInputTokens
	}
	if other.MaxSources > 0 && (out.MaxSources <= 0 || other.MaxSources < out.MaxSources) {
		out.MaxSources = other.MaxSources
	}
	if other.MaxClaimsPerSource > 0 && (out.MaxClaimsPerSource <= 0 || other.MaxClaimsPerSource < out.MaxClaimsPerSource) {
		out.MaxClaimsPerSource = other.MaxClaimsPerSource
	}
	if other.MaxChunkTokens > 0 && (out.MaxChunkTokens <= 0 || other.MaxChunkTokens < out.MaxChunkTokens) {
		out.MaxChunkTokens = other.MaxChunkTokens
	}
	if other.AlwaysFetch {
		out.AlwaysFetch = true
	}
	return out
}
