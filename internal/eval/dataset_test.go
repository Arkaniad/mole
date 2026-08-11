package eval_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// M9 slice 5.
//
// The metrics that qualify a dataset without any labelled data: whether every row
// can be traced to a sentence, how much the merge collapsed, how much a second
// source corroborated, and how much the sources disagree. "40 rows" invites
// confidence that "40 rows, 31 from a single source, 6 contested" does not.

func datasetSession(t *testing.T, rows []dataset.Row, withSchema bool) (*sqlite.DB, string) {
	t.Helper()
	db, err := sqlite.Open(":memory:", sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	sess := &core.Session{
		ID: core.NewSessionID(), Prompt: "company revenues", Mode: core.ModeDataset,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: 1000, Status: core.StatusDone,
		CreatedAt: time.Now().UTC(),
	}
	schema, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertSession(ctx, sess); err != nil {
			return err
		}
		if withSchema {
			if err := tx.SetDatasetSchema(ctx, sess.ID, schema); err != nil {
				return err
			}
		}
		return tx.InsertRows(ctx, sess.ID, rows)
	}); err != nil {
		t.Fatal(err)
	}
	return db, sess.ID
}

func datasetMetric(t *testing.T, card eval.Scorecard, name string) eval.Metric {
	t.Helper()
	for _, m := range card.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no metric named %q", name)
	return eval.Metric{}
}

func TestDatasetMetricsQualifyTheResult(t *testing.T) {
	rows := []dataset.Row{
		// Two sources for one company, disagreeing.
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
			Source: "https://a.example", Quote: "Acme Ltd reported 1.2m"},
		{Values: map[string]string{"company": "Acme Limited", "revenue": "1350000"},
			Source: "https://b.example", Quote: "Acme Limited turnover 1.35m"},
		// One company, one source.
		{Values: map[string]string{"company": "Beta GmbH", "revenue": "900000"},
			Source: "https://c.example", Quote: "Beta GmbH made 900,000"},
	}
	db, id := datasetSession(t, rows, true)

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if m := datasetMetric(t, card, "dataset row integrity"); m.Value != 100 || m.Regression {
		t.Errorf("integrity = %v regression=%v, want 100 and false", m.Value, m.Regression)
	}
	// Three extractions became two rows.
	if m := datasetMetric(t, card, "dataset merge collapse"); !strings.Contains(m.Detail, "3 extractions became 2 rows") {
		t.Errorf("collapse detail = %q", m.Detail)
	}
	if m := datasetMetric(t, card, "dataset corroboration"); m.Value != 50 {
		t.Errorf("corroboration = %v, want 50 (one of two rows)", m.Value)
	}
	if m := datasetMetric(t, card, "dataset disagreement"); m.Value != 50 {
		t.Errorf("disagreement = %v, want 50", m.Value)
	}
	// The merge's own accuracy is named as measured elsewhere rather than omitted:
	// a scorecard listing four dataset numbers and not the one about whether the
	// merge is CORRECT would read as though nobody had asked.
	m := datasetMetric(t, card, "dataset merge accuracy")
	if m.Status != eval.Blocked {
		t.Errorf("accuracy status = %q, want blocked", m.Status)
	}
	if !strings.Contains(m.Reason, "constructed ground truth") {
		t.Errorf("the reason does not say where it is measured: %q", m.Reason)
	}
}

// TestARowWithoutAQuoteIsAHardRegression. Extraction already refuses those, so a
// stored row without one means something bypassed §11.5 rather than that a page
// was thin.
func TestARowWithoutAQuoteIsAHardRegression(t *testing.T) {
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd"}, Source: "https://a.example",
			Quote: "Acme Ltd exists"},
		{Values: map[string]string{"company": "Beta GmbH"}, Source: "https://b.example"},
	}
	db, id := datasetSession(t, rows, true)

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := datasetMetric(t, card, "dataset row integrity")
	if !m.Regression {
		t.Error("a row with no quote is not a hard regression")
	}
	if m.Value != 50 {
		t.Errorf("integrity = %v, want 50", m.Value)
	}
	if !card.Failed() {
		t.Error("the scorecard does not fail")
	}
}

// TestAReportSessionHasNoDatasetMetrics, rather than a row of zeros: a report
// session has no rows, and 0% would be indistinguishable from a dataset session
// whose extraction failed entirely.
func TestAReportSessionHasNoDatasetMetrics(t *testing.T) {
	db, id := datasetSession(t, nil, false)

	card, err := eval.Score(context.Background(), db, id, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range card.Metrics {
		if strings.HasPrefix(m.Name, "dataset ") {
			t.Errorf("a report session reports %q", m.Name)
		}
	}
}
