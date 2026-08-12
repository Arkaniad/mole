package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode, slice 4: mole retrieves the pairs, the agent judges them.
//
// The retrieval is deterministic and costs no model call, so a toolkit graph and an
// autonomous one are built over the same candidate set. What mole cannot do here is
// enforce the confirm pass — see the note on edge_add for why asking twice would be
// a ritual rather than a second judgement.

// twoClaims records a pair of claims that mention the same subject, so the lexical
// retriever puts them up as candidates.
func twoClaims(t *testing.T, r *rig) (session string, pairID string) {
	t.Helper()
	sess, doc := openWithDoc(t, r)

	for _, c := range []struct{ text, quote string }{
		{"Fasting reduced fasting glucose in adults with prediabetes.", realQuote},
		{"Fasting did not reduce fasting glucose in adults with prediabetes.",
			"Adherence was the most commonly reported limitation"},
	} {
		res := r.call(t, "mole.claim_add", map[string]any{
			"session_id": sess, "doc_id": doc, "text": c.text, "quote": c.quote}, nil)
		if res.IsError {
			t.Fatalf("claim refused: %s", errText(res))
		}
	}

	var pairs struct {
		Pairs []struct {
			PairID string `json:"pair_id"`
			A      string `json:"a"`
			B      string `json:"b"`
		} `json:"pairs"`
	}
	r.call(t, "mole.pairs_candidates", map[string]any{"session_id": sess}, &pairs)
	if len(pairs.Pairs) == 0 {
		t.Fatal("no candidate pairs for two claims about the same subject")
	}
	return sess, pairs.Pairs[0].PairID
}

// TestCandidatePairsAreOfferedWithBothTexts, since an agent cannot judge two ids.
func TestCandidatePairsAreOfferedWithBothTexts(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, _ := twoClaims(t, r)

	var pairs struct {
		Pairs []struct {
			PairID  string `json:"pair_id"`
			A       string `json:"a"`
			B       string `json:"b"`
			ASource string `json:"a_source"`
		} `json:"pairs"`
		Note string `json:"note"`
	}
	r.call(t, "mole.pairs_candidates", map[string]any{"session_id": sess}, &pairs)

	p := pairs.Pairs[0]
	if p.A == "" || p.B == "" {
		t.Error("a pair arrived without its claim texts")
	}
	if p.ASource == "" {
		t.Error("a pair arrived without its sources; the judge cannot check attribution")
	}
	if !strings.Contains(pairs.Note, "neither") {
		t.Errorf("the note does not say most pairs are neither: %q", pairs.Note)
	}
}

// TestAContradictionIsRecorded.
func TestAContradictionIsRecorded(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)

	var added struct {
		EdgeID string `json:"edge_id"`
		Note   string `json:"note"`
	}
	res := r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": pairID, "relation": "contradicts",
		"rationale": "one says reduced, the other says not reduced, same population",
	}, &added)
	if res.IsError {
		t.Fatalf("a contradiction was refused: %s", errText(res))
	}
	if added.EdgeID == "" {
		t.Fatal("no edge id")
	}

	var edges []*core.ClaimEdge
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		edges, err = q.ListEdges(ctx, sess, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Kind != core.EdgeContradicts {
		t.Fatalf("edges = %+v, want one contradiction", edges)
	}
	if edges[0].Rationale == "" {
		t.Error("the rationale was dropped; a person inspecting the graph gets no reason")
	}
}

// TestNeitherWritesNoEdge.
//
// "neither" is the right answer for most pairs, and mole's graph stores only the
// relations something downstream acts on. Accepting the call silently would leave a
// caller unsure whether anything happened.
func TestNeitherWritesNoEdge(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)

	var added struct {
		EdgeID string `json:"edge_id"`
		Note   string `json:"note"`
	}
	r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": pairID, "relation": "neither"}, &added)

	if added.EdgeID != "" {
		t.Error("neither wrote an edge")
	}
	if !strings.Contains(added.Note, "no edge") {
		t.Errorf("the caller is not told nothing was written: %q", added.Note)
	}
}

// TestARetiredRelationIsNormalisedRatherThanRefused.
//
// A model answering "supports" out of habit has still made a real judgement, and
// discarding it over vocabulary would lose the judgement — the same rule the
// verifier applies at its own boundary.
func TestARetiredRelationIsNormalisedRatherThanRefused(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)

	var added struct {
		Relation string `json:"relation"`
		EdgeID   string `json:"edge_id"`
	}
	res := r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": pairID, "relation": "supports"}, &added)
	if res.IsError {
		t.Fatalf("a retired relation was refused outright: %s", errText(res))
	}
	if added.Relation != "neither" {
		t.Errorf("relation = %q, want it folded onto neither", added.Relation)
	}
}

// TestAnInventedRelationIsRefused, since a verdict nothing can interpret is not a
// verdict.
func TestAnInventedRelationIsRefused(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)

	res := r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": pairID, "relation": "sort-of-disagrees"}, nil)
	if !res.IsError {
		t.Fatal("an invented relation was accepted")
	}
}

// TestAnEdgeCannotReferenceAnotherSessionsClaims, or the graph has edges nothing in
// the session explains.
func TestAnEdgeCannotReferenceAnotherSessionsClaims(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	_, pairA := twoClaims(t, r)
	sessB, _ := twoClaims(t, r)

	res := r.call(t, "mole.edge_add", map[string]any{
		"session_id": sessB, "pair_id": pairA, "relation": "contradicts"}, nil)
	if !res.IsError {
		t.Fatal("an edge was written between another session's claims")
	}
	if !strings.Contains(errText(res), "not in session") {
		t.Errorf("the refusal does not say why: %s", errText(res))
	}
}

// TestAClaimCannotContradictItself is a property, not a unit test on one check.
//
// Falsifying it found two layers refusing: deleting the tool's own check leaves
// the store's (`sqlite/queries.go:1157`), which has rejected self-edges since a
// claim corroborating itself was found inflating its own confidence. The tool
// keeps its check anyway — it fails with a sentence a model can act on rather
// than a sqlite error — but this test passes as long as either layer holds.
func TestAClaimCannotContradictItself(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)
	first := strings.SplitN(pairID, "|", 2)[0]

	res := r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": first + "|" + first, "relation": "contradicts"}, nil)
	if !res.IsError {
		t.Fatal("a claim was recorded as contradicting itself")
	}
	// The message is what the tool's own check adds over the store's: without it
	// the refusal is "sqlite: edge 1/1 is a self-edge on clm_...", which tells a
	// model nothing it can act on. Asserting it keeps the tool layer falsifiable.
	if !strings.Contains(errText(res), "cannot relate to itself") {
		t.Errorf("the refusal reads like a database error: %s", errText(res))
	}
}

// TestTheMeasurementIsInTheToolDescription.
//
// mole cannot enforce the confirm pass here — it cannot tell an independent second
// judgement from the same assertion repeated. The 51%-versus-70% finding is
// therefore advice, and advice only helps if it reaches the model.
func TestTheMeasurementIsInTheToolDescription(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	for _, tool := range r.toolDefs(t) {
		if tool.Name != "mole.edge_add" {
			continue
		}
		// The digits, not the sentence around them: the description will be
		// reworded, and what must survive rewording is the measurement itself.
		for _, want := range []string{"51", "70"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("edge_add's description omits %q: %s", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("mole.edge_add is not registered")
}

// TestAToolkitEdgeIsScoredByEval is the point of the slice.
//
// An agent's judgements are only worth exposing if they land in the same graph
// everything else reads — a parallel toolkit graph that `mole eval` could not see
// would make the mode unmeasurable, which is the one thing toolkit mode was sold
// on keeping.
func TestAToolkitEdgeIsScoredByEval(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, pairID := twoClaims(t, r)

	r.call(t, "mole.edge_add", map[string]any{
		"session_id": sess, "pair_id": pairID, "relation": "contradicts"}, nil)

	card, err := eval.Score(context.Background(), r.db, sess, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range card.Metrics {
		if m.Name != "disagreement rate" {
			continue
		}
		if m.Value == 0 {
			t.Fatalf("eval sees no disagreement in a session with a contradiction: %s", m.Detail)
		}
		return
	}
	t.Fatal("eval reported no disagreement rate at all")
}
