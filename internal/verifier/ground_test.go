package verifier_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/verifier"
)

// fakeFetch serves pages by URL, or an error.
type fakeFetch struct {
	pages map[string]string
	err   error
	calls int
}

func (f *fakeFetch) Fetch(_ context.Context, rawURL string) (*fetch.Result, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.pages[rawURL]
	if !ok {
		return &fetch.Result{URL: rawURL, Outcome: fetch.OutcomeNotFound, StatusCode: 404}, nil
	}
	return &fetch.Result{
		URL: rawURL, Outcome: fetch.OutcomeOK, StatusCode: 200,
		ContentType: "text/plain", Content: []byte(body),
	}, nil
}

// passthroughExtract returns the bytes as text, so a test controls the exact
// document the grounding check reads.
type passthroughExtract struct{}

func (passthroughExtract) Extract(_ context.Context, content []byte, _ string, _ *url.URL) (*extract.Document, error) {
	return &extract.Document{Text: string(content)}, nil
}

const groundedQuote = "MambaByte reaches 1.31 bits per byte on the PG-19 benchmark at 350M parameters"

func pageContaining(quote string) string {
	return "Introduction. Prior work established the baseline. " + quote +
		". The remainder of the paper discusses limitations at greater length."
}

// groundRig builds a session with claims and a Verifier wired for grounding.
func groundRig(t *testing.T, claims []core.Claim, pages map[string]string, judge func(prompt string) (string, error)) *rig {
	t.Helper()
	r := newRig(t, 4_000_000, claims, func(_ int, prompt string) (string, error) {
		return judge(prompt)
	})
	r.v.Grounder = &verifier.Grounder{
		Fetch:   &fakeFetch{pages: pages},
		Extract: passthroughExtract{},
	}
	return r
}

func alwaysSupported() func(string) (string, error) {
	return func(string) (string, error) { return `{"supported":true,"why":"the passage asserts it"}`, nil }
}

// judgeReply adapts a prompt-only judge to newRig's (call, prompt) signature.
func judgeReply(f func(string) (string, error)) func(int, string) (string, error) {
	return func(_ int, prompt string) (string, error) { return f(prompt) }
}

// TestAQuoteThatDoesNotSupportItsClaimIsCaught is the failure §11.5 exists for.
//
// The quote is present, verbatim, exactly as extraction verified it — and it does
// not support the claim. Mechanism 1 cannot see this at all: quote-mining passes a
// verbatim check by construction. It is what the model call buys.
func TestAQuoteThatDoesNotSupportItsClaimIsCaught(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	claims := []core.Claim{{
		Text:   "MambaByte outperforms every subword model on every benchmark.",
		Source: src, Quote: groundedQuote,
	}}
	r := groundRig(t, claims, map[string]string{src: pageContaining(groundedQuote)},
		func(string) (string, error) {
			return `{"supported":false,"why":"the passage reports one benchmark at one size"}`, nil
		})

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if rep.Unsupported != 1 {
		t.Fatalf("Unsupported = %d, want 1 (report: %+v)", rep.Unsupported, rep)
	}

	stored := r.claims(t)["MambaByte outperforms every subword model on every benchmark."]
	if stored.Grounded == nil || *stored.Grounded {
		t.Errorf("Grounded = %v, want false", stored.Grounded)
	}
	if !strings.Contains(stored.GroundingNote, "does NOT support") {
		t.Errorf("note does not record the verdict: %q", stored.GroundingNote)
	}
	// And §11.3's penalty must have landed: a claim its own source does not support
	// should score below one nobody checked.
	unchecked := groundRig(t, []core.Claim{{
		Text: "An unchecked claim.", Source: src, Quote: groundedQuote,
	}}, nil, alwaysSupported())
	if _, err := unchecked.v.Run(context.Background(), unchecked.sess.ID); err != nil {
		t.Fatal(err)
	}
	base := unchecked.claims(t)["An unchecked claim."].Confidence
	if stored.Confidence >= base {
		t.Errorf("an unsupported claim scores %.4f, no lower than an unchecked one (%.4f) "+
			"— confidence was not re-derived after grounding",
			stored.Confidence, base)
	}
}

// TestAConfirmedQuoteRaisesConfidence, so the check that was paid for is worth
// something.
func TestAConfirmedQuoteRaisesConfidence(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	claims := []core.Claim{{
		Text:   "MambaByte reaches 1.31 bits per byte on PG-19 at 350M parameters.",
		Source: src, Quote: groundedQuote,
	}}
	r := groundRig(t, claims, map[string]string{src: pageContaining(groundedQuote)},
		alwaysSupported())

	// Score it once without grounding, to have something to compare against.
	if _, err := r.v.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatal(err)
	}
	before := r.claims(t)["MambaByte reaches 1.31 bits per byte on PG-19 at 350M parameters."].Confidence

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if rep.Confirmed != 1 {
		t.Fatalf("Confirmed = %d, want 1 (%+v)", rep.Confirmed, rep)
	}
	after := r.claims(t)["MambaByte reaches 1.31 bits per byte on PG-19 at 350M parameters."]
	if after.Grounded == nil || !*after.Grounded {
		t.Errorf("Grounded = %v, want true", after.Grounded)
	}
	if after.Confidence <= before {
		t.Errorf("confidence %.4f after a confirmed check, %.4f before", after.Confidence, before)
	}
}

// TestAVanishedQuoteIsNotEvidenceAgainstTheClaim.
//
// The quote WAS verified verbatim against the fetched text at extraction time
// (§11.5 mechanism 1). Its absence now means the page changed. Scoring that as "the
// evidence does not support the claim" would penalize a claim near-fatally for a
// publisher's edit — and would make every site that reorganizes look like a site
// full of fabrications.
func TestAVanishedQuoteIsNotEvidenceAgainstTheClaim(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	claims := []core.Claim{{
		Text: "MambaByte reaches 1.31 bits per byte on PG-19.", Source: src, Quote: groundedQuote,
	}}
	// The page no longer contains the quote.
	r := groundRig(t, claims, map[string]string{src: "This page has been rewritten entirely."},
		func(string) (string, error) {
			return "", errors.New("the judge must not be called for a vanished quote")
		})

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if rep.Vanished != 1 {
		t.Fatalf("Vanished = %d, want 1 (%+v)", rep.Vanished, rep)
	}
	if rep.Calls != 0 {
		t.Errorf("%d model call(s) for a quote that is not there", rep.Calls)
	}

	stored := r.claims(t)["MambaByte reaches 1.31 bits per byte on PG-19."]
	if stored.Grounded != nil {
		t.Errorf("Grounded = %v, want nil: the page changed, which says nothing about "+
			"whether the quote supported the claim", stored.Grounded)
	}
	if !strings.Contains(stored.GroundingNote, "no longer present") {
		t.Errorf("the note does not tell a reader what happened: %q", stored.GroundingNote)
	}
}

// TestAnUnreachableSourceIsNotEvidenceEither. A host being down is not a claim being
// wrong, and a run that penalized claims for network weather would rank by uptime.
func TestAnUnreachableSourceIsNotEvidenceEither(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	claims := []core.Claim{{
		Text: "A claim from an unreachable page.", Source: src, Quote: groundedQuote,
	}}
	r := groundRig(t, claims, nil, alwaysSupported())
	r.v.Grounder = &verifier.Grounder{
		Fetch:   &fakeFetch{err: errors.New("dial tcp: connection refused")},
		Extract: passthroughExtract{},
	}

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if rep.Unreachable != 1 {
		t.Fatalf("Unreachable = %d, want 1 (%+v)", rep.Unreachable, rep)
	}
	stored := r.claims(t)["A claim from an unreachable page."]
	if stored.Grounded != nil {
		t.Errorf("Grounded = %v, want nil for an unreachable source", stored.Grounded)
	}
	if stored.GroundingNote == "" {
		t.Error("nothing recorded, so an unreachable source is indistinguishable from an unchecked claim")
	}
}

// TestAJudgeThatWillNotAnswerIsNotAVerdict.
//
// `supported` is parsed as a pointer for exactly this: a response that omits the
// field must be UNDECIDED, not false. Defaulting to false would take §11.3's
// near-fatal penalty every time the judge failed, punishing claims for the checker.
func TestAJudgeThatWillNotAnswerIsNotAVerdict(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	for name, reply := range map[string]string{
		"prose":          "I would rather not say.",
		"missing field":  `{"why":"unclear"}`,
		"empty":          "",
		"malformed json": `{"supported":`,
	} {
		claims := []core.Claim{{
			Text: "A claim awaiting judgement.", Source: src, Quote: groundedQuote,
		}}
		r := groundRig(t, claims, map[string]string{src: pageContaining(groundedQuote)},
			func(string) (string, error) { return reply, nil })

		rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
		if err != nil {
			t.Fatalf("%s: ground: %v", name, err)
		}
		if rep.Undecided != 1 {
			t.Errorf("%s: Undecided = %d, want 1 (%+v)", name, rep.Undecided, rep)
		}
		stored := r.claims(t)["A claim awaiting judgement."]
		if stored.Grounded != nil {
			t.Errorf("%s: Grounded = %v, want nil — the judge failed, not the claim",
				name, stored.Grounded)
		}
	}
}

// TestGroundingIsBoundedByBothCaps. Grounding is the most expensive check in the
// system — a fetch, an extraction and a model call each — and it spends the money the
// report needs. A pass that grounds everything produces well-checked claims and no
// report to put them in.
func TestGroundingIsBoundedByBothCaps(t *testing.T) {
	pages := map[string]string{}
	var claims []core.Claim
	for i := 0; i < 12; i++ {
		src := fmt.Sprintf("https://s%02d.example/p", i)
		pages[src] = pageContaining(groundedQuote)
		claims = append(claims, core.Claim{
			Text:   fmt.Sprintf("Distinct finding number %d about tokenization.", i),
			Source: src, Quote: groundedQuote,
		})
	}

	t.Run("check count", func(t *testing.T) {
		r := groundRig(t, claims, pages, alwaysSupported())
		r.v.MaxGroundChecks = 3
		rep, err := r.v.Ground(context.Background(), r.sess.ID, 4_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Checked != 3 {
			t.Errorf("Checked = %d against a cap of 3", rep.Checked)
		}
		if rep.Skipped == 0 {
			t.Error("nothing reported as skipped, so a reader cannot tell coverage was partial")
		}
	})

	t.Run("spend allowance", func(t *testing.T) {
		r := groundRig(t, claims, pages, alwaysSupported())
		r.v.MaxGroundChecks = 12
		// Enough for one judge call, not twelve. The gate is on the ESTIMATE — a
		// hold has to cover the call before it is made (§8.2) — so this is sized
		// against groundCallEstimate rather than against the fake's tiny real cost.
		allowance := int64(5_000)
		rep, err := r.v.Ground(context.Background(), r.sess.ID, allowance)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Spent > allowance {
			t.Errorf("spent %d against an allowance of %d", rep.Spent, allowance)
		}
		if rep.Checked >= 12 {
			t.Errorf("checked %d claims; the allowance never bound", rep.Checked)
		}
		if rep.Degraded == "" {
			t.Error("stopped short without saying why")
		}
		if rep.Skipped == 0 {
			t.Error("claims the allowance could not cover were not reported as skipped")
		}
	})

	t.Run("zero allowance does nothing", func(t *testing.T) {
		r := groundRig(t, claims, pages, alwaysSupported())
		rep, err := r.v.Ground(context.Background(), r.sess.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Checked != 0 || rep.Calls != 0 {
			t.Errorf("a zero allowance checked %d claims in %d calls", rep.Checked, rep.Calls)
		}
	})
}

// TestOneSourceIsFetchedOnceForAllItsClaims. The common case on a real run: one
// arXiv page produced seven of thirteen claims. Re-fetching per claim would pay
// seven times for one document.
func TestOneSourceIsFetchedOnceForAllItsClaims(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	var claims []core.Claim
	for i := 0; i < 4; i++ {
		claims = append(claims, core.Claim{
			Text:   fmt.Sprintf("Distinct finding %d about byte-level scaling.", i),
			Source: src, Quote: groundedQuote,
		})
	}
	ff := &fakeFetch{pages: map[string]string{src: pageContaining(groundedQuote)}}
	r := groundRig(t, claims, nil, alwaysSupported())
	r.v.Grounder = &verifier.Grounder{Fetch: ff, Extract: passthroughExtract{}}
	r.v.MaxGroundChecks = 4

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 4 {
		t.Fatalf("Checked = %d, want 4", rep.Checked)
	}
	if ff.calls != 1 {
		t.Errorf("%d fetches for four claims citing one source", ff.calls)
	}
	if rep.Fetches != 1 {
		t.Errorf("reported %d fetches, want 1", rep.Fetches)
	}
}

// TestContradictedClaimsAreCheckedFirst. §11.5's priority: the report has to say
// something about a disagreement, and knowing which side its own source supports is
// the cheapest way to resolve one.
func TestContradictedClaimsAreCheckedFirst(t *testing.T) {
	ctx := context.Background()
	pages := map[string]string{}
	var claims []core.Claim
	for i := 0; i < 6; i++ {
		src := fmt.Sprintf("https://s%02d.example/p", i)
		pages[src] = pageContaining(groundedQuote)
		claims = append(claims, core.Claim{
			Text:   fmt.Sprintf("Tokenization finding number %d.", i),
			Source: src, Quote: groundedQuote,
		})
	}
	r := groundRig(t, claims, pages, alwaysSupported())

	// Record a contradiction between two of them, and nothing else.
	stored := r.claims(t)
	a := stored["Tokenization finding number 4."]
	b := stored["Tokenization finding number 5."]
	if err := r.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertEdges(ctx, []core.ClaimEdge{{
			SessionID: r.sess.ID, FromID: a.ID, ToID: b.ID,
			Kind: core.EdgeContradicts, Weight: 1,
		}})
	}); err != nil {
		t.Fatal(err)
	}

	r.v.MaxGroundChecks = 2
	rep, err := r.v.Ground(ctx, r.sess.ID, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 2 {
		t.Fatalf("Checked = %d, want 2", rep.Checked)
	}
	got := map[string]bool{}
	for _, res := range rep.Results {
		got[res.ClaimID] = true
	}
	if !got[a.ID] || !got[b.ID] {
		t.Errorf("the two contradicting claims were not the ones checked; got %v", rep.Results)
	}
}

// TestAlreadyCheckedClaimsAreNotRechecked. The answer does not change, and the
// budget is better spent on a claim nobody has read.
func TestAlreadyCheckedClaimsAreNotRechecked(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	claims := []core.Claim{
		{Text: "First distinct finding about scaling.", Source: src, Quote: groundedQuote},
		{Text: "Second distinct finding about throughput.", Source: src, Quote: groundedQuote},
	}
	r := groundRig(t, claims, map[string]string{src: pageContaining(groundedQuote)},
		alwaysSupported())
	r.v.MaxGroundChecks = 1

	first, err := r.v.Ground(context.Background(), r.sess.ID, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 {
		t.Fatalf("first pass checked %d", first.Checked)
	}
	firstID := first.Results[0].ClaimID

	second, err := r.v.Ground(context.Background(), r.sess.ID, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if second.Checked != 1 {
		t.Fatalf("second pass checked %d", second.Checked)
	}
	if second.Results[0].ClaimID == firstID {
		t.Error("the second pass re-checked the claim the first one already did")
	}
}

// TestNoGrounderIsASupportedConfiguration. Claims keep the verbatim quote checked at
// extraction time, and Grounded stays nil rather than being guessed.
func TestNoGrounderIsASupportedConfiguration(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	r := newRig(t, 4_000_000, []core.Claim{
		{Text: "A claim.", Source: src, Quote: groundedQuote},
	}, judgeReply(alwaysSupported()))
	r.v.Grounder = nil

	rep, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000)
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if rep.Checked != 0 {
		t.Errorf("checked %d claims with no grounder", rep.Checked)
	}
	if rep.Degraded == "" {
		t.Error("skipped silently; a reader cannot tell this from a pass that found nothing")
	}
	if c := r.claims(t)["A claim."]; c.Grounded != nil {
		t.Errorf("Grounded = %v with no grounder configured", c.Grounded)
	}
}

// TestGroundingSpendIsAttributedToTheVerifier, so `mole trace` separates what
// checking cost from what finding cost.
func TestGroundingSpendIsAttributedToTheVerifier(t *testing.T) {
	const src = "https://arxiv.example/abs/1"
	r := groundRig(t, []core.Claim{
		{Text: "A claim to check.", Source: src, Quote: groundedQuote},
	}, map[string]string{src: pageContaining(groundedQuote)}, alwaysSupported())

	if _, err := r.v.Ground(context.Background(), r.sess.ID, 1_000_000); err != nil {
		t.Fatal(err)
	}

	var byRole map[core.Role]core.Cost
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		byRole, err = q.SumCostsByRole(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c, ok := byRole[core.RoleVerifier]; !ok || c.TotalTokens() == 0 {
		t.Errorf("no verifier spend recorded; byRole = %v", byRole)
	}

	// And nothing is left held on any path.
	after := r.reload(t)
	if after.Held != 0 {
		t.Errorf("%d still held after grounding", after.Held)
	}
	v, err := r.led.Verify(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Consistent() {
		t.Errorf("ledger drift after grounding: spent recorded=%d ledger=%d",
			v.SpentRecorded, v.SpentFromLedger)
	}
}
