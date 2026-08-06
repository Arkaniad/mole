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
		{Name: "budget overshoot", Status: eval.Measured, Value: 0},
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
			Metrics: []eval.Metric{{Name: "budget overshoot", Status: eval.Measured, Value: 4, Regression: true}},
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

// TestUnlabelledPairsAreExcludedNotGuessed. A score computed over whichever pairs someone
// got round to labelling, reported as the score, is how a test set starts lying.
func TestUnlabelledPairsAreExcludedNotGuessed(t *testing.T) {
	ps := &eval.PairSet{Pairs: []eval.LabelledPair{
		{Pair: "a|b", Model: "contradicts", Label: "contradicts", Judged: true},
		{Pair: "a|c", Model: "contradicts", Label: "unrelated", Judged: true},
		{Pair: "a|d", Model: "contradicts", Label: "", Judged: true},
		{Pair: "a|e", Model: "supports", Label: "  ", Judged: true},
	}}
	s := eval.ScorePairs(ps)

	if s.Labelled != 2 || s.Unlabelled != 2 {
		t.Errorf("labelled=%d unlabelled=%d, want 2 and 2", s.Labelled, s.Unlabelled)
	}
	if s.Accuracy() != 0.5 {
		t.Errorf("accuracy = %.2f over the two labelled pairs, want 0.50", s.Accuracy())
	}
	if p := s.Precision["contradicts"]; p != 0.5 {
		t.Errorf("contradicts precision = %.2f, want 0.50", p)
	}
}

// TestAPairTheModelNeverSawIsNotHeldAgainstIt. Verification is incremental and can stop
// early — at max_leads, or when the share cap binds — so a pair involving an unverified
// claim was never put to the judge. Scoring it would measure coverage as accuracy.
func TestAPairTheModelNeverSawIsNotHeldAgainstIt(t *testing.T) {
	ps := &eval.PairSet{Pairs: []eval.LabelledPair{
		{Pair: "a|b", Model: "contradicts", Label: "contradicts", Judged: true},
		{Pair: "a|c", Model: "unrelated", Label: "contradicts", Judged: false},
	}}
	s := eval.ScorePairs(ps)

	if s.Labelled != 1 {
		t.Errorf("labelled = %d, want 1", s.Labelled)
	}
	if s.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", s.Skipped)
	}
	if s.Accuracy() != 1 {
		t.Errorf("accuracy = %.2f; a pair the model never saw was scored against it",
			s.Accuracy())
	}
}

// TestPrecisionAndRecallAnswerDifferentQuestions.
//
// The distinction is the whole point here. A judge that calls everything a contradiction
// has perfect recall and terrible precision, and a live run looked exactly like that: 16
// contradictions whose rationales described the pairs as being about different topics.
func TestPrecisionAndRecallAnswerDifferentQuestions(t *testing.T) {
	// Says "contradicts" to everything. Two of the six really are.
	var pairs []eval.LabelledPair
	for i := 0; i < 6; i++ {
		label := "unrelated"
		if i < 2 {
			label = "contradicts"
		}
		pairs = append(pairs, eval.LabelledPair{
			Pair: string(rune('a'+i)) + "|x", Model: "contradicts", Label: label, Judged: true,
		})
	}
	s := eval.ScorePairs(&eval.PairSet{Pairs: pairs})

	if p := s.Precision["contradicts"]; p > 0.34 {
		t.Errorf("contradicts precision = %.2f; an over-reporting judge scored well", p)
	}
	if r := s.Recall["contradicts"]; r != 1 {
		t.Errorf("contradicts recall = %.2f, want 1.00 — it found every real one", r)
	}
	// And the confusion says what it said instead, which is what tells you whether to
	// change the prompt or the model.
	if n := s.Confusion["contradicts"]["unrelated"]; n != 4 {
		t.Errorf("confusion contradicts→unrelated = %d, want 4", n)
	}
}

// TestSelfConsistencyIsMeasurableWithoutLabels.
//
// The cheapest useful measurement of a judge. Run the same model over the same pairs twice
// and see how often it agrees with itself: qwen2.5:3b re-judging sixteen of its own
// contradictions kept three, calling seven "unrelated" and four "supports". A judge that
// does not agree with itself is not measuring anything, and no quantity of labelling fixes
// it — so this screens judges far more cheaply than a labelled set can.
func TestSelfConsistencyIsMeasurableWithoutLabels(t *testing.T) {
	a := &eval.PairSet{Model: "run-one", Pairs: []eval.LabelledPair{
		{Pair: "1", Model: "contradicts", Judged: true},
		{Pair: "2", Model: "contradicts", Judged: true},
		{Pair: "3", Model: "contradicts", Judged: true},
		{Pair: "4", Model: "supports", Judged: true},
	}}
	b := &eval.PairSet{Model: "run-two", Pairs: []eval.LabelledPair{
		{Pair: "1", Model: "contradicts", Judged: true},
		{Pair: "2", Model: "unrelated", Judged: true},
		{Pair: "3", Model: "supports", Judged: true},
		{Pair: "4", Model: "supports", Judged: true},
	}}

	ag := eval.CompareVerdicts(a, b)
	if ag.Compared != 4 || ag.Same != 2 {
		t.Errorf("compared=%d same=%d, want 4 and 2", ag.Compared, ag.Same)
	}
	if ag.Rate() != 0.5 {
		t.Errorf("rate = %.2f, want 0.50", ag.Rate())
	}
	if ag.Confusion["contradicts"]["unrelated"] != 1 {
		t.Errorf("confusion does not record what it changed to: %+v", ag.Confusion)
	}
}

// TestAMutualNonAnswerIsNotAgreement. Counting two silences as a match is how a judge that
// answers nothing scores perfectly.
func TestAMutualNonAnswerIsNotAgreement(t *testing.T) {
	a := &eval.PairSet{Pairs: []eval.LabelledPair{
		{Pair: "1", Model: "", Judged: false},
		{Pair: "2", Model: "contradicts", Judged: true},
		{Pair: "3", Model: "contradicts", Judged: true},
	}}
	b := &eval.PairSet{Pairs: []eval.LabelledPair{
		{Pair: "1", Model: "", Judged: false},
		{Pair: "2", Model: "contradicts", Judged: true},
		{Pair: "3", Model: "", Judged: false},
	}}

	ag := eval.CompareVerdicts(a, b)
	if ag.Compared != 1 || ag.Same != 1 {
		t.Errorf("compared=%d same=%d, want 1 and 1", ag.Compared, ag.Same)
	}
	if ag.Unanswered != 2 {
		t.Errorf("unanswered = %d, want 2", ag.Unanswered)
	}
	if ag.Rate() != 1 {
		t.Errorf("rate = %.2f over the single pair both judged", ag.Rate())
	}
}
