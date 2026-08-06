package eval_test

import (
	"fmt"
	"testing"

	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/verifier"
)

func judgedSet(verdicts ...string) *eval.PairSet {
	ps := &eval.PairSet{}
	for i, v := range verdicts {
		ps.Pairs = append(ps.Pairs, eval.LabelledPair{
			Pair: fmt.Sprintf("p%d", i), Model: v, Judged: true,
		})
	}
	return ps
}

// TestAgreementSeparatesInertDisagreementsFromRealOnes.
//
// The measurement that prompted this: claude-haiku-4-5 judging 37 pairs twice scored
// 84% raw, and the six disagreements read as a prompt problem. Five were
// supports/refines/unrelated shuffles, which the graph consumes identically —
// nothing derives anything from those edges. One was contradicts-vs-supports, which
// moves a confidence score and queues a research lead. Acting on 84% would have
// meant rewriting a prompt to chase six problems when there was one.
func TestAgreementSeparatesInertDisagreementsFromRealOnes(t *testing.T) {
	a := judgedSet("supports", "supports", "unrelated", "contradicts", "duplicate_of", "contradicts")
	b := judgedSet("supports", "refines", "supports", "supports", "unrelated", "contradicts")
	//              same        inert      inert       REAL         REAL          same

	ag := eval.CompareVerdicts(a, b)
	if ag.Compared != 6 {
		t.Fatalf("compared %d, want 6", ag.Compared)
	}
	if ag.Same != 2 {
		t.Errorf("Same = %d, want 2 (only the byte-identical verdicts)", ag.Same)
	}
	// The two inert shuffles join the two identical pairs; the contradicts and
	// duplicate_of flips do not.
	if ag.SameEffect != 4 {
		t.Errorf("SameEffect = %d, want 4", ag.SameEffect)
	}
	if got := ag.EffectRate(); got < 0.66 || got > 0.67 {
		t.Errorf("EffectRate = %.3f, want ~0.667", got)
	}

	// duplicate_of is NOT inert: it merges a cluster and so changes the publisher
	// count corroboration is computed from. Folding it in with supports would make
	// this measurement wrong in the direction that looks reassuring.
	if verifier.RelDuplicate.EffectOf() == verifier.RelSupports.EffectOf() {
		t.Error("duplicate_of is being treated as inert; it drives clustering")
	}

	// The reverse mistake: a judge agreeing with itself exactly must never score
	// below its raw rate.
	same := eval.CompareVerdicts(a, a)
	if same.Same != 6 || same.SameEffect != 6 {
		t.Errorf("a set compared with itself: Same=%d SameEffect=%d, want 6 and 6",
			same.Same, same.SameEffect)
	}
}

// TestEveryRelationHasADeliberateEffect.
//
// EffectOf returns inert for anything it does not name, so a relation added later
// would quietly stop counting against agreement — the exact failure this measurement
// exists to catch. Pinning all five makes that a test failure instead.
func TestEveryRelationHasADeliberateEffect(t *testing.T) {
	want := map[verifier.Relation]verifier.Effect{
		verifier.RelContradicts: verifier.EffectContradiction,
		verifier.RelDuplicate:   verifier.EffectDuplicate,
		verifier.RelSupports:    verifier.EffectInert,
		verifier.RelRefines:     verifier.EffectInert,
		verifier.RelUnrelated:   verifier.EffectInert,
	}
	for rel, eff := range want {
		if got := rel.EffectOf(); got != eff {
			t.Errorf("%s has effect %q, want %q", rel, got, eff)
		}
	}
	if len(want) != 5 {
		t.Fatal("a relation was added or removed without updating this table")
	}
}
