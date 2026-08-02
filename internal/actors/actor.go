// Package actors executes leads.
//
// An Actor is the only component that touches raw source material. It takes one
// lead, does the work, and returns a summary plus claims — never the pages,
// never the raw text. That boundary is what keeps planner cost from compounding
// as research deepens (§2), and it is also where fabricated citations die
// (§11.5).
package actors

import (
	"context"

	"github.com/lajosdeme/mole/internal/core"
)

// Result is what one lead produced.
type Result struct {
	// Summary is a few paragraphs. The planner sees this; it never sees the
	// pages behind it.
	Summary string

	// Claims are atomic facts, each carrying a source and a verbatim quote
	// that has already been checked against the text it came from.
	Claims []core.Claim

	// Costs are the tool calls this run made, for the ledger to settle. Every
	// call is recorded whether or not it succeeded — the money was spent
	// either way, and a ledger of successes only cannot enforce a ceiling.
	Costs []core.ToolCall

	// Truncated reports that the sub-budget could not cover the whole
	// document. Surfaced rather than silently dropping content (§4.1).
	Truncated bool

	// Stats describe what happened, for the trace view and for M2's metrics.
	Stats RunStats
}

// RunStats is per-run accounting that is not money.
type RunStats struct {
	SearchResults int
	Fetched       int
	// SkippedFetch counts results whose content the search provider already
	// supplied. The whole reason for preferring a provider that returns page
	// text (§10.4), and worth measuring rather than assuming.
	SkippedFetch  int
	Chunks        int
	ChunksSkipped int

	ClaimsProposed int
	// ClaimsRejected counts claims discarded because their quote did not
	// appear in the source. A rising rate here is the signal that a model or
	// prompt has started fabricating.
	ClaimsRejected int
}

// Actor executes a lead.
type Actor interface {
	Run(ctx context.Context, lead core.Lead) (*Result, error)
	Type() core.ActorType
}

// Budget is what an actor may spend on one lead.
//
// The actor does not reserve or settle — the executor owns that. It receives a
// ceiling and reports what it used, so the reservation and the actual cost stay
// in one place.
type Budget struct {
	// MaxInputTokens caps the text fed to the model across the whole run.
	MaxInputTokens int64
	// MaxSources caps how many search results are read.
	MaxSources int
	// MaxClaimsPerSource bounds a single page's contribution, so one verbose
	// document cannot dominate the graph.
	MaxClaimsPerSource int
}

func (b Budget) withDefaults() Budget {
	if b.MaxInputTokens <= 0 {
		b.MaxInputTokens = 60_000
	}
	if b.MaxSources <= 0 {
		b.MaxSources = 5
	}
	if b.MaxClaimsPerSource <= 0 {
		b.MaxClaimsPerSource = 8
	}
	return b
}
