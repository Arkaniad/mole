package eval

import (
	"context"
	"errors"
	"fmt"

	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/store"
)

// Dataset-mode metrics (M9, §14.3).
//
// §15 gives M9 its "own eval set", and the honest reading of that has two halves.
//
// The half that needs ground truth — is this the right company, is this revenue
// figure true — needs labelled data, and is blocked on exactly what §14.2's corpus
// is blocked on. It is not faked here.
//
// The half that does not is worth more than it sounds. Whether a row quotes its
// source, how much the merge collapsed, how much of the result any second source
// corroborated, and how much the sources disagree about are all computable from a
// finished session with no labels and no model. They are also the numbers that
// qualify a dataset: "40 rows" invites confidence that "40 rows, 31 from a single
// source, 6 contested" does not.
//
// The merge's own accuracy is measured where ground truth can be constructed
// rather than labelled — TestMergeQualityOnKnownDuplicates reports pairwise
// precision and recall on entities written several ways — and is reported here as
// measured elsewhere, the same treatment §14.3's exfil assertion gets.

// datasetMetrics scores a dataset session.
//
// Returns nil for a session that is not one, rather than a row of zeros: a report
// session has no rows, and a metric reading 0% would be indistinguishable from a
// dataset session whose extraction failed entirely.
func datasetMetrics(ctx context.Context, st store.Store, sessionID string) []Metric {
	d, err := store.LoadDataset(ctx, st, sessionID, dataset.Options{})
	if errors.Is(err, store.ErrNotDataset) {
		return nil
	}
	if err != nil {
		// A failed read is not the same fact as "this is a report session", and
		// returning nil for both made a broken database look like a mode nobody
		// asked about. Named, so a reader can tell.
		return []Metric{{
			Name: "dataset row integrity", Status: Blocked,
			Reason: "could not read the session's rows: " + err.Error(),
		}}
	}
	metrics := []Metric{rowIntegrity(d), mergeCollapse(d), corroboration(d), disagreement(d)}

	// The merge's accuracy, reported as measured elsewhere. Naming it rather than
	// omitting it: a scorecard that listed four dataset numbers and not the one
	// about whether the merge is CORRECT would read as though nobody had asked.
	metrics = append(metrics, Metric{
		Name: "dataset merge accuracy", Status: Blocked,
		Reason: "measured on constructed ground truth rather than per session " +
			"(internal/dataset TestMergeQualityOnKnownDuplicates: pairwise precision " +
			"and recall over entities written several ways); a per-session number " +
			"needs labelled answers, as §14.2's corpus does",
	})
	return metrics
}

// rowIntegrity is §11.5 applied to a table.
//
// A row with no quote is a row nobody can trace to a sentence, and this is the one
// dataset metric that is a hard regression: extraction already refuses those, so a
// stored row without one means something bypassed the check rather than that a
// page was thin.
func rowIntegrity(d dataset.Dataset) Metric {
	m := Metric{Name: "dataset row integrity", Status: Measured, Unit: "%"}
	if d.Extracted == 0 {
		m.Status = NotApplicable
		m.Detail = "no rows extracted"
		return m
	}
	quoted := d.Extracted - d.Unquoted
	m.Value = 100 * float64(quoted) / float64(d.Extracted)
	m.Detail = fmt.Sprintf("%d of %d extracted rows carry a source and a verbatim quote",
		quoted, d.Extracted)
	if d.Unquoted > 0 {
		m.Regression = true
	}
	return m
}

// mergeCollapse is how much folding happened.
//
// Not a quality score in itself — a dataset of forty unique companies SHOULD
// collapse nothing — but the number that tells a reader which of those two
// situations they are looking at.
func mergeCollapse(d dataset.Dataset) Metric {
	m := Metric{Name: "dataset merge collapse", Status: Measured, Unit: "%"}
	if d.Extracted == 0 {
		m.Status = NotApplicable
		m.Detail = "no rows extracted"
		return m
	}
	m.Value = 100 * (1 - float64(len(d.Rows))/float64(d.Extracted))
	m.Detail = fmt.Sprintf("%d extractions became %d rows", d.Extracted, len(d.Rows))
	return m
}

// corroboration is the share of rows more than one source agreed on.
//
// The single most useful number about a dataset, and the one a row count hides: a
// table where every row came from one page is a list of things one page said.
func corroboration(d dataset.Dataset) Metric {
	m := Metric{Name: "dataset corroboration", Status: Measured, Unit: "%"}
	if len(d.Rows) == 0 {
		m.Status = NotApplicable
		m.Detail = "no rows"
		return m
	}
	c := d.Corroborated()
	m.Value = 100 * float64(c) / float64(len(d.Rows))
	m.Detail = fmt.Sprintf("%d of %d rows have more than one source", c, len(d.Rows))
	return m
}

// disagreement is the share of rows whose sources conflict on some field.
//
// Reported without a direction, for the reason §14.3's other rates are: a higher
// rate is worse if the merge is inventing conflicts and better if it is finding
// real ones, and nothing here can tell which.
func disagreement(d dataset.Dataset) Metric {
	m := Metric{Name: "dataset disagreement", Status: Measured, Unit: "%"}
	if len(d.Rows) == 0 {
		m.Status = NotApplicable
		m.Detail = "no rows"
		return m
	}
	c := d.Contested()
	m.Value = 100 * float64(c) / float64(len(d.Rows))
	m.Detail = fmt.Sprintf("%d of %d rows have a field the sources disagree about",
		c, len(d.Rows))
	return m
}
