package actors_test

import (
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/llm"
)

// TestChunksFitThePerRequestCap. A context window is not the only ceiling. A
// provider's per-minute token allowance rejects an oversized request with 413
// no matter how much budget is left, and the default chunk (~8k tokens) is
// larger than a Groq free tier's 6k TPM. Sized wrong, every chunk of every
// source fails and the run reports success having read nothing — which is
// exactly what the first live run did.
func TestChunksFitThePerRequestCap(t *testing.T) {
	for _, cap := range []int64{6000, 4000, 2000, 16000} {
		b := actors.Budget{MaxChunkTokens: cap}
		opts := b.ChunkOptions()

		// Every chunk the splitter can emit must estimate under the cap, with
		// room left for the prompt wrapped around it.
		if est := llm.EstimateTokens(opts.MaxChars); est >= cap {
			t.Errorf("cap %d: a full chunk estimates at %d tokens, leaving nothing for the prompt",
				cap, est)
		}
		if opts.OverlapChars >= opts.MaxChars {
			t.Errorf("cap %d: overlap %d >= max %d", cap, opts.OverlapChars, opts.MaxChars)
		}
		if opts.MaxChars <= 0 || opts.MinChars <= 0 {
			t.Errorf("cap %d: degenerate options %+v", cap, opts)
		}
	}
}

// TestRealDocumentSplitsUnderTheCap is the end-to-end version: run the splitter
// and check every chunk it actually produced, not just the configured ceiling.
func TestRealDocumentSplitsUnderTheCap(t *testing.T) {
	const cap = 6000 // Groq free tier
	text := longArticle(400)

	opts := actors.Budget{MaxChunkTokens: cap}.ChunkOptions()
	chunks := llm.Split(text, opts)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunk(s); the fixture must be long enough to split", len(chunks))
	}
	for _, c := range chunks {
		if est := llm.EstimateTokens(len(c.Text)); est >= cap {
			t.Errorf("chunk %d estimates at %d tokens against a %d cap", c.Index, est, cap)
		}
	}
}

// TestNoCapKeepsTheDefault: the cap is opt-in, and a provider with a large
// context window should not be chunked into uselessly small pieces.
func TestNoCapKeepsTheDefault(t *testing.T) {
	got := actors.Budget{}.ChunkOptions()
	want := llm.DefaultChunkOptions()
	if got != want {
		t.Errorf("with no cap set, options = %+v, want the default %+v", got, want)
	}

	// A cap larger than the default must also leave the default alone rather
	// than inflating chunks past what the splitter was tuned for.
	if big := (actors.Budget{MaxChunkTokens: 1_000_000}).ChunkOptions(); big.MaxChars != want.MaxChars {
		t.Errorf("a large cap changed MaxChars to %d, want the default %d", big.MaxChars, want.MaxChars)
	}
}

// TestAbsurdlySmallCapStillProducesUsableChunks rather than dividing by zero or
// emitting single characters.
func TestAbsurdlySmallCapStillProducesUsableChunks(t *testing.T) {
	opts := actors.Budget{MaxChunkTokens: 1}.ChunkOptions()
	if opts.MaxChars <= 0 {
		t.Fatalf("degenerate MaxChars %d", opts.MaxChars)
	}
	chunks := llm.Split(longArticle(20), opts)
	if len(chunks) == 0 {
		t.Error("no chunks produced")
	}
}
