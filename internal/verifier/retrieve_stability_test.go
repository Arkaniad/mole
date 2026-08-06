package verifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
)

// realClaims are claims from a recorded run, kept verbatim.
//
// Synthetic text does not reproduce the defect these tests exist for: an early
// version of this fixture used tidy sentences whose IDF weights summed exactly,
// and the probe passed while the real pool was unstable. The instability needs
// realistic vocabulary overlap and the irrational weights that come out of
// idfOver's smoothed logarithm.
var realClaims = []string{
	"MambaByte maintains a fixed-sized memory state that is independent of context length, unlike Transformers whose memory scales linearly with sequence length.",
	"Byte-level modeling in MambaByte inflates sequence lengths to 4-5 times longer than subword representations, which exacerbates computational costs in conventional Transformers.",
	"MambaByte is more resilient to text corruptions and spelling errors compared to subword tokenized models.",
	"Tokenizers create encodings that represent words, subwords or characters to provide structured syntactic understanding.",
	"MambaByte training has O(Lctx) computational complexity, while even compressed models like MegaByte have O(L\u00b2ctx/p\u00b2 + Lctx\u00b7p) complexity for patch size p.",
	"MambaByte can extrapolate to sequences at least 4\u00d7 longer than the training length without significant performance degradation.",
	"Mamba provides constant memory at inference and linear time complexity for autoregressive decoding.",
	"MambaByte is a token-free state space model (SSM) designed for modeling long byte-sequences.",
	"Mamba uses a selection mechanism where B, C, and \u0394 (timestep) are defined as input-dependent functions, making SSM parameters selective based on the input.",
	"State Space Models offer a viable alternative to transformer models with fixed memory and efficient decoding mechanisms.",
	"MambaByte maintains a single hidden state per layer that evolves with time, enabling constant time per generation step, unlike Transformer models which require caching the entire context.",
	"MambaByte-972M outperforms all byte-level models and achieves competitive performance with subword models on the PG19 dataset while using only 150B bytes of training data.",
	"Tokenization introduces challenges including processing long sequences, hallucinations based on token structure, memory scaling limitations, and pre-processing overhead.",
	"An adaptation of speculative decoding with tokenized drafting and byte-level verification results in a 2.6x inference speedup for MambaByte.",
	"State space models are viable for enabling token-free language modeling.",
	"Standard autoregressive Transformers scale poorly when operating on byte sequences because effective memory required grows with sequence length.",
}

func stabilityPool() []*core.Claim {
	out := make([]*core.Claim, 0, len(realClaims))
	for i, txt := range realClaims {
		out = append(out, &core.Claim{ID: fmt.Sprintf("c_%03d", i), Text: txt})
	}
	return out
}

// TestScoringIsBitStable.
//
// cosine and norm sum floating point values, and floating point addition is not
// associative. Ranging a map to do that summation gives Go's deliberately
// randomized order, so identical inputs produced sums differing in the last bits
// — measured at five distinct values over 2000 calls before the vector became a
// sorted slice.
//
// Exact bits, not a tolerance. The consumer compares scores with ==, so "close
// enough" is precisely the property that does not hold.
func TestScoringIsBitStable(t *testing.T) {
	pool := stabilityPool()
	idf := idfOver(pool, nil)
	a := weigh(tokenize(pool[0].Text), idf)
	b := weigh(tokenize(pool[1].Text), idf)

	bits := map[string]int{}
	norms := map[string]int{}
	for i := 0; i < 3000; i++ {
		bits[fmt.Sprintf("%b", cosine(a, b))]++
		// Rebuilt each time: weigh's own map iteration must not leak into the
		// vector either.
		norms[fmt.Sprintf("%b", norm(weigh(tokenize(pool[2].Text), idf)))]++
	}
	if len(bits) != 1 {
		t.Errorf("cosine returned %d distinct values for identical inputs: %v", len(bits), keysOf(bits))
	}
	if len(norms) != 1 {
		t.Errorf("norm returned %d distinct values for identical input: %v", len(norms), keysOf(norms))
	}
}

// TestRetrievalSelectsTheSameCandidatesEveryTime.
//
// The score instability above is not cosmetic, because Candidates cuts the ranked
// list to the top N. A one-ULP difference skips the ID tiebreak, and a flip at
// the boundary changes which candidates are SELECTED — a different candidate set
// is a different pair set, a different batch count, and a different graph, from
// identical inputs. That is what makes a cassette replay stop being a regression
// gate: you cannot tell a change you made from a run that wobbled.
func TestRetrievalSelectsTheSameCandidatesEveryTime(t *testing.T) {
	pool := stabilityPool()
	var r LexicalRetriever

	fingerprints := map[string]int{}
	for iter := 0; iter < 60; iter++ {
		var fp string
		for _, target := range pool {
			got, err := r.Candidates(context.Background(), target, pool, 4)
			if err != nil {
				t.Fatal(err)
			}
			fp += target.ID + ":"
			for _, c := range got {
				fp += c.ID + ","
			}
			fp += ";"
		}
		fingerprints[fp]++
	}
	if len(fingerprints) != 1 {
		t.Errorf("retrieval produced %d distinct candidate sets over 60 identical passes",
			len(fingerprints))
	}

	// A small N is deliberate: the cut is where a reordering turns into a
	// different SET, so a cap far below the pool size is the sensitive case.
	if len(pool) <= 4 {
		t.Fatal("the pool must be larger than the cap or the cut never bites")
	}
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
