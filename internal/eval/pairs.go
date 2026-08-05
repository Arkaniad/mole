package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/verifier"
)

// Adjudicator test sets.
//
// §11.2's edge inference is the stage whose errors are hardest to see: a bad extractor
// produces no claims, a bad judge produces a confident graph of wrong edges and everything
// downstream reads it as fact. A live run produced 16 contradictions out of 37 edges, and
// the model's own rationales described the pairs as being about different topics — which
// is "unrelated". Every false positive spent a follow-up lead, docked two claims'
// confidence, and put a disagreement in the report that was not there.
//
// None of the existing metrics can see that. "disagreement rate" counts what was FOUND and
// says so in its own comment; measuring what is CORRECT needs someone to say what the right
// answer was.
//
// So: dump the pairs a session judged, with both claim texts, for a person to label. After
// that, any prompt or model change is measurable in seconds, offline, against real pairs a
// real run produced — rather than against questions someone invented to be measured.

// LabelledPair is one judgement, before and after a human looks at it.
type LabelledPair struct {
	// Pair is the canonical key, stable across dumps of the same session.
	Pair string `json:"pair"`

	A string `json:"a"`
	B string `json:"b"`

	SourceA string `json:"source_a,omitempty"`
	SourceB string `json:"source_b,omitempty"`

	// Model is what the adjudicator said. "unrelated" means no edge was stored, which
	// covers both "judged unrelated" and "never reached" — Judged distinguishes them.
	Model string `json:"model"`
	// Why is the model's own rationale, which is often the clearest evidence that a
	// verdict is wrong.
	Why string `json:"why,omitempty"`

	// Judged is false when one of the claims was never verified, so the pair was never
	// put to the model at all. Scoring skips those: they say nothing about the judge.
	Judged bool `json:"judged"`

	// Label is the truth, filled in by hand. Empty means unlabelled.
	Label string `json:"label"`
	// Note is for the labeller.
	Note string `json:"note,omitempty"`
}

// PairSet is a dumped set awaiting labels.
type PairSet struct {
	Session string         `json:"session"`
	Model   string         `json:"adjudicator,omitempty"`
	Pairs   []LabelledPair `json:"pairs"`
}

// DumpOptions filter what is written.
type DumpOptions struct {
	// Kinds restricts the dump to pairs the model gave these verdicts. Empty means every
	// pair that carries an edge.
	Kinds []string
	// All includes pairs with no edge — the ones the model called unrelated.
	//
	// Off by default because they dominate: a 25-claim session produces a few dozen
	// edges and several hundred candidate pairs, and a labelling task nobody finishes
	// measures nothing. Turn it on to measure RECALL — whether real disagreements were
	// missed — once precision is known.
	All bool
	// MaxCandidatesPerClaim mirrors the Verifier's retrieval cap, so the candidate space
	// is regenerated exactly as the run saw it.
	MaxCandidatesPerClaim int
}

// DumpPairs regenerates a session's candidate pairs and pairs them with what the
// adjudicator said.
//
// Retrieval is deterministic and costs nothing, so the candidate space is reproduced
// rather than stored — which also means a dump taken today reflects today's retriever.
func DumpPairs(ctx context.Context, st store.Store, sessionID string, opts DumpOptions) (*PairSet, error) {
	var (
		claims []*core.Claim
		edges  []*core.ClaimEdge
	)
	if err := st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if claims, err = q.ListClaims(ctx, sessionID, 0); err != nil {
			return err
		}
		edges, err = q.ListEdges(ctx, sessionID, 0)
		return err
	}); err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("eval: session %s has no claims", sessionID)
	}

	byID := make(map[string]*core.Claim, len(claims))
	for _, c := range claims {
		byID[c.ID] = c
	}

	// What the model said, keyed the way pairs are keyed.
	type verdict struct{ kind, why string }
	said := make(map[string]verdict, len(edges))
	for _, e := range edges {
		a, b := e.FromID, e.ToID
		if a > b {
			a, b = b, a
		}
		said[a+"|"+b] = verdict{string(e.Kind), e.Rationale}
	}

	// Every pair that CARRIES AN EDGE, regardless of whether retrieval would still
	// surface it.
	//
	// Regenerating the candidate space does not reproduce what the run judged. The
	// Verifier works incrementally (§11.1): when a claim was new it was retrieved against
	// the handful of claims that existed then, and its top-8 against the finished set of
	// 25 is a different eight. Measured on a real session: regeneration found 11 of the
	// 16 contradictions the run actually produced, so a test set built from it would have
	// silently excluded five real judgements — and they are judgements, which is the only
	// thing this file is for.
	//
	// Regeneration is still used for the no-edge pairs, since there is no other record of
	// them; that set is approximate and only matters when measuring recall.
	all := make([]verifier.Pair, 0, len(edges))
	for _, e := range edges {
		a, b := byID[e.FromID], byID[e.ToID]
		if a == nil || b == nil {
			continue
		}
		if a.ID > b.ID {
			a, b = b, a
		}
		all = append(all, verifier.Pair{A: a, B: b})
	}

	if opts.All {
		pairs, free, err := verifier.CandidatePairs(ctx, verifier.LexicalRetriever{},
			claims, claims, opts.MaxCandidatesPerClaim, nil)
		if err != nil {
			return nil, err
		}
		all = append(all, pairs...)
		// Mechanically-decided pairs count too: they are judgements the pipeline made,
		// and a wrong one is as damaging as a wrong model verdict.
		for _, f := range free {
			all = append(all, f.Pair)
		}
	}

	keep := map[string]bool{}
	for _, k := range opts.Kinds {
		keep[strings.ToLower(strings.TrimSpace(k))] = true
	}

	out := &PairSet{Session: sessionID}
	seen := map[string]bool{}
	for _, p := range all {
		key := p.Key()
		if seen[key] {
			continue
		}
		seen[key] = true

		v, hasEdge := said[key]
		kind := v.kind
		if !hasEdge {
			kind = "unrelated"
		}
		if len(keep) > 0 && !keep[kind] {
			continue
		}
		out.Pairs = append(out.Pairs, LabelledPair{
			Pair: key,
			A:    oneLine(p.A.Text), B: oneLine(p.B.Text),
			SourceA: p.A.Source, SourceB: p.B.Source,
			Model: kind, Why: v.why,
			// Both ends verified means the pair was reachable by the pass that ran.
			Judged: p.A.VerifiedAt != nil && p.B.VerifiedAt != nil,
		})
	}
	sort.SliceStable(out.Pairs, func(i, j int) bool { return out.Pairs[i].Pair < out.Pairs[j].Pair })
	return out, nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// LoadPairs reads a labelled set.
func LoadPairs(path string) (*PairSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ps PairSet
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	return &ps, nil
}

// WritePairs writes a set for labelling.
func WritePairs(path string, ps *PairSet) error {
	raw, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// PairScore is how the adjudicator did against the labels.
type PairScore struct {
	Labelled   int `json:"labelled"`
	Unlabelled int `json:"unlabelled"`
	// Skipped is pairs that were never put to the model.
	Skipped int `json:"skipped"`
	Correct int `json:"correct"`

	// Confusion[modelVerdict][trueLabel] is how often the model said one thing and the
	// truth was another.
	Confusion map[string]map[string]int `json:"confusion"`

	// Precision and Recall per relation. Precision is what matters for a judge that
	// over-reports: of the pairs it called X, how many were X.
	Precision map[string]float64 `json:"precision"`
	Recall    map[string]float64 `json:"recall"`
}

// ScorePairs compares the model's verdicts against the labels.
//
// Unlabelled pairs are counted and excluded, never guessed at — a score computed over
// whichever pairs someone got round to labelling, reported as the score, is how a test set
// starts lying.
func ScorePairs(ps *PairSet) PairScore {
	s := PairScore{
		Confusion: map[string]map[string]int{},
		Precision: map[string]float64{},
		Recall:    map[string]float64{},
	}
	said := map[string]int{}
	truth := map[string]int{}
	hit := map[string]int{}

	for _, p := range ps.Pairs {
		label := strings.ToLower(strings.TrimSpace(p.Label))
		if label == "" {
			s.Unlabelled++
			continue
		}
		if !p.Judged {
			// Never put to the model, so it says nothing about the judge.
			s.Skipped++
			continue
		}
		model := strings.ToLower(strings.TrimSpace(p.Model))

		s.Labelled++
		said[model]++
		truth[label]++
		if s.Confusion[model] == nil {
			s.Confusion[model] = map[string]int{}
		}
		s.Confusion[model][label]++
		if model == label {
			s.Correct++
			hit[label]++
		}
	}

	for k, n := range said {
		if n > 0 {
			s.Precision[k] = float64(hit[k]) / float64(n)
		}
	}
	for k, n := range truth {
		if n > 0 {
			s.Recall[k] = float64(hit[k]) / float64(n)
		}
	}
	return s
}

// Accuracy is the fraction of labelled, judged pairs the model got right.
func (s PairScore) Accuracy() float64 {
	if s.Labelled == 0 {
		return 0
	}
	return float64(s.Correct) / float64(s.Labelled)
}
