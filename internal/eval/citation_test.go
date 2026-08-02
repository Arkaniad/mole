package eval_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
)

// fakeReader serves fixed text per URL, standing in for a cassette replay.
type fakeReader struct {
	pages map[string]string
	reads map[string]int
}

func (f *fakeReader) Text(ctx context.Context, rawURL string) (string, error) {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[rawURL]++
	if t, ok := f.pages[rawURL]; ok {
		return t, nil
	}
	return "", fmt.Errorf("no such page")
}

const paperText = "MambaByte is a token-free selective state space model. " +
	"It achieves 1.31 bits per byte on the PG-19 benchmark at 350M parameters. " +
	"The authors note that byte-level modelling removes tokenizer bias entirely."

const blogText = "Everything you need to know about LLM cost tracking in 2026. " +
	"Cut your spend by routing between models and caching aggressively."

func claimAt(text, source, quote string, offset int64) core.Claim {
	return core.Claim{Text: text, Source: source, Quote: quote, QuoteOffset: offset, Confidence: 0.8}
}

// TestQuoteAttributedToTheWrongSourceIsCaught is the failure this metric
// exists for. The actor's own check (§11.5) compares a quote against the chunk
// it came from, so a quote lifted from source A and stored against source B
// passes it — and produces a perfectly plausible citation pointing at a page
// that never said it.
func TestQuoteAttributedToTheWrongSourceIsCaught(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)

	quote := "It achieves 1.31 bits per byte on the PG-19 benchmark"
	f.addClaims(t,
		// Correctly attributed.
		claimAt("MambaByte reports 1.31 BPB.", "https://paper.example/x", quote,
			int64(strings.Index(paperText, quote))),
		// Same quote, wrong page.
		claimAt("MambaByte reports 1.31 BPB.", "https://blog.example/y", quote, 0),
	)

	reader := &fakeReader{pages: map[string]string{
		"https://paper.example/x": paperText,
		"https://blog.example/y":  blogText,
	}}

	card, err := eval.Score(context.Background(), f.db, f.sess.ID, eval.Options{Citations: reader})
	if err != nil {
		t.Fatal(err)
	}

	m := metric(t, card, "citation accuracy")
	if !m.Regression {
		t.Errorf("a quote attributed to the wrong source did not fail: %s", m.Detail)
	}
	if card.Citations.Mismatch != 1 {
		t.Errorf("mismatch = %d, want 1", card.Citations.Mismatch)
	}
	if card.Citations.Verified != 1 {
		t.Errorf("verified = %d, want 1", card.Citations.Verified)
	}
	if m.Value != 50 {
		t.Errorf("accuracy = %.0f%%, want 50%%", m.Value)
	}
}

// TestCorrectCitationsPass so a mismatch keeps meaning something.
func TestCorrectCitationsPass(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)

	q1 := "It achieves 1.31 bits per byte on the PG-19 benchmark"
	q2 := "byte-level modelling removes tokenizer bias entirely"
	f.addClaims(t,
		claimAt("One.", "https://paper.example/x", q1, int64(strings.Index(paperText, q1))),
		claimAt("Two.", "https://paper.example/x", q2, int64(strings.Index(paperText, q2))),
	)

	reader := &fakeReader{pages: map[string]string{"https://paper.example/x": paperText}}
	card, err := eval.Score(context.Background(), f.db, f.sess.ID, eval.Options{Citations: reader})
	if err != nil {
		t.Fatal(err)
	}

	m := metric(t, card, "citation accuracy")
	if m.Regression || m.Value != 100 {
		t.Errorf("correct citations scored %.0f%% (regression=%v): %s", m.Value, m.Regression, m.Detail)
	}
	// One read for two claims on the same page: a source cited eight times
	// should not cost eight fetches.
	if reader.reads["https://paper.example/x"] != 1 {
		t.Errorf("source read %d times, want 1", reader.reads["https://paper.example/x"])
	}
}

// TestOffsetDriftIsReportedButNotFatal. The citation is sound — the quote is on
// the page — so it is not a fabrication. It still matters, because a later
// re-verification that trusts the offset reads the wrong span.
func TestOffsetDriftIsReportedButNotFatal(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)

	q := "It achieves 1.31 bits per byte on the PG-19 benchmark"
	f.addClaims(t, claimAt("A claim.", "https://paper.example/x", q, 9999))

	reader := &fakeReader{pages: map[string]string{"https://paper.example/x": paperText}}
	card, err := eval.Score(context.Background(), f.db, f.sess.ID, eval.Options{Citations: reader})
	if err != nil {
		t.Fatal(err)
	}

	if card.Citations.OffsetDrift != 1 {
		t.Errorf("offset drift = %d, want 1", card.Citations.OffsetDrift)
	}
	if m := metric(t, card, "citation accuracy"); m.Regression {
		t.Error("offset drift failed the build; the citation itself is sound")
	}
}

// TestUnreachableSourcesDoNotCountAsAccurate. Excluding them is what stops a
// run with no network reporting perfect accuracy.
func TestUnreachableSourcesDoNotCountAsAccurate(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)
	f.addClaims(t,
		claimAt("A claim.", "https://gone.example/x", "a quote long enough to be evidence here", 0),
		claimAt("Another.", "https://gone.example/y", "a quote long enough to be evidence here", 0),
	)

	card, err := eval.Score(context.Background(), f.db, f.sess.ID,
		eval.Options{Citations: &fakeReader{pages: map[string]string{}}})
	if err != nil {
		t.Fatal(err)
	}

	m := metric(t, card, "citation accuracy")
	if m.Status != eval.NotApplicable {
		t.Errorf("status = %s, want n/a — nothing could be checked", m.Status)
	}
	if m.Value == 100 {
		t.Error("an entirely unreachable run reported perfect accuracy")
	}
	if m.Regression {
		t.Error("unreachable sources failed the build; they say nothing about the claim")
	}
	if card.Citations.Unreachable != 2 {
		t.Errorf("unreachable = %d, want 2", card.Citations.Unreachable)
	}
}

// TestProviderSuppliedSourcesAreSkipped. Their text came from the search
// provider's extractor, not ours. Re-fetching runs a different extractor over
// the same page and disagrees on whitespace and boilerplate, so every one would
// report a mismatch that says nothing about the claim.
func TestProviderSuppliedSourcesAreSkipped(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)

	const url = "https://provider.example/x"
	f.addClaims(t, claimAt("A claim.", url, "a quote long enough to be real evidence", 0))

	sid, lid := f.sess.ID, f.lead.ID
	if err := f.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.RecordFetchOutcome(ctx, &store.FetchOutcome{
			SessionID: &sid, LeadID: &lid, URL: url, Domain: "provider.example",
			Outcome: "provider_content",
		})
	}); err != nil {
		t.Fatal(err)
	}

	// A reader that would report a mismatch if it were consulted at all.
	reader := &fakeReader{pages: map[string]string{url: "completely different text"}}
	card, err := eval.Score(ctx, f.db, f.sess.ID, eval.Options{Citations: reader})
	if err != nil {
		t.Fatal(err)
	}

	if card.Citations.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", card.Citations.Skipped)
	}
	if card.Citations.Mismatch != 0 {
		t.Error("a provider-supplied source was re-fetched and reported as a mismatch")
	}
	if reader.reads[url] != 0 {
		t.Errorf("the source was fetched %d times; it should not have been read at all", reader.reads[url])
	}
}

// TestFetchedSourcesAreStillCheckedAlongsideSkippedOnes: the skip must be
// per-URL, not a blanket opt-out whenever any provider content exists.
func TestFetchedSourcesAreStillCheckedAlongsideSkippedOnes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)

	const provided = "https://provider.example/x"
	const fetched = "https://paper.example/y"
	q := "It achieves 1.31 bits per byte on the PG-19 benchmark"

	f.addClaims(t,
		claimAt("From the provider.", provided, "a quote long enough to be real evidence", 0),
		claimAt("From a fetch.", fetched, q, int64(strings.Index(paperText, q))),
	)

	sid, lid := f.sess.ID, f.lead.ID
	if err := f.db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.RecordFetchOutcome(ctx, &store.FetchOutcome{
			SessionID: &sid, LeadID: &lid, URL: provided, Outcome: "provider_content",
		}); err != nil {
			return err
		}
		return tx.RecordFetchOutcome(ctx, &store.FetchOutcome{
			SessionID: &sid, LeadID: &lid, URL: fetched, Outcome: "ok", StatusCode: 200,
		})
	}); err != nil {
		t.Fatal(err)
	}

	reader := &fakeReader{pages: map[string]string{fetched: paperText}}
	card, err := eval.Score(ctx, f.db, f.sess.ID, eval.Options{Citations: reader})
	if err != nil {
		t.Fatal(err)
	}

	if card.Citations.Skipped != 1 || card.Citations.Verified != 1 {
		t.Errorf("skipped=%d verified=%d, want 1 and 1 — the skip must be per-URL",
			card.Citations.Skipped, card.Citations.Verified)
	}
}

// TestCitationsAreOptOutByDefault: re-reading sources costs a fetch per source,
// so a bare `mole eval` must not silently make network calls.
func TestCitationsAreOptOutByDefault(t *testing.T) {
	f := newFixture(t, 3*core.MicrosPerUSD)
	f.spend(t, 100_000, 10_000)
	f.addClaims(t, claimAt("A claim.", "https://a.example", "a quote long enough to be evidence", 0))

	card, err := eval.Score(context.Background(), f.db, f.sess.ID, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if card.Citations != nil {
		t.Error("citations were verified without being asked for")
	}

	m := metric(t, card, "citation accuracy")
	if m.Status != eval.Blocked {
		t.Errorf("status = %s, want blocked", m.Status)
	}
	// The reason must say it is available, not that it is impossible — the
	// difference between "declined" and "not built yet".
	if !strings.Contains(m.Reason, "--citations") {
		t.Errorf("reason does not say how to enable it: %q", m.Reason)
	}
}
