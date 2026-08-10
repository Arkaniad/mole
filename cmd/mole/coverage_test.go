package main

import (
	"testing"

	"github.com/lajosdeme/mole/internal/tools/academic"
)

// M6 review round: the coverage command is the decision gate for building a PDF
// extractor, so the ways it can quietly report a wrong number matter more than
// they would anywhere else.

// TestUnpaywallCanOnlyUpgradeReadability.
//
// Unpaywall's verdict is built from its own record alone. An arXiv paper with a
// journal DOI that Unpaywall has as is_oa false was filed "closed" — while its
// PDF sits openly on arXiv, which the provider already told us. That moves
// papers OUT of pdf_only, the one bucket this command exists to size, in the
// direction of "we don't need a parser".
func TestUnpaywallCanOnlyUpgradeReadability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		known, new academic.FullTextFormat
		wantBetter bool
	}{
		{"closed over pdf_only is a downgrade", academic.FormatPDFOnly, academic.FormatClosed, false},
		{"closed over html is a downgrade", academic.FormatHTML, academic.FormatClosed, false},
		{"pdf_only over html is a downgrade", academic.FormatHTML, academic.FormatPDFOnly, false},
		{"html over pdf_only is an upgrade", academic.FormatPDFOnly, academic.FormatHTML, true},
		{"html over closed is an upgrade", academic.FormatClosed, academic.FormatHTML, true},
		{"same is not better", academic.FormatPDFOnly, academic.FormatPDFOnly, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := better(tc.new, tc.known); got != tc.wantBetter {
				t.Fatalf("better(%q, %q) = %v, want %v", tc.new, tc.known, got, tc.wantBetter)
			}
		})
	}
}

// TestPaperKeyIdentifiesAPaperAcrossProvidersAndQuestions. rep.Papers is
// len(rep.Rows), so a paper counted twice moves every percentage.
func TestPaperKeyIdentifiesAPaperAcrossProvidersAndQuestions(t *testing.T) {
	arxivSide := academic.Paper{DOI: "10.1234/Same", ArXivID: "2401.1", Source: academic.KindArXiv}
	pubmedSide := academic.Paper{DOI: "10.1234/same", PMID: "99", Source: academic.KindPubMed}
	if paperKey(arxivSide) != paperKey(pubmedSide) {
		t.Errorf("one paper on two providers produced two keys: %q vs %q",
			paperKey(arxivSide), paperKey(pubmedSide))
	}

	// Different papers must stay distinct, or deduplication would eat the
	// corpus instead of tidying it.
	a := academic.Paper{PMID: "1"}
	b := academic.Paper{PMID: "2"}
	if paperKey(a) == paperKey(b) {
		t.Error("two different papers collapsed into one key")
	}
	if paperKey(academic.Paper{}) != "" {
		t.Error("a paper with no identifier and no title should key to empty")
	}
}

// TestReadabilityOrderIsTotal. better() is an ordering; if two formats compared
// equal in both directions the "upgrade only" rule would silently permit either.
func TestReadabilityOrderIsTotal(t *testing.T) {
	all := []academic.FullTextFormat{academic.FormatHTML, academic.FormatPDFOnly, academic.FormatClosed}
	for _, x := range all {
		for _, y := range all {
			if x == y {
				continue
			}
			if better(x, y) == better(y, x) {
				t.Errorf("%q and %q are not ordered against each other", x, y)
			}
		}
	}
}
