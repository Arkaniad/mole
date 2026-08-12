// Package compute runs one hypothesis against a connector and reports what
// crossed.
//
// Extracted when toolkit mode needed the same pipeline the LocalComputeActor runs.
// Two copies of it would have been two places for §12's rules to drift apart, and
// the rules are the product: render only from a template, parse-gate the statement,
// aggregate behind the k-anonymity floor, record every crossing including the
// refusals.
package compute

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"log/slog"

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/gate"
	"github.com/lajosdeme/mole/internal/compute/hypothesis"
	"github.com/lajosdeme/mole/internal/compute/sqlguard"
	"github.com/lajosdeme/mole/internal/core"
)

// Result is one hypothesis run: what crossed, and the audit rows describing it.
type Result struct {
	// Envelope is empty unless Err is nil.
	Envelope gate.AggregateEnvelope
	// Query is the rendered statement, or a plan descriptor when rendering itself
	// failed and there is no statement to name.
	Query string
	// Outcome and Detail are what the audit trail records. A refusal is the gate
	// working; a withholding is a rule upstream having broken in a way the exfil
	// backstop caught, and folding the two together would make §14.3's number
	// unmeasurable.
	Outcome core.CrossingOutcome
	Detail  string
	// Record summarizes a successful envelope for the audit trail.
	Record gate.Record

	Err error
}

// Run renders a plan, checks it, executes it behind the aggregation gate, and
// reports the outcome.
//
// Every exit produces an Outcome, including the ones that never reach the
// database. A trail that began at the gate would show only the questions that got
// as far as a statement — and the ones refused earlier, a free-text column asked to
// be a grouping key or a template whose slots were filled wrongly, are exactly the
// attempts somebody auditing this would want to see.
func Run(
	ctx context.Context,
	c connector.Connector,
	p hypothesis.Plan,
	opts gate.Options,
	log *slog.Logger,
) Result {
	if log == nil {
		log = slog.Default()
	}

	query, err := hypothesis.Render(c, p)
	if err != nil {
		// No statement was built, so the audit row names the plan instead.
		return Result{Query: PlanDescriptor(p), Outcome: core.CrossingRefused,
			Detail: err.Error(), Err: err}
	}

	// hypothesis.Render only emits statements from its own templates, so this
	// cannot fail — which is exactly why it is called. §12.2 asks for the parse
	// gate on the path to the database, not on the paths thought likely to carry
	// something bad.
	if err := sqlguard.Check(query); err != nil {
		return Result{Query: query, Outcome: core.CrossingRefused,
			Detail: err.Error(), Err: err}
	}

	db, err := c.Open()
	if err != nil {
		return Result{Query: query, Outcome: core.CrossingRefused,
			Detail: err.Error(), Err: err}
	}
	defer db.Close()

	opts.Log = log
	opts.FreeTextColumns = FreeTextColumns(c)

	env, err := gate.Aggregate(ctx, db, query, opts)
	if err != nil {
		outcome := core.CrossingRefused
		if errors.Is(err, gate.ErrLeak) {
			outcome = core.CrossingWithheld
		}
		return Result{Query: query, Outcome: outcome, Detail: err.Error(), Err: err}
	}
	return Result{
		Envelope: env, Query: query, Outcome: core.CrossingCrossed,
		Record: gate.Describe(env),
	}
}

// Crossing turns a Result into the audit row for it.
func (r Result) Crossing(sessionID, leadID, connectorName string) core.Crossing {
	rec := r.Record
	if rec.QueryHash == "" {
		// A refusal never became an envelope, so there is no hash to read off one.
		rec.Query, rec.QueryHash = r.Query, gate.HashQuery(r.Query)
	}
	return core.Crossing{
		SessionID:       sessionID,
		LeadID:          leadID,
		Connector:       connectorName,
		Query:           rec.Query,
		QueryHash:       rec.QueryHash,
		Outcome:         r.Outcome,
		Detail:          r.Detail,
		RowsDescribed:   rec.RowsDescribed,
		Columns:         rec.Columns,
		ColumnsWithheld: rec.ColumnsWithheld,
		Buckets:         rec.Buckets,
		Suppressed:      rec.Suppressed,
		BeyondTopK:      rec.BeyondTopK,
		Tests:           rec.Tests,
		Truncated:       rec.Truncated,
	}
}

// FreeTextColumns are the names the connector's profile flagged as prose.
//
// The gate re-derives the flag from the result anyway — a result column can be an
// expression the profile never saw — and takes the union. This is the profile's
// half of that.
func FreeTextColumns(c connector.Connector) []string {
	var out []string
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			if col.FreeText {
				out = append(out, col.Name)
			}
		}
	}
	return out
}

// PlanDescriptor identifies an attempt that never became a statement.
//
// Not SQL and not pretending to be: the template and the columns chosen, which is
// what a reader auditing a refusal needs and all that exists at that point.
// Identifiers only — the columns are names from the profile, which already crossed
// when the schema was shown.
func PlanDescriptor(p hypothesis.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- not rendered: template=%s table=%s", p.Template, p.Table)
	names := make([]string, 0, len(p.Columns))
	for role := range p.Columns {
		names = append(names, role)
	}
	sort.Strings(names)
	for _, role := range names {
		fmt.Fprintf(&b, " %s=%s", role, p.Columns[role])
	}
	return b.String()
}
