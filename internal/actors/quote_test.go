package actors_test

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
)

const source = `MambaByte is a token-free selective state space model. ` +
	`It achieves 1.31 bits per byte on the PG-19 benchmark at 350M parameters, ` +
	`outperforming comparable subword transformers at equal compute.
The authors note that byte-level modelling removes tokenizer bias entirely.`

func TestExactQuoteIsFound(t *testing.T) {
	q := "It achieves 1.31 bits per byte on the PG-19 benchmark"

	m, ok := actors.FindQuote(source, q)
	if !ok {
		t.Fatal("exact quote not found")
	}
	if !m.Exact {
		t.Error("exact match not reported as exact")
	}
	if got := source[m.Offset : m.Offset+len(m.Text)]; got != q {
		t.Errorf("offset points at %q, want %q", got, q)
	}
}

// TestFabricatedQuoteIsRejected is the whole point. A hallucinated claim is
// perfectly self-consistent, so no downstream consistency check catches it —
// only the fact that its evidence does not exist in the source.
func TestFabricatedQuoteIsRejected(t *testing.T) {
	fabrications := []string{
		"It achieves 0.98 bits per byte on the PG-19 benchmark",     // number changed
		"MambaByte outperforms GPT-4 on every published benchmark",  // invented wholesale
		"The authors conclude that tokenizers should be abandoned.", // plausible paraphrase
	}
	for _, q := range fabrications {
		if m, ok := actors.FindQuote(source, q); ok {
			t.Errorf("fabricated quote accepted at offset %d: %q", m.Offset, q)
		}
	}
}

// TestWhitespaceDifferencesTolerated: a model re-wrapping a line is a
// formatting difference, not a fabrication. Rejecting these would throw away
// genuinely grounded claims.
func TestWhitespaceDifferencesTolerated(t *testing.T) {
	q := "It  achieves 1.31 bits\n  per byte on the PG-19   benchmark"

	m, ok := actors.FindQuote(source, q)
	if !ok {
		t.Fatal("re-wrapped quote rejected")
	}
	if m.Exact {
		t.Error("normalized match reported as exact")
	}

	// The stored span must be the SOURCE's text, not the model's rendering —
	// otherwise the archive records something the page never said.
	if got := source[m.Offset : m.Offset+len(m.Text)]; got != m.Text {
		t.Errorf("returned text is not a slice of the source")
	}
	if strings.Contains(m.Text, "  ") || strings.Contains(m.Text, "\n  ") {
		t.Errorf("stored the model's spacing rather than the source's: %q", m.Text)
	}
	if !strings.Contains(m.Text, "1.31 bits per byte") {
		t.Errorf("matched span is wrong: %q", m.Text)
	}
}

func TestCaseInsensitiveFallback(t *testing.T) {
	q := "IT ACHIEVES 1.31 BITS PER BYTE ON THE PG-19 BENCHMARK"
	m, ok := actors.FindQuote(source, q)
	if !ok {
		t.Fatal("case-variant quote rejected")
	}
	if !strings.Contains(m.Text, "1.31 bits per byte") {
		t.Errorf("matched the wrong span: %q", m.Text)
	}
}

// TestShortQuotesRejected: a three-word span matches almost any document by
// chance, so accepting one would make verification theatre.
func TestShortQuotesRejected(t *testing.T) {
	for _, q := range []string{"", "the", "It achieves", "byte on"} {
		if _, ok := actors.FindQuote(source, q); ok {
			t.Errorf("accepted quote too short to be evidence: %q", q)
		}
	}
}

func TestOffsetsSurviveMultibyteText(t *testing.T) {
	src := "序文です。MambaByteは1.31ビット毎バイトを達成しました。これは重要な結果です。"
	q := "MambaByteは1.31ビット毎バイトを達成しました"

	m, ok := actors.FindQuote(src, q)
	if !ok {
		t.Fatal("multibyte quote not found")
	}
	if got := src[m.Offset : m.Offset+len(m.Text)]; got != m.Text {
		t.Errorf("offset does not slice cleanly: got %q", got)
	}
	if !strings.Contains(m.Text, "1.31") {
		t.Errorf("wrong span: %q", m.Text)
	}
}

func TestQuoteTruncationIsSafe(t *testing.T) {
	long := strings.Repeat("This is a long sentence that keeps going. ", 200)
	got := actors.TruncateQuote(long)

	if len(got) > 1200 {
		t.Errorf("truncated quote is %d bytes, want <= 1200", len(got))
	}
	if strings.HasSuffix(got, " ") {
		t.Error("truncation left trailing whitespace")
	}
	// Short quotes pass through untouched.
	short := "A brief but sufficient piece of evidence."
	if actors.TruncateQuote(short) != short {
		t.Error("short quote was modified")
	}
}

func TestEmptySourceNeverMatches(t *testing.T) {
	if _, ok := actors.FindQuote("", "any quote at all that is long enough"); ok {
		t.Error("matched against an empty source")
	}
}
