package llm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/llm"
)

// TestOffsetsSurviveExoticWhitespace: Chunk.Start is what a stored quote is
// later located by (§11.5). The trims at either end of a chunk have to agree on
// what whitespace is — a three-character cutset against TrimSpace's
// unicode.IsSpace disagreed on \r, \v, \f and NBSP, and every byte of
// disagreement shifted the offset. Provider-supplied page text is full of them.
func TestOffsetsSurviveExoticWhitespace(t *testing.T) {
	for name, pad := range map[string]string{
		"nbsp":         " ",
		"carriage":     "\r",
		"vertical-tab": "\v",
		"form-feed":    "\f",
		"ideographic":  "　",
		"narrow-nbsp":  " ",
	} {
		body := strings.Repeat("Sentence with substance. ", 400)
		text := pad + pad + body + pad + pad + body

		chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 2000, OverlapChars: 100, MinChars: 50})
		if len(chunks) < 2 {
			t.Fatalf("%s: expected several chunks, got %d", name, len(chunks))
		}
		for _, c := range chunks {
			if c.Start < 0 || c.End > len(text) || c.Start > c.End {
				t.Fatalf("%s: chunk %d has impossible bounds [%d,%d]", name, c.Index, c.Start, c.End)
			}
			// The contract: offsets index the ORIGINAL text and the recorded
			// span is exactly what the model was shown.
			if got := text[c.Start:c.End]; got != c.Text {
				t.Errorf("%s: chunk %d offsets point at %.40q, but the text sent was %.40q",
					name, c.Index, got, c.Text)
			}
		}
	}
}

// TestSplitTerminatesOnInvalidUTF8: boundaryBefore could walk its cut position
// back to the chunk start, at which point Split emitted an empty chunk and
// re-entered at the same offset forever. A document that hangs the chunker is
// worse than one that splits badly, and invalid UTF-8 arrives from the network
// as a matter of course.
func TestSplitTerminatesOnInvalidUTF8(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		// Continuation bytes only: no rune starts anywhere in the window.
		text := strings.Repeat("\x80", 20_000)
		done <- len(llm.Split(text, llm.ChunkOptions{MaxChars: 1000, OverlapChars: 100, MinChars: 10}))
	}()

	select {
	case n := <-done:
		if n == 0 {
			t.Error("no chunks produced from malformed input")
		}
	case <-timeout():
		t.Fatal("Split did not terminate on invalid UTF-8")
	}
}

// TestSplitTerminatesOnTruncatedMultibyte covers the realistic version: a page
// cut mid-character by a byte-range read.
func TestSplitTerminatesOnTruncatedMultibyte(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		text := strings.Repeat("日本語のテキスト", 500) + "\xe6\x97" // trailing partial rune
		done <- len(llm.Split(text, llm.ChunkOptions{MaxChars: 900, OverlapChars: 80, MinChars: 40}))
	}()

	select {
	case n := <-done:
		if n < 2 {
			t.Errorf("got %d chunks, want several", n)
		}
	case <-timeout():
		t.Fatal("Split did not terminate on truncated multibyte text")
	}
}

// TestChunksCoverTheDocument: overlap and trimming must not silently drop a
// region. A claim in a lost span is simply never found, with no error anywhere.
func TestChunksCoverTheDocument(t *testing.T) {
	text := strings.Repeat("Paragraph one carries meaning.\n\nParagraph two also does.\n\n", 200)
	chunks := llm.Split(text, llm.ChunkOptions{MaxChars: 1500, OverlapChars: 200, MinChars: 50})

	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	prevEnd := 0
	for _, c := range chunks {
		if c.Start > prevEnd {
			t.Errorf("gap in coverage: bytes [%d,%d) appear in no chunk", prevEnd, c.Start)
		}
		if c.End > prevEnd {
			prevEnd = c.End
		}
	}
	// Trailing whitespace is trimmed off the last chunk by design; anything
	// else left over is content that reached no chunk.
	if rest := strings.TrimSpace(text[prevEnd:]); rest != "" {
		t.Errorf("coverage stops at %d of %d bytes, dropping %.40q", prevEnd, len(text), rest)
	}
}

func timeout() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-time.After(5 * time.Second)
		close(ch)
	}()
	return ch
}
