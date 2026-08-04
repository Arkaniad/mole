package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
)

// realRunClaims are the thirteen claims a live run produced against a local
// qwen2.5:3b, verbatim from the database.
//
// Fixtures rather than invented pairs, because the pairs that matter here are the
// ones a real extractor actually emits. Two are load-bearing:
//
//	c_01 / c_03 — the same finding twice from ONE source, which is what makes a
//	              report restate itself.
//	c_04 / c_11 — the same finding from TWO sources, worded differently. This is
//	              corroboration, it spans different leads, and it is the case
//	              §11.1 says rev 1 structurally could not see.
var realRunClaims = []string{
	/* 00 */ "MambaByte achieves better performance faster compared to Transformers.",
	/* 01 */ "MambaByte is competitive with and even outperforms state-of-the-art subword Transformers.",
	/* 02 */ "MambaByte benefits from linear scaling in length compared to Transformers.",
	/* 03 */ "Compared to existing subword models, MambaByte is competitive and even outperforms them in some cases.",
	/* 04 */ "MambaByte operates directly on raw byte sequences without any intermediate tokenization step.",
	/* 05 */ "MambaByte demonstrates strong ability to extrapolate to sequences much longer than those it was trained on.",
	/* 06 */ "MambaByte consumes less computational resources compared to previous Transformer-based models like MegaByte when normalized.",
	/* 07 */ "Mamba Byte's speculative decoding approach results in a 2.6× inference speedup compared to the standard MambaByte implementation.",
	/* 08 */ "MambaByte compares favorably to various subword baselines in terms of performance while handling significantly longer sequences.",
	/* 09 */ "Using the Mamba architecture, which is simpler than MegaByte and can handle longer sequences without patching issues, improves both compute efficiency and model performance.",
	/* 10 */ "MambaByte Model is a token-free byte-level language model that leverages input-dependent selective SSM layers for efficient next-byte prediction and robust inference.",
	/* 11 */ "It removes subword tokenization by operating directly on UTF-8 bytes, enabling fixed memory usage and mitigating quadratic scaling challenges of transformers.",
	/* 12 */ "To enable resuming during the parallel scan, we extended the fast CUDA kernel, allowing verification to restart from the mismatched position instead of beginning from the start.",
}

func realPool() []*core.Claim {
	out := make([]*core.Claim, 0, len(realRunClaims))
	for i, text := range realRunClaims {
		out = append(out, &core.Claim{ID: fmt.Sprintf("c_%02d", i), Text: text})
	}
	return out
}

func topCandidate(t *testing.T, target *core.Claim, pool []*core.Claim) *core.Claim {
	t.Helper()
	got, err := LexicalRetriever{}.Candidates(context.Background(), target, pool, DefaultMaxCandidates)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) == 0 {
		return nil
	}
	return got[0]
}

// TestRealDuplicatesRankFirst is the whole point of the retriever.
//
// Both pairs come from one live run. The cross-source pair is the harder and the
// more important: the two claims share almost no wording ("raw byte sequences
// without any intermediate tokenization" against "removes subword tokenization by
// operating directly on UTF-8 bytes"), and it is corroboration from two publishers
// rather than one page repeating itself.
func TestRealDuplicatesRankFirst(t *testing.T) {
	pool := realPool()
	byID := map[string]*core.Claim{}
	for _, c := range pool {
		byID[c.ID] = c
	}

	pairs := [][2]string{
		{"c_01", "c_03"}, // same source, near-identical wording
		{"c_03", "c_01"}, // and symmetrically
		{"c_04", "c_11"}, // different sources, different wording
		{"c_11", "c_04"},
	}
	for _, p := range pairs {
		got := topCandidate(t, byID[p[0]], pool)
		if got == nil {
			t.Errorf("%s retrieved nothing; %s should be its top candidate", p[0], p[1])
			continue
		}
		if got.ID != p[1] {
			t.Errorf("%s: top candidate is %s (%.50q), want %s", p[0], got.ID, got.Text, p[1])
		}
	}
}

// TestTopicWordsDoNotRankEverything is why the score is IDF-weighted.
//
// Constructed so overlap COUNT and IDF disagree, because otherwise the test proves
// nothing: the first version compared a claim sharing one common word against one
// sharing two rare words, and the two-word claim won on arithmetic alone. Flat
// weights passed it.
//
// Here the distractor shares TWO words with the target and the right answer shares
// ONE. Every claim in the pool contains "MambaByte" and "Transformer", so those two
// carry no information about which claims relate; "perplexity" appears twice and
// carries most of it. Count says distractor, IDF says the rare term.
func TestTopicWordsDoNotRankEverything(t *testing.T) {
	pool := []*core.Claim{
		{ID: "c_target", Text: "MambaByte outperforms Transformer baselines on perplexity."},
		// Two shared words, both ubiquitous.
		{ID: "c_common", Text: "MambaByte replaces Transformer attention."},
		// One shared word, and it is the rare one.
		{ID: "c_rare", Text: "Perplexity was the chosen metric."},
	}
	// Padding that makes "MambaByte" and "Transformer" genuinely ubiquitous, which
	// is the situation every real session is in: the topic is in every claim.
	for i := 0; i < 8; i++ {
		pool = append(pool, &core.Claim{
			ID:   fmt.Sprintf("c_pad%d", i),
			Text: fmt.Sprintf("MambaByte differs from Transformer designs in aspect %d.", i),
		})
	}

	got := topCandidate(t, pool[0], pool)
	if got == nil {
		t.Fatal("retrieved nothing")
	}
	if got.ID != "c_rare" {
		t.Errorf("top candidate is %s (%.50q), want c_rare: the distractor shares more "+
			"words but only ubiquitous ones, so it is being ranked by topic",
			got.ID, got.Text)
	}
}

// TestInflectionsDoNotHideAMatch: claim text about one fact rarely agrees on tense
// or number, so an unstemmed retriever misses the pair outright. From the real run:
// c_03 says "Compared to existing subword models", c_08 says "compares favorably".
func TestInflectionsDoNotHideAMatch(t *testing.T) {
	cases := [][2]string{
		{"compares", "compared"},
		{"model", "models"},
		{"outperform", "outperforms"},
		{"sequence", "sequences"},
		{"tokenize", "tokenizes"},
		{"scaling", "scale"},
	}
	for _, c := range cases {
		a, b := tokenize(c[0]), tokenize(c[1])
		if len(a) == 0 || len(b) == 0 {
			t.Errorf("%q/%q: one side tokenized to nothing", c[0], c[1])
			continue
		}
		if a[0] != b[0] {
			t.Errorf("%q -> %q but %q -> %q; the pair will never be compared",
				c[0], a[0], c[1], b[0])
		}
	}
}

// TestNumbersSurviveTokenization. A shared figure is among the strongest signals
// two claims are about the same fact, and stemming or splitting one destroys it:
// "1.31" must not become "1" and "31", and "2.6×" must not lose its decimal.
func TestNumbersSurviveTokenization(t *testing.T) {
	got := tokenize("MambaByte reports 1.31 bits per byte and a 2.6× speedup on PG-19.")
	joined := strings.Join(got, " ")
	for _, want := range []string{"1.31", "2.6", "19"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q lost from %v", want, got)
		}
	}
	// Split apart rather than kept whole is the failure that matters: "1" and "31"
	// match every other claim containing a 1.
	for _, bad := range []string{" 1 ", " 31 "} {
		if strings.Contains(" "+joined+" ", bad) {
			t.Errorf("a decimal was split apart: %v", got)
		}
	}
}

// TestAClaimAboutSomethingElseRetrievesNothing.
//
// c_12 is about resuming a CUDA kernel during a parallel scan. Nothing else in the
// session shares a content word with it, and returning candidates anyway would buy
// a model call per claim to be told "unrelated" — the cost the cap exists to bound
// and the retriever exists to avoid.
//
// It is also an honest limit: c_07 mentions speculative decoding and c_12 describes
// its kernel, a relation no shared wording expresses. That pair is what an
// embedding retriever would buy, and this test records the miss rather than hiding
// it.
func TestAClaimAboutSomethingElseRetrievesNothing(t *testing.T) {
	pool := realPool()
	got, err := LexicalRetriever{}.Candidates(context.Background(), pool[12], pool, DefaultMaxCandidates)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) != 0 {
		for _, c := range got {
			t.Logf("  %s %.60q", c.ID, c.Text)
		}
		t.Errorf("%d candidates for a claim sharing no content word with any other", len(got))
	}
}

// TestRetrievalIgnoresPoolOrder is what the ID tiebreak buys.
//
// The candidate list decides which pairs reach the model, so a list that depends
// on the order claims came back in makes a cassette replay miss — and §14.1's
// whole argument is that eval is free because it replays. Ties are not
// hypothetical: ListClaims orders by created_at, and a batch of claims from one
// actor run is inserted in the same transaction.
//
// The first version of this test ran the same pool five times and passed with the
// tiebreak removed, because sort.SliceStable preserves input order and the input
// was a fixed slice. Reversing the pool is what makes the tie visible.
func TestRetrievalIgnoresPoolOrder(t *testing.T) {
	// Two claims with identical content words, so their scores tie exactly.
	pool := []*core.Claim{
		{ID: "c_target", Text: "Subword tokenization is removed entirely."},
		{ID: "c_aaa", Text: "Tokenization of subwords was removed."},
		{ID: "c_zzz", Text: "Removed: the tokenization of subwords."},
		{ID: "c_other", Text: "Byte-level scaling is linear in length."},
	}
	reversed := make([]*core.Claim, len(pool))
	for i, c := range pool {
		reversed[len(pool)-1-i] = c
	}

	ids := func(in []*core.Claim) string {
		got, err := LexicalRetriever{}.Candidates(context.Background(), in[0], in, 4)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range got {
			out = append(out, c.ID)
		}
		return strings.Join(out, ",")
	}

	forward := ids(pool)
	if forward == "" {
		t.Fatal("retrieved nothing, so ordering was never exercised")
	}
	// Same target, same claims, opposite order. reversed[0] is not the target, so
	// pass the pool in reverse but keep the target explicit.
	got, err := LexicalRetriever{}.Candidates(context.Background(), pool[0], reversed, 4)
	if err != nil {
		t.Fatal(err)
	}
	var back []string
	for _, c := range got {
		back = append(back, c.ID)
	}
	if strings.Join(back, ",") != forward {
		t.Errorf("pool order changed the candidate list: %v vs %s", back, forward)
	}
	// And the tied pair must be ordered by ID, not by chance.
	if !strings.Contains(forward, "c_aaa,c_zzz") {
		t.Errorf("tied candidates are not ID-ordered: %s", forward)
	}
}

// TestLongClaimsDoNotDominate is why the score is a cosine rather than a sum.
//
// A claim mentioning twenty things shares more words with everything than a claim
// mentioning two, so an unnormalized score ranks by length: the verbose claim wins
// every candidate list and the precise near-duplicate is pushed out. What matters
// is the PROPORTION of each claim's content that overlaps, not the count.
func TestLongClaimsDoNotDominate(t *testing.T) {
	pool := []*core.Claim{
		{ID: "c_target", Text: "Subword tokenization is removed."},
		// Shares both content words and nothing else: almost the same claim.
		{ID: "c_precise", Text: "Tokenization of subwords is eliminated."},
		// Shares all three, plus a paragraph of unrelated material. Higher raw
		// overlap, far lower proportion.
		{ID: "c_verbose", Text: "Subword tokenization is removed in this architecture, " +
			"which also introduces selective state space layers, linear attention scaling, " +
			"a fast CUDA kernel, speculative decoding, fixed memory usage, patch-free " +
			"handling of long documents, and improved perplexity on the PG-19 corpus."},
		{ID: "c_pad1", Text: "Byte-level scaling is linear in length."},
		{ID: "c_pad2", Text: "The PG-19 corpus measures long-document perplexity."},
	}

	got := topCandidate(t, pool[0], pool)
	if got == nil {
		t.Fatal("retrieved nothing")
	}
	if got.ID != "c_precise" {
		t.Errorf("top candidate is %s, want c_precise: a verbose claim outranked a "+
			"near-duplicate because it shared more words in absolute terms", got.ID)
	}
}

// TestTargetIsNeverItsOwnCandidate. A claim compared against itself is a guaranteed
// duplicate_of edge, which InsertEdges rejects as a self-edge — after paying for
// the call that produced it.
func TestTargetIsNeverItsOwnCandidate(t *testing.T) {
	pool := realPool()
	for _, target := range pool {
		got, err := LexicalRetriever{}.Candidates(context.Background(), target, pool, len(pool))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.ID == target.ID {
				t.Fatalf("%s was returned as its own candidate", target.ID)
			}
		}
	}
}

// TestCapBoundsTheBill. Every candidate is a model call, so the cap is the only
// thing standing between a large session and a verification bill larger than the
// research it is verifying.
func TestCapBoundsTheBill(t *testing.T) {
	// A pool where everything relates to everything.
	var pool []*core.Claim
	for i := 0; i < 40; i++ {
		pool = append(pool, &core.Claim{
			ID:   fmt.Sprintf("c_%02d", i),
			Text: fmt.Sprintf("Subword tokenization affects byte-level scaling in variant %d.", i),
		})
	}
	for _, max := range []int{1, 3, 8} {
		got, err := LexicalRetriever{}.Candidates(context.Background(), pool[0], pool, max)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != max {
			t.Errorf("max=%d returned %d candidates", max, len(got))
		}
	}
}
