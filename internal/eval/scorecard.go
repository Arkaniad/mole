// Package eval scores a finished session.
//
// §14.3 lists nine metrics. Four of them cannot be computed yet — three need
// components that arrive in M4 and M8, one needs labelled answers — and this
// package reports that rather than omitting them. A scorecard showing five
// numbers reads as a complete picture; a scorecard showing five numbers and
// four explicit "not measured" lines reads as what it is.
//
// Everything here is mechanical: pure arithmetic over persisted state, no model
// calls, no network, no judgement. That is the whole point. These are the
// numbers that can block a merge without anyone arguing about them, and they
// work today against a session `mole research` already wrote.
package eval

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// Status says whether a metric was computed, and if not, why not.
type Status string

const (
	// Measured means the number is real.
	Measured Status = "measured"
	// Blocked means the metric cannot be computed until something lands.
	// Reported explicitly so a zero is never mistaken for a regression.
	Blocked Status = "blocked"
	// NotApplicable means this session had nothing to measure.
	NotApplicable Status = "n/a"
)

// Metric is one line of the scorecard.
type Metric struct {
	Name   string `json:"name"`
	Status Status `json:"status"`

	// Value and Unit are set when Status is Measured.
	Value float64 `json:"value,omitempty"`
	Unit  string  `json:"unit,omitempty"`

	// Detail is the arithmetic behind the number, so a reader can check it.
	Detail string `json:"detail,omitempty"`
	// Reason explains a Blocked status: what is missing and which milestone
	// brings it.
	Reason string `json:"reason,omitempty"`

	// Regression marks a metric that should fail a build. Only ever set on
	// metrics that are objectively wrong, never on quality judgements.
	Regression bool `json:"regression,omitempty"`
}

// Scorecard is one session's evaluation.
type Scorecard struct {
	SessionID string   `json:"session_id"`
	Metrics   []Metric `json:"metrics"`

	// Citations is the per-claim detail behind the citation accuracy metric,
	// present only when it was computed.
	Citations *CitationReport `json:"citations,omitempty"`
}

// Failed reports whether any metric is a hard regression.
func (s Scorecard) Failed() bool {
	for _, m := range s.Metrics {
		if m.Regression {
			return true
		}
	}
	return false
}

// Options tune what Score computes.
type Options struct {
	// Citations, when set, re-reads every cited source and checks the quote is
	// actually in it (§14.3 citation accuracy).
	//
	// Off by default because it costs a fetch per source. Under cassette
	// replay it is free and deterministic, which is what §14.1 was built for.
	Citations SourceReader
}

// Score evaluates one session.
func Score(ctx context.Context, st store.Store, sessionID string, opts Options) (Scorecard, error) {
	card := Scorecard{SessionID: sessionID}

	var (
		sess     *core.Session
		claims   []*core.Claim
		calls    []*core.ToolCall
		outcomes []*store.FetchOutcome
	)
	if err := st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if sess, err = q.GetSession(ctx, sessionID); err != nil {
			return err
		}
		if claims, err = q.ListClaims(ctx, sessionID, 10_000); err != nil {
			return err
		}
		if calls, err = q.ListToolCalls(ctx, sessionID, 10_000); err != nil {
			return err
		}
		outcomes, err = q.ListFetchOutcomes(ctx, sessionID, 10_000)
		return err
	}); err != nil {
		return card, err
	}

	led := budget.New(st, budget.DefaultConfig())
	verify, err := led.Verify(ctx, sessionID)
	if err != nil {
		return card, err
	}

	card.Metrics = append(card.Metrics,
		budgetAdherence(sess),
		ledgerConsistency(verify),
		holdsReleased(verify),
		claimIntegrity(claims),
		sourceConcentration(claims),
		costPerClaim(sess, claims),
		toolCallCount(calls),
	)
	if opts.Citations != nil {
		rep := VerifyCitations(ctx, claims, providerSupplied(outcomes), opts.Citations)
		card.Citations = &rep
		card.Metrics = append(card.Metrics, citationAccuracy(rep))
	}

	card.Metrics = append(card.Metrics, blockedMetrics(opts)...)
	return card, nil
}

// ---------------------------------------------------------------------------
// Mechanical metrics
// ---------------------------------------------------------------------------

// budgetAdherence is §14.3's "max observed overshoot past ceiling. Must be ~0."
//
// The headline correctness property of the whole system. Everything else here
// is diagnostic; this one is a contract.
func budgetAdherence(s *core.Session) Metric {
	m := Metric{Name: "budget adherence", Status: Measured, Unit: "%"}

	over := s.Spent - s.Budget
	if over <= 0 {
		m.Value = 0
		m.Detail = fmt.Sprintf("spent %s of %s, no overshoot",
			core.FormatAmount(s.Spent, s.BudgetUnit), core.FormatAmount(s.Budget, s.BudgetUnit))
		return m
	}

	m.Value = 100 * float64(over) / float64(s.Budget)
	m.Detail = fmt.Sprintf("spent %s against a %s ceiling — over by %s",
		core.FormatAmount(s.Spent, s.BudgetUnit),
		core.FormatAmount(s.Budget, s.BudgetUnit),
		core.FormatAmount(over, s.BudgetUnit))
	// Any overshoot at all is a regression. The reserve/settle design exists to
	// make this impossible, so a non-zero value means the design is not holding
	// rather than that a limit was slightly loose.
	m.Regression = true
	return m
}

// ledgerConsistency reconciles the materialized counters against the
// append-only rows. A divergence means the ceiling is being enforced against a
// number nothing checks.
func ledgerConsistency(v budget.VerifyResult) Metric {
	m := Metric{Name: "ledger consistency", Status: Measured, Unit: "ok"}
	if v.Consistent() {
		m.Value = 1
		m.Detail = fmt.Sprintf("%d call(s) reconcile", v.CallsFromRows)
		return m
	}
	m.Value = 0
	m.Regression = true
	m.Detail = fmt.Sprintf("spent recorded=%d ledger=%d · held recorded=%d rows=%d · calls recorded=%d rows=%d",
		v.SpentRecorded, v.SpentFromLedger, v.HeldRecorded, v.HeldFromRows, v.CallsRecorded, v.CallsFromRows)
	return m
}

// holdsReleased catches budget stranded in a reservation nobody settled. It is
// invisible in a spend total — the money is neither spent nor available — and
// it is how a long session runs out of a budget it never used.
func holdsReleased(v budget.VerifyResult) Metric {
	m := Metric{Name: "holds released", Status: Measured, Unit: "held"}
	m.Value = float64(v.HeldFromRows)
	if v.HeldFromRows == 0 {
		m.Detail = "no reservations left held"
		return m
	}
	m.Regression = true
	m.Detail = fmt.Sprintf("%d still held after the session finished", v.HeldFromRows)
	return m
}

// claimIntegrity checks every claim against the invariants §11.5 promises: a
// quote long enough to be evidence, an offset that could locate it, and a
// source that resolves.
//
// This is NOT citation accuracy — that requires the source text, which the
// actor discards by design (§2). It is the subset checkable from the row alone,
// and a failure here means a claim was written that the pipeline should have
// rejected.
//
// RetrievedAt is deliberately not checked: InsertClaims defaults it, so a
// persisted claim cannot have a zero one. A check that can never fire implies
// coverage this does not have.
func claimIntegrity(claims []*core.Claim) Metric {
	m := Metric{Name: "claim integrity", Status: Measured, Unit: "%"}
	if len(claims) == 0 {
		m.Status = NotApplicable
		m.Detail = "no claims"
		return m
	}

	var problems []string
	bad := 0
	for _, c := range claims {
		var why []string
		if strings.TrimSpace(c.Text) == "" {
			why = append(why, "empty text")
		}
		if len(strings.TrimSpace(c.Quote)) < 24 {
			why = append(why, "quote too short to be evidence")
		}
		if c.QuoteOffset < 0 {
			why = append(why, "negative quote offset")
		}
		if u, err := url.Parse(c.Source); err != nil || u.Host == "" {
			why = append(why, "unresolvable source")
		}
		// Both are 0-1. Confidence is 0 on an unverified claim, which is correct
		// rather than malformed, so only the range is checked here.
		if c.Confidence < 0 || c.Confidence > 1 {
			why = append(why, "derived confidence outside 0-1")
		}
		if c.AssertionStrength < 0 || c.AssertionStrength > 1 {
			why = append(why, "assertion strength outside 0-1")
		}
		if len(why) > 0 {
			bad++
			if len(problems) < 3 {
				problems = append(problems, fmt.Sprintf("%.40q: %s", c.Text, strings.Join(why, ", ")))
			}
		}
	}

	m.Value = 100 * float64(len(claims)-bad) / float64(len(claims))
	m.Detail = fmt.Sprintf("%d of %d claims well-formed", len(claims)-bad, len(claims))
	if bad > 0 {
		m.Regression = true
		m.Detail += " — " + strings.Join(problems, "; ")
	}
	return m
}

// sourceConcentration reports how much of the answer came from one page.
//
// Diagnostic, not a pass/fail: a question with one good source is legitimate.
// But a run that reads five sources and cites one is usually a run where four
// of them failed quietly, and that is invisible in a claim count.
func sourceConcentration(claims []*core.Claim) Metric {
	m := Metric{Name: "source concentration", Status: Measured, Unit: "%"}
	if len(claims) == 0 {
		m.Status = NotApplicable
		m.Detail = "no claims"
		return m
	}

	bySource := map[string]int{}
	for _, c := range claims {
		bySource[c.Source]++
	}
	top, topN := "", 0
	for src, n := range bySource {
		if n > topN || (n == topN && src < top) {
			top, topN = src, n
		}
	}

	m.Value = 100 * float64(topN) / float64(len(claims))
	m.Detail = fmt.Sprintf("%d distinct source(s); the largest contributed %d of %d claims (%s)",
		len(bySource), topN, len(claims), top)
	return m
}

// costPerClaim is the denominator half of §14.3's headline metric.
//
// Explicitly NOT "cost per correct claim": correctness needs labels this
// package does not have. Reported under its own name so the two are never
// confused — optimizing cost per claim alone rewards a model that emits more
// claims, which is the opposite of the intent.
func costPerClaim(s *core.Session, claims []*core.Claim) Metric {
	m := Metric{Name: "cost per claim", Status: Measured, Unit: string(s.BudgetUnit)}
	if len(claims) == 0 {
		m.Status = NotApplicable
		m.Detail = fmt.Sprintf("no claims; %s spent", core.FormatAmount(s.Spent, s.BudgetUnit))
		return m
	}
	per := s.Spent / int64(len(claims))
	m.Value = float64(per)
	m.Detail = fmt.Sprintf("%s over %d claims = %s each",
		core.FormatAmount(s.Spent, s.BudgetUnit), len(claims),
		core.FormatAmount(per, s.BudgetUnit))
	return m
}

// toolCallCount is context for the numbers above: cost per claim means
// something different at three calls than at thirty.
func toolCallCount(calls []*core.ToolCall) Metric {
	m := Metric{Name: "tool calls", Status: Measured, Unit: "calls", Value: float64(len(calls))}

	byType := map[core.CallType]int{}
	failed := 0
	for _, c := range calls {
		byType[c.Type]++
		if c.Err != "" {
			failed++
		}
	}
	kinds := make([]string, 0, len(byType))
	for t, n := range byType {
		kinds = append(kinds, fmt.Sprintf("%s %d", t, n))
	}
	sort.Strings(kinds)

	m.Detail = strings.Join(kinds, " · ")
	if failed > 0 {
		m.Detail += fmt.Sprintf(" · %d errored", failed)
	}
	return m
}

// ---------------------------------------------------------------------------
// What cannot be measured yet
// ---------------------------------------------------------------------------

// blockedMetrics names §14.3's remaining entries and why each is unavailable.
//
// Listing them is the point. Four of nine metrics will read zero until M4 and
// M8, and a scorecard that silently omitted them would let a reader conclude
// the pipeline is fully scored — then read a genuine future regression as
// normal.
func blockedMetrics(opts Options) []Metric {
	metrics := []Metric{
		{
			Name: "claim precision", Status: Blocked,
			Reason: "needs labelled answers; the question corpus (§14.2) is not built yet",
		},
		{
			Name: "grounding rate", Status: Blocked,
			Reason: "needs a judge: whether a quote SUPPORTS its claim is not mechanical. " +
				"Quote-was-found is already enforced at the actor boundary (§11.5)",
		},
		{
			Name: "contradiction recall", Status: Blocked,
			Reason: "needs the Verifier and planted disagreements — M4",
		},
		{
			Name: "staleness detection", Status: Blocked,
			Reason: "needs supersedes edges — M4",
		},
		{
			Name: "exfil regression", Status: Blocked,
			Reason: "needs the aggregation gate (§12.1) — M8",
		},
	}

	if opts.Citations == nil {
		// Reported as blocked rather than omitted: it IS computable now, and a
		// reader should know a run declined to compute it rather than assume
		// the metric does not exist yet.
		metrics = append(metrics, Metric{
			Name: "citation accuracy", Status: Blocked,
			Reason: "not requested; re-reads every cited source, so pass --citations " +
				"(free and deterministic under MOLE_RECORD=replay)",
		})
	}
	return metrics
}
