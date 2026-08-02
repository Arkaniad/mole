package llm_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lajosdeme/mole/internal/llm"
)

func para(n int) string {
	return strings.Repeat("This is a sentence of moderate length used as filler. ", n)
}

// TestChunkOffsetsIndexTheSource is the property quote verification depends on:
// a chunk's text must appear at its recorded offsets in the original, so a
// quote found in a chunk can be located in the source without re-searching.
func TestChunkOffsetsIndexTheSource(t *testing.T) {
	text := strings.Join([]string{para(40), para(40), para(40), para(40)}, "\n\n")

	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 2000, OverlapChars: 200})
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}

	for _, c := range chunks {
		if c.Start < 0 || c.End > len(text) || c.Start >= c.End {
			t.Fatalf("chunk %d has bad offsets [%d,%d) for a %d-byte source",
				c.Index, c.Start, c.End, len(text))
		}
		if got := text[c.Start:c.End]; got != c.Text {
			t.Errorf("chunk %d text does not match its offsets:\n got %.60q\nwant %.60q",
				c.Index, got, c.Text)
		}
	}
}

func TestChunkCoversWholeDocument(t *testing.T) {
	text := strings.Join([]string{para(30), para(30), para(30)}, "\n\n")
	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 1500, OverlapChars: 0})

	// With no overlap the chunks should tile the document with no gap large
	// enough to lose a sentence.
	prevEnd := 0
	for _, c := range chunks {
		if c.Start > prevEnd+2 {
			t.Errorf("gap between %d and %d — content dropped", prevEnd, c.Start)
		}
		prevEnd = c.End
	}
	if len(text)-prevEnd > 2 {
		t.Errorf("tail of the document was dropped: %d bytes unread", len(text)-prevEnd)
	}
}

func TestChunkPrefersParagraphBoundaries(t *testing.T) {
	a := para(20)
	b := para(20)
	text := a + "\n\n" + b

	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: len(a) + 50, OverlapChars: 0})
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2", len(chunks))
	}
	// The first chunk should end at the paragraph break, not mid-sentence.
	if strings.HasSuffix(strings.TrimSpace(chunks[0].Text), "of") ||
		!strings.HasSuffix(strings.TrimSpace(chunks[0].Text), ".") {
		t.Errorf("first chunk ends mid-sentence: %.80q", tail(chunks[0].Text))
	}
}

// TestChunkNeverSplitsMidSentenceWhenAvoidable: a claim whose evidence
// straddles a mid-sentence split cannot be extracted from either half.
func TestChunkNeverSplitsMidSentenceWhenAvoidable(t *testing.T) {
	text := para(200)
	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 3000, OverlapChars: 0})

	for i, c := range chunks[:len(chunks)-1] {
		trimmed := strings.TrimSpace(c.Text)
		if !strings.HasSuffix(trimmed, ".") {
			t.Errorf("chunk %d ends mid-sentence: %q", i, tail(trimmed))
		}
	}
}

func TestChunkOverlapRepeatsContext(t *testing.T) {
	text := para(100)
	const overlap = 300

	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 2000, OverlapChars: overlap})
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	// Consecutive chunks should overlap in the source, so a claim near a
	// boundary appears with context in at least one of them.
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Start >= chunks[i-1].End {
			t.Errorf("chunks %d and %d do not overlap (%d >= %d)",
				i-1, i, chunks[i].Start, chunks[i-1].End)
		}
	}
}

func TestChunkHandlesUnbreakableContent(t *testing.T) {
	// A single enormous token — a base64 blob or a minified line — has no
	// natural boundary anywhere.
	text := strings.Repeat("A", 10_000)
	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 1000, OverlapChars: 0})

	if len(chunks) < 2 {
		t.Fatalf("unbreakable content produced %d chunks — splitting stalled", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Errorf("chunk %d is not valid UTF-8", c.Index)
		}
	}
}

func TestChunkPreservesMultibyteRunes(t *testing.T) {
	text := strings.Repeat("日本語のテキストです。これは分割のテストです。", 200)
	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 1000, OverlapChars: 100})

	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d split a multibyte rune", c.Index)
		}
		if text[c.Start:c.End] != c.Text {
			t.Errorf("chunk %d offsets wrong for multibyte text", c.Index)
		}
	}
}

func TestShortTextIsOneChunk(t *testing.T) {
	text := "Just a short document."
	chunks := llm.Split(text, llm.DefaultChunkOptions())
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if chunks[0].Text != text || chunks[0].Start != 0 || chunks[0].End != len(text) {
		t.Errorf("short text mangled: %+v", chunks[0])
	}
}

func TestEmptyTextProducesNoChunks(t *testing.T) {
	if got := llm.Split("", llm.DefaultChunkOptions()); len(got) != 0 {
		t.Errorf("got %d chunks for empty text", len(got))
	}
}

// TestPlanRespectsSubBudget is §4.1's rule: a document too large for the
// lead's reservation is truncated with a flag, not silently dropped and not
// allowed to blow the budget.
func TestPlanRespectsSubBudget(t *testing.T) {
	text := para(2000) // well beyond any single call

	full := llm.Plan(text, 0, llm.ChunkOptions{MaxChars: 4000})
	if full.Truncated {
		t.Error("unlimited budget reported truncation")
	}

	// A budget covering roughly two chunks.
	twoChunks := llm.EstimateTokens(8000)
	limited := llm.Plan(text, twoChunks, llm.ChunkOptions{MaxChars: 4000})

	if !limited.Truncated {
		t.Fatal("constrained budget did not report truncation")
	}
	if limited.Skipped <= 0 {
		t.Error("truncated plan reported no skipped chunks")
	}
	if len(limited.Chunks) >= len(full.Chunks) {
		t.Errorf("kept %d of %d chunks — budget not applied",
			len(limited.Chunks), len(full.Chunks))
	}

	// The estimate must not exceed the budget it was given.
	var est int64
	for _, c := range limited.Chunks {
		est += llm.EstimateTokens(len(c.Text))
	}
	if est > twoChunks {
		t.Errorf("plan estimated %d tokens against a %d budget", est, twoChunks)
	}
}

// TestPlanAlwaysKeepsOneChunk: a budget too small for even one chunk should
// still attempt the document rather than return nothing and report success.
func TestPlanAlwaysKeepsOneChunk(t *testing.T) {
	plan := llm.Plan(para(500), 1, llm.ChunkOptions{MaxChars: 4000})
	if len(plan.Chunks) == 0 {
		t.Fatal("tiny budget produced no chunks at all")
	}
	if !plan.Truncated {
		t.Error("tiny budget did not report truncation")
	}
}

// TestEstimateRoundsUp: the estimate gates spending before any call is made,
// so it must never be optimistic.
func TestEstimateRoundsUp(t *testing.T) {
	if got := llm.EstimateTokens(0); got < 1 {
		t.Errorf("EstimateTokens(0) = %d, want >= 1", got)
	}
	// Roughly 3-4 chars per token; the estimate should sit at or above the
	// realistic count rather than below it.
	if got := llm.EstimateTokens(3500); got < 875 {
		t.Errorf("EstimateTokens(3500) = %d, suspiciously low", got)
	}
}

func tail(s string) string {
	if len(s) <= 40 {
		return s
	}
	return "…" + s[len(s)-40:]
}
