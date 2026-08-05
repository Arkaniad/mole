package eval_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/eval"
)

func writeCorpus(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestACorpusThatCannotBeTrustedIsRejected. A corpus file is the thing every later number
// is attributed to, so a duplicate or missing id silently misattributes a result.
func TestACorpusThatCannotBeTrustedIsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"no questions":  `{"name":"x","questions":[]}`,
		"empty text":    `{"name":"x","questions":[{"id":"a","question":"   "}]}`,
		"duplicate ids": `{"name":"x","questions":[{"id":"a","question":"one"},{"id":"a","question":"two"}]}`,
		"not json":      `{`,
	} {
		if _, err := eval.LoadCorpus(writeCorpus(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Ids are optional and filled in positionally.
	c, err := eval.LoadCorpus(writeCorpus(t,
		`{"name":"x","questions":[{"question":"one"},{"question":"two","tags":["stale"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Questions[0].ID != "q1" || c.Questions[1].ID != "q2" {
		t.Errorf("ids not assigned: %+v", c.Questions)
	}
	if len(c.Questions[1].Tags) != 1 {
		t.Errorf("tags were dropped: %+v", c.Questions[1])
	}
}

// TestARunThatProducedNothingFailsTheCorpus.
//
// The scorecard's hard regressions are budget overshoot, ledger drift and stranded holds —
// a session that extracted zero claims has none of them and scores clean. The first replay
// through this runner reported a green tick for a run whose planner call had failed
// outright, which is the exact regression the runner exists to catch passing its own gate.
func TestARunThatProducedNothingFailsTheCorpus(t *testing.T) {
	clean := eval.Scorecard{Metrics: []eval.Metric{
		{Name: "budget adherence", Status: eval.Measured, Value: 0},
	}}

	cases := map[string]struct {
		res  eval.QuestionResult
		fail bool
	}{
		"ran and found claims":  {eval.QuestionResult{Status: "done", Claims: 7, Card: clean}, false},
		"ran and found nothing": {eval.QuestionResult{Status: "done", Claims: 0, Card: clean}, true},
		"session failed":        {eval.QuestionResult{Status: "failed", Claims: 3, Card: clean}, true},
		"could not run at all":  {eval.QuestionResult{Err: "no search provider"}, true},
		"budget overshot": {eval.QuestionResult{Status: "done", Claims: 4, Card: eval.Scorecard{
			Metrics: []eval.Metric{{Name: "budget adherence", Status: eval.Measured, Value: 4, Regression: true}},
		}}, true},
	}
	for name, tc := range cases {
		rep := eval.CorpusReport{Results: []eval.QuestionResult{tc.res}}
		if rep.Failed() != tc.fail {
			t.Errorf("%s: Failed()=%v, want %v", name, rep.Failed(), tc.fail)
		}
	}
}

// TestTheAggregateSaysHowMuchItAveragedOver. A mean over two questions read as a mean over
// forty is how a corpus reports confidence it has not earned.
func TestTheAggregateSaysHowMuchItAveragedOver(t *testing.T) {
	results := []eval.QuestionResult{
		{Card: eval.Scorecard{Metrics: []eval.Metric{
			{Name: "cost per claim", Status: eval.Measured, Value: 100, Unit: "tokens"},
			{Name: "claim precision", Status: eval.Blocked, Reason: "needs labelled answers"},
		}}},
		{Card: eval.Scorecard{Metrics: []eval.Metric{
			{Name: "cost per claim", Status: eval.Measured, Value: 300, Unit: "tokens"},
			{Name: "claim precision", Status: eval.Blocked, Reason: "needs labelled answers"},
		}}},
		{Card: eval.Scorecard{Metrics: []eval.Metric{
			{Name: "cost per claim", Status: eval.Blocked, Reason: "no claims"},
		}}},
	}
	agg := eval.Aggregate(results)

	var cost, precision *eval.Metric
	for i := range agg {
		switch agg[i].Name {
		case "cost per claim":
			cost = &agg[i]
		case "claim precision":
			precision = &agg[i]
		}
	}
	if cost == nil || precision == nil {
		t.Fatalf("metrics missing: %+v", agg)
	}
	if cost.Value != 200 {
		t.Errorf("mean = %v, want 200 (the two measured, not the blocked one)", cost.Value)
	}
	if !strings.Contains(cost.Detail, "mean of 2") {
		t.Errorf("the detail does not say how many it averaged: %q", cost.Detail)
	}
	if !strings.Contains(cost.Detail, "1 blocked") {
		t.Errorf("a partial average is not declared partial: %q", cost.Detail)
	}
	// A metric blocked everywhere stays blocked rather than becoming 0.
	if precision.Status != eval.Blocked {
		t.Errorf("claim precision became %s with a value of %v", precision.Status, precision.Value)
	}
}

// TestAMetricThatStopsBeingReportedIsNoticed. A vanished metric reads as "no problem" in a
// diff, which is the same failure as a silently skipped check.
func TestAMetricThatStopsBeingReportedIsNoticed(t *testing.T) {
	base := []eval.Metric{
		{Name: "cost per claim", Status: eval.Measured, Value: 100},
		{Name: "disagreement rate", Status: eval.Measured, Value: 40},
		{Name: "grounding rate", Status: eval.Measured, Value: 90},
	}
	current := []eval.Metric{
		{Name: "cost per claim", Status: eval.Measured, Value: 150},
		{Name: "disagreement rate", Status: eval.Measured, Value: 40},
		{Name: "duplicate collapse", Status: eval.Measured, Value: 20},
	}

	deltas, notes := eval.Compare(base, current)
	if len(deltas) != 1 || deltas[0].Name != "cost per claim" || deltas[0].Change != 50 {
		t.Errorf("deltas = %+v, want one +50 on cost per claim", deltas)
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "grounding rate is no longer reported") {
		t.Errorf("a vanished metric was not flagged: %q", joined)
	}
	if !strings.Contains(joined, "duplicate collapse is new") {
		t.Errorf("a new metric was not flagged: %q", joined)
	}
}
