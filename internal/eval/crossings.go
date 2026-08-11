package eval

import (
	"context"
	"fmt"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// §14.3's exfil number, per session (M8, §12.1).
//
// It was blocked here, twice — once with the reason "needs the aggregation gate
// (M8)", which had landed, and once with "per-session reporting needs the
// LocalComputeActor to have run", which was true and stayed true because nothing
// durable recorded a run. The gate's audit line went to a log, and a scorecard
// cannot read a log.
//
// The crossings table makes it computable, and the number it reports is a real
// one rather than a restatement of the enforcement: every envelope the exfil check
// WITHHELD is recorded beside every one that crossed, so the share is measured
// from outcomes rather than asserted from the fact that a check exists.
//
// The metric is worded as a regression rather than a rate to maximise. The check
// is a backstop for structural rules that refuse first — see
// internal/compute/gate/exfil.go — so a non-zero count means one of those rules
// broke in a way the backstop caught. Zero is the only acceptable value, and
// anything else fails the build.

func crossingMetrics(ctx context.Context, st store.Store, sessionID string) []Metric {
	var list []core.Crossing
	if err := st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		list, err = q.ListCrossings(ctx, sessionID)
		return err
	}); err != nil {
		// A failed read is not the same fact as "this session used no local data",
		// and reporting them alike is the mistake the dataset metrics made.
		return []Metric{{
			Name: "exfil regression", Status: Blocked,
			Reason: "could not read the session's crossings: " + err.Error(),
		}}
	}
	if len(list) == 0 {
		// Blocked, not zero. "0% of envelopes leaked" over no envelopes reads as a
		// clean bill of health for a check that never ran.
		return []Metric{{
			Name: "exfil regression", Status: Blocked,
			Reason: "this session crossed no local data; the assertion is enforced at " +
				"the gate on every envelope (internal/compute/gate, §14.3) and is " +
				"scored here only for a session that used a connector",
		}}
	}

	var crossed, refused, withheld int
	var describedRows int64
	var suppressed, withheldCols int
	for _, c := range list {
		switch c.Outcome {
		case core.CrossingCrossed:
			crossed++
			describedRows += c.RowsDescribed
		case core.CrossingRefused:
			refused++
		case core.CrossingWithheld:
			withheld++
		}
		suppressed += c.Suppressed
		withheldCols += c.ColumnsWithheld
	}

	exfil := Metric{
		Name: "exfil regression", Status: Measured, Unit: "%",
		Value: 100 * float64(withheld) / float64(len(list)),
		Detail: fmt.Sprintf("%d of %d envelope(s) were withheld for carrying row-level data",
			withheld, len(list)),
	}
	if withheld > 0 {
		// Objectively wrong rather than a quality judgement, which is the bar for
		// setting this flag.
		exfil.Regression = true
	}

	return []Metric{
		exfil,
		{
			Name: "gate refusal rate", Status: Measured, Unit: "%",
			Value: 100 * float64(refused) / float64(len(list)),
			Detail: fmt.Sprintf("%d of %d hypotheses refused; %d crossed, describing %d row(s)",
				refused, len(list), crossed, describedRows),
		},
		{
			// Reported without a direction, like the disagreement rate: a high
			// number means the data has small groups or free-text columns, which
			// is a fact about the data rather than a fault in the run.
			Name: "k-anonymity suppression", Status: Measured, Unit: "count",
			Value: float64(suppressed),
			Detail: fmt.Sprintf("%d bucket(s) fell below the floor and %d column(s) were "+
				"withheld as free text across %d crossing(s)",
				suppressed, withheldCols, len(list)),
		},
	}
}
