package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Corpus running and aggregation (§14.2).
//
// §14.3's rule is "every milestone from M3 on reports these numbers; regressions block
// merge", and until now `mole eval` scored one session at a time — so there was nothing to
// block a merge with. One session's numbers are an anecdote.
//
// This half of §14.2 needs no labelled answers. It runs a set of questions, scores each,
// and aggregates — which under MOLE_RECORD=replay is free, deterministic, offline, and
// catches the class of regression that has actually bitten this codebase: a change that
// quietly stops claims being extracted, stops edges being written, or starts rejecting
// every synthesis. The labelled half (claim precision, contradiction recall, staleness
// detection) needs ground truth a person has to supply, and is deliberately not faked here.

// Question is one corpus entry.
type Question struct {
	// ID is stable across runs so a baseline can be compared question by question.
	ID       string `json:"id"`
	Question string `json:"question"`

	// Tags group results in the aggregate. §14.2's categories — settled, disputed,
	// stale, plausible-but-wrong — are the intended vocabulary, but nothing here
	// enforces a list: a corpus that cannot describe its own questions is less useful
	// than one with an unexpected tag in it.
	Tags []string `json:"tags,omitempty"`

	// Notes is for whoever maintains the corpus. Never sent to a model.
	Notes string `json:"notes,omitempty"`
}

// Corpus is a question set.
type Corpus struct {
	Name      string     `json:"name"`
	Questions []Question `json:"questions"`
}

// LoadCorpus reads a corpus file.
func LoadCorpus(path string) (*Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read corpus: %w", err)
	}
	var c Corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("eval: parse corpus %s: %w", path, err)
	}
	if len(c.Questions) == 0 {
		return nil, fmt.Errorf("eval: corpus %s has no questions", path)
	}

	seen := map[string]bool{}
	for i := range c.Questions {
		q := &c.Questions[i]
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			return nil, fmt.Errorf("eval: corpus %s: question %d has no text", path, i+1)
		}
		if q.ID == "" {
			// Positional, so a corpus can be written without ids — but a later edit
			// that reorders questions then breaks baseline comparison, which the
			// warning in Compare is there to make visible.
			q.ID = fmt.Sprintf("q%d", i+1)
		}
		if seen[q.ID] {
			return nil, fmt.Errorf("eval: corpus %s: duplicate question id %q", path, q.ID)
		}
		seen[q.ID] = true
	}
	return &c, nil
}

// QuestionResult is one question's outcome.
type QuestionResult struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Tags     []string `json:"tags,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	// Err is set when the run itself failed. Distinct from a run that produced a poor
	// answer: one is a broken pipeline, the other is a research outcome, and folding
	// them together is how a corpus reports 100% success on a build that cannot run.
	Err string `json:"error,omitempty"`

	// Status and Claims are how a corpus tells "ran and found nothing" from "ran".
	//
	// Neither is visible in the scorecard: its hard regressions are budget overshoot,
	// ledger drift and stranded holds, and a session that produced zero claims has none
	// of them. The first replay through this runner reported a green tick for a run
	// whose planner call had failed outright — the exact regression the runner exists to
	// catch, passing its own gate.
	Status string `json:"status,omitempty"`
	Claims int    `json:"claims"`

	Card Scorecard `json:"scorecard"`
}

// Barren reports whether the question ran but produced nothing worth scoring.
func (q QuestionResult) Barren() bool {
	return q.Err == "" && (q.Claims == 0 || q.Status == "failed")
}

// CorpusReport is a whole run.
type CorpusReport struct {
	Corpus  string           `json:"corpus"`
	Results []QuestionResult `json:"results"`

	// Aggregate holds the mean of every metric measured on at least one question.
	Aggregate []Metric `json:"aggregate"`
}

// Failed reports whether the run should block a merge.
//
// Any hard regression, any question that could not run, and any question that ran and
// produced nothing. Quality is deliberately not part of it: without labelled answers there
// is no defensible threshold, and a gate that fires on a judgement call gets disabled
// within a week. "Zero claims" is not a judgement call.
func (r CorpusReport) Failed() bool {
	for _, q := range r.Results {
		if q.Err != "" || q.Barren() || q.Card.Failed() {
			return true
		}
	}
	return false
}

// Aggregate summarizes a set of question results.
//
// The MEAN of each metric across the questions that measured it, with the count carried
// in Detail so a number averaged over two questions is not read as one averaged over
// forty. Metrics that were blocked everywhere stay blocked rather than becoming 0.
func Aggregate(results []QuestionResult) []Metric {
	type acc struct {
		sum     float64
		n       int
		unit    string
		blocked int
		reason  string
	}
	byName := map[string]*acc{}
	var order []string

	for _, q := range results {
		for _, m := range q.Card.Metrics {
			a, ok := byName[m.Name]
			if !ok {
				a = &acc{unit: m.Unit}
				byName[m.Name] = a
				order = append(order, m.Name)
			}
			switch m.Status {
			case Measured:
				a.sum += m.Value
				a.n++
				if a.unit == "" {
					a.unit = m.Unit
				}
			case Blocked:
				a.blocked++
				if a.reason == "" {
					a.reason = m.Reason
				}
			}
		}
	}

	out := make([]Metric, 0, len(order))
	for _, name := range order {
		a := byName[name]
		m := Metric{Name: name, Unit: a.unit}
		switch {
		case a.n > 0:
			m.Status = Measured
			m.Value = a.sum / float64(a.n)
			m.Detail = fmt.Sprintf("mean of %d question(s)", a.n)
			if a.blocked > 0 {
				// Said out loud: an average over half the corpus is not the corpus.
				m.Detail += fmt.Sprintf("; %d blocked", a.blocked)
			}
		case a.blocked > 0:
			m.Status = Blocked
			m.Reason = a.reason
		default:
			m.Status = NotApplicable
			m.Detail = "not measured on any question"
		}
		out = append(out, m)
	}
	return out
}

// Delta is one metric's movement against a baseline.
type Delta struct {
	Name     string  `json:"name"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Change   float64 `json:"change"`
	Unit     string  `json:"unit,omitempty"`
}

// Compare reports how the aggregate moved against a stored baseline.
//
// Reports movement and says nothing about whether it is good. Without labelled answers
// there is no direction of improvement for most of these — a lower disagreement rate is
// better if the adjudicator was producing false positives and worse if it has stopped
// finding real ones — and a tool that guesses at that will be believed.
func Compare(baseline, current []Metric) ([]Delta, []string) {
	base := map[string]Metric{}
	for _, m := range baseline {
		base[m.Name] = m
	}

	var deltas []Delta
	var notes []string
	seen := map[string]bool{}

	for _, m := range current {
		seen[m.Name] = true
		b, ok := base[m.Name]
		if !ok {
			notes = append(notes, fmt.Sprintf("%s is new since the baseline", m.Name))
			continue
		}
		if m.Status != Measured || b.Status != Measured {
			if m.Status != b.Status {
				notes = append(notes, fmt.Sprintf("%s went from %s to %s", m.Name, b.Status, m.Status))
			}
			continue
		}
		if m.Value == b.Value {
			continue
		}
		deltas = append(deltas, Delta{
			Name: m.Name, Baseline: b.Value, Current: m.Value,
			Change: m.Value - b.Value, Unit: m.Unit,
		})
	}
	for _, m := range baseline {
		if !seen[m.Name] {
			// A metric that stops being reported reads as "no problem" in a diff, which
			// is the same failure as a silently skipped check.
			notes = append(notes, fmt.Sprintf("%s is no longer reported", m.Name))
		}
	}

	sort.SliceStable(deltas, func(i, j int) bool { return deltas[i].Name < deltas[j].Name })
	sort.Strings(notes)
	return deltas, notes
}
