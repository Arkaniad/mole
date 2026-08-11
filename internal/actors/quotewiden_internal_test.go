package actors

// In-package, because the property that needs checking is an OFFSET into the
// passage, and from outside the package a test only sees the prompt that wraps it.
// An offset that points at the wrong place makes the stored provenance a lie: an
// auditor re-reading the source lands somewhere else.

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
)

const verdictPassage = `Aggregate over 400 record(s).

Columns:
  bucket (text), values from "east" to "west", 4 distinct, 0 null

Groups:
  west — 100 records, mean 141.97
  south — 100 records, mean 78.30

Statistical tests:
  the mean in "west" is higher than in "south" by 63.67 (means 141.97 and 78.30; n = 100 and 100); statistically significant (p = <0.001), Welch t = 20.13, effect size 2.85 (large), 95% CI 57.41 to 69.93, unadjusted p = <0.001 over 6 pairwise comparisons; the difference points the same way in all 3 holdout windows
`

func TestTheWidenedOffsetPointsAtTheQuote(t *testing.T) {
	truncated := `the mean in "west" is higher than in "south" by 63.67 ` +
		`(means 141.97 and 78.30; n = 100 and 100); statistically significant (p = <0.001)`
	if !strings.Contains(verdictPassage, truncated) {
		t.Fatal("the fixture's truncated quote is not in the fixture's passage")
	}

	got := widenToQualifiedLine(verdictPassage, []core.Claim{{
		Text:  "Spend differs between the regions.",
		Quote: truncated,
	}})
	if len(got) != 1 {
		t.Fatalf("%d claim(s) back, want 1", len(got))
	}
	c := got[0]

	if !strings.Contains(c.Quote, "holdout windows") {
		t.Errorf("the quote was not widened to the whole sentence:\n%q", c.Quote)
	}
	off := int(c.QuoteOffset)
	if off < 0 || off+len(c.Quote) > len(verdictPassage) {
		t.Fatalf("offset %d is outside the passage", off)
	}
	if verdictPassage[off:off+len(c.Quote)] != c.Quote {
		t.Errorf("offset %d points at %q, not at the quote",
			off, verdictPassage[off:off+len(c.Quote)])
	}
}

// TestAQuoteThatAlreadyCarriesTheWholeSentenceIsLeftAlone. Widening an untruncated
// quote would be work with no effect, and a changed offset for no reason.
func TestAQuoteThatAlreadyCarriesTheWholeSentenceIsLeftAlone(t *testing.T) {
	var full string
	for _, line := range strings.Split(verdictPassage, "\n") {
		if strings.Contains(line, "statistically significant") {
			full = strings.TrimSpace(line)
		}
	}
	if full == "" {
		t.Fatal("fixture has no verdict line")
	}

	got := widenToQualifiedLine(verdictPassage, []core.Claim{{Quote: full, QuoteOffset: 42}})
	if got[0].Quote != full {
		t.Errorf("a complete quote was rewritten:\n%q", got[0].Quote)
	}
	if got[0].QuoteOffset != 42 {
		t.Errorf("offset moved to %d for a quote that did not change", got[0].QuoteOffset)
	}
}

// TestANonVerdictQuoteIsUntouched.
func TestANonVerdictQuoteIsUntouched(t *testing.T) {
	got := widenToQualifiedLine(verdictPassage, []core.Claim{{
		Quote: `west — 100 records`, QuoteOffset: 7,
	}})
	if got[0].Quote != `west — 100 records` {
		t.Errorf("a bucket quote was widened: %q", got[0].Quote)
	}
	if got[0].QuoteOffset != 7 {
		t.Errorf("offset moved to %d", got[0].QuoteOffset)
	}
}

// TestAnUnderpoweredVerdictIsAlsoWidened. The three verdict wordings must be
// treated alike, or the one that says "this is not evidence" is the one that loses
// its qualification.
func TestAnUnderpoweredVerdictIsAlsoWidened(t *testing.T) {
	passage := "Statistical tests:\n  the mean in \"a\" is higher than in \"b\" by 5 " +
		"(means 10 and 5; n = 6 and 6); UNDERPOWERED — fewer than 20 records, so this " +
		"is not evidence either way\n"
	got := widenToQualifiedLine(passage, []core.Claim{{
		Quote: `the mean in "a" is higher than in "b" by 5 (means 10 and 5; n = 6 and 6)`,
	}})
	if !strings.Contains(got[0].Quote, "not evidence either way") {
		t.Errorf("an underpowered verdict kept its truncated quote:\n%q", got[0].Quote)
	}
}
