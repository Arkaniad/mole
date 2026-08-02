package actors_test

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
)

// TestFindQuoteSurvivesInvalidUTF8: the source is bytes off the network. The
// normalized path derived each rune's width from the decoded value, so an
// invalid byte — decoded as RuneError, which re-encodes to three bytes —
// pushed the recorded offset past the end of the string and panicked on the
// final slice. A malformed page must reject a claim, not kill the actor.
func TestFindQuoteSurvivesInvalidUTF8(t *testing.T) {
	// The trigger is specific: the match must END on an invalid byte. RuneError
	// re-encodes to three bytes, so the recorded end offset ran two bytes past
	// the string and the final slice panicked. Split cuts at byte positions, so
	// a chunk ending mid-character is exactly how this arrives — the model then
	// copies that tail verbatim, as it is told to.
	// Note the doubled space in the quotes below. An exact match never reaches
	// the offset table, so only a re-wrapped quote — the ordinary case the
	// normalized path exists for — gets there.
	const tail = "MambaByte achieves 1.31 bits per byte on PG-19"
	const rewrapped = "MambaByte  achieves 1.31 bits per byte on PG-19"
	cases := []struct{ src, quote string }{
		{tail + "\xff", rewrapped + "\xff"},
		{tail + "\xe6\x97", rewrapped + "\xe6\x97"},
		{tail + "\xff", tail + "\xff"},
		{"\xffValid prose after a bad leading byte here.", "Valid prose after a bad leading byte here."},
		{"Valid opening text. \xff\xfe Some more prose after the bad bytes.", "Some more prose after the bad bytes"},
		{strings.Repeat("\x80", 64), strings.Repeat("\x80", 64)},
		{tail, "a quote that is simply not present anywhere"},
	}

	for _, tc := range cases {
		// The assertion is that this returns rather than panics, and that any
		// offset it hands back actually slices the source.
		m, ok := actors.FindQuote(tc.src, tc.quote)
		if !ok {
			continue
		}
		if m.Offset < 0 || m.Offset+len(m.Text) > len(tc.src) {
			t.Errorf("match points outside the source: offset %d + len %d > %d bytes",
				m.Offset, len(m.Text), len(tc.src))
			continue
		}
		if tc.src[m.Offset:m.Offset+len(m.Text)] != m.Text {
			t.Errorf("returned span is not a slice of the source for %.30q", tc.src)
		}
	}
}

// TestPaddedShortQuotesRejected: minQuoteLen exists so a span too small to be
// evidence cannot pass. Measuring the raw string let a model clear the bar with
// spaces — "It          achieves" is two words and 24 bytes.
func TestPaddedShortQuotesRejected(t *testing.T) {
	src := "It  achieves 1.31 bits per byte. " + strings.Repeat("Filler sentence here. ", 20)

	for _, q := range []string{
		"It          achieves",
		"It\t\t\t\t\t\t\t\t\t\t\tachieves",
		"It \n \n \n \n \n \n \n achieves",
		strings.Repeat(" ", 40) + "the" + strings.Repeat(" ", 40),
	} {
		if _, ok := actors.FindQuote(src, q); ok {
			t.Errorf("accepted a padded short quote as evidence: %q", q)
		}
	}
}

// TestSubstantialQuotesStillPassAfterPaddingCheck: the fix must not start
// rejecting genuine re-wrapped quotes, which is the case the normalized path
// exists to accept.
func TestSubstantialQuotesStillPassAfterPaddingCheck(t *testing.T) {
	src := "MambaByte achieves 1.31 bits per byte on the PG-19 benchmark at 350M parameters."
	q := "MambaByte   achieves 1.31 bits\n\tper byte on the PG-19 benchmark"

	m, ok := actors.FindQuote(src, q)
	if !ok {
		t.Fatal("a substantial re-wrapped quote was rejected")
	}
	if src[m.Offset:m.Offset+len(m.Text)] != m.Text {
		t.Error("returned span is not a slice of the source")
	}
}
