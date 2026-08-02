package llm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Chunking.
//
// A 300-page PDF does not fit a context window, and §4.1 makes splitting the
// actor's job. Two properties matter more than elegance here:
//
//  1. Chunks must be contiguous slices of the source text, with byte offsets
//     recorded. §11.5 verifies a claim's quote verbatim against the source, so
//     a chunk that paraphrased, reordered, or normalized its text would make
//     grounded claims unverifiable.
//  2. Splits should land on paragraph then sentence boundaries. A claim whose
//     evidence straddles a mid-sentence split cannot be extracted from either
//     half.

// Chunk is one contiguous span of the source.
type Chunk struct {
	Text  string
	Start int // byte offset into the source
	End   int
	Index int
}

// ChunkOptions tune splitting.
type ChunkOptions struct {
	// MaxChars is the target ceiling per chunk. Characters rather than tokens
	// because the tokenizer differs per provider and an estimate that runs
	// long produces a request rejection rather than a slightly larger bill.
	MaxChars int
	// OverlapChars repeats the tail of the previous chunk. A claim near a
	// boundary is otherwise visible in neither chunk with full context.
	OverlapChars int
	// MinChars avoids emitting a final sliver that costs a model call and
	// carries nothing.
	MinChars int
}

// DefaultChunkOptions targets roughly 6k tokens of input per chunk on typical
// English prose, leaving generous room under any current context window.
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxChars: 24_000, OverlapChars: 800, MinChars: 200}
}

func (o ChunkOptions) withDefaults() ChunkOptions {
	d := DefaultChunkOptions()
	if o.MaxChars <= 0 {
		o.MaxChars = d.MaxChars
	}
	if o.OverlapChars < 0 {
		o.OverlapChars = 0
	}
	if o.OverlapChars >= o.MaxChars {
		o.OverlapChars = o.MaxChars / 4
	}
	if o.MinChars <= 0 {
		o.MinChars = d.MinChars
	}
	return o
}

// Split divides text into chunks.
//
// Offsets index the ORIGINAL string, so a quote found in a chunk can be
// located in the source without re-searching — which is what keeps quote
// verification exact for documents that were never held in one context.
func Split(text string, opts ChunkOptions) []Chunk {
	opts = opts.withDefaults()

	if len(text) == 0 {
		return nil
	}
	if len(text) <= opts.MaxChars {
		return []Chunk{{Text: text, Start: 0, End: len(text), Index: 0}}
	}

	var chunks []Chunk
	pos := 0

	for pos < len(text) {
		end := pos + opts.MaxChars
		if end >= len(text) {
			end = len(text)
		} else {
			end = boundaryBefore(text, pos, end)
		}
		if end <= pos {
			// boundaryBefore must never fail to advance, but a chunker that can
			// hang on malformed input is worse than one that splits it badly.
			// This is the backstop, not the fix.
			end = pos + opts.MaxChars
			if end > len(text) {
				end = len(text)
			}
		}

		chunk := text[pos:end]
		// Trailing whitespace would shift the recorded offsets relative to the
		// text actually sent, so trim symmetrically and adjust both ends.
		//
		// Both trims must agree on what whitespace is. A three-character cutset
		// against TrimSpace's unicode.IsSpace silently disagreed on \r, \v, \f
		// and NBSP, and every byte of disagreement shifts Start — which is the
		// offset a stored quote is later located by (§11.5).
		lead := len(chunk) - len(strings.TrimLeftFunc(chunk, unicode.IsSpace))
		trimmed := strings.TrimSpace(chunk)

		if len(trimmed) >= opts.MinChars || end >= len(text) {
			if trimmed != "" {
				chunks = append(chunks, Chunk{
					Text:  trimmed,
					Start: pos + lead,
					End:   pos + lead + len(trimmed),
					Index: len(chunks),
				})
			}
		}

		if end >= len(text) {
			break
		}

		// Align the overlap rewind to a rune boundary. Subtracting a raw byte
		// count can land inside a multibyte character, which makes the NEXT
		// chunk start mid-rune — a failure that only shows up on non-Latin
		// text and is invisible in ASCII fixtures.
		next := alignRune(text, end-opts.OverlapChars)
		if next <= pos {
			// Guard against no forward progress when a single "paragraph"
			// exceeds MaxChars and the boundary search returns the start.
			next = end
		}
		pos = next
	}

	return chunks
}

// boundaryBefore finds the latest natural break at or before limit.
//
// Paragraph breaks are preferred, then sentence ends, then any whitespace. It
// never returns a position at or before start, so splitting always advances.
func boundaryBefore(text string, start, limit int) int {
	if limit <= start {
		return limit
	}
	// Only look back over the last quarter of the window; a break far earlier
	// would waste most of the chunk. Aligned so the rune-wise scan below does
	// not begin on a continuation byte and see a replacement char.
	floor := alignRune(text, start+(limit-start)*3/4)
	if floor < start {
		floor = start
	}

	if i := strings.LastIndex(text[floor:limit], "\n\n"); i >= 0 {
		return floor + i + 2
	}
	if i := lastSentenceEnd(text[floor:limit]); i >= 0 {
		return floor + i
	}
	if i := strings.LastIndexFunc(text[floor:limit], unicode.IsSpace); i >= 0 {
		return floor + i + 1
	}

	// No break available — a base64 blob, a minified line, or CJK prose with
	// no spaces. Cut at the limit, but back up to a rune boundary first.
	//
	// The test is on the byte AT limit, not before it: text[pos:limit] is valid
	// only when the first EXCLUDED byte starts a rune. Checking limit-1 instead
	// happily slices a three-byte character into pieces.
	cut := limit
	for cut > start && cut < len(text) && !isRuneStartByte(text[cut]) {
		cut--
	}
	if cut <= start {
		// Every byte in the window is a continuation byte, so the input is not
		// valid UTF-8. Returning start would make Split emit an empty chunk and
		// re-enter at the same position forever. Cut at the original limit
		// instead: malformed input gets a malformed split, not a hang.
		return limit
	}
	return cut
}

// lastSentenceEnd returns the index just past the final sentence terminator.
//
// Iterates runes rather than bytes so CJK terminators are recognized. Without
// them, Japanese and Chinese prose has no ASCII period and no spaces, so every
// split would fall through to the byte-truncation path above — which is both
// worse placement and the path where a rune-boundary bug hides.
func lastSentenceEnd(s string) int {
	best := -1
	for i, r := range s {
		switch r {
		case '.', '!', '?':
			// Require following whitespace, so "1.31 BPB" and "e.g." are not
			// mistaken for sentence ends and split a claim in half.
			next := i + utf8.RuneLen(r)
			if next < len(s) && isSpaceByte(s[next]) {
				best = next
			}
		case '。', '！', '？', '．', '…':
			// CJK terminators are not followed by a space.
			best = i + utf8.RuneLen(r)
		}
	}
	return best
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\n' || b == '\t' || b == '\r'
}

func isRuneStartByte(b byte) bool { return b&0xC0 != 0x80 }

// alignRune moves i back to the nearest rune boundary, so slicing at i cannot
// bisect a character.
func alignRune(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !isRuneStartByte(s[i]) {
		i--
	}
	return i
}

// ---------------------------------------------------------------------------
// Budgeted map-reduce
// ---------------------------------------------------------------------------

// EstimateTokens approximates a token count from characters.
//
// Deliberately rough and deliberately high: it is used to decide how many
// chunks fit a sub-budget BEFORE any call is made, and over-estimating stops
// early while under-estimating overspends. Actual charges always come from the
// provider's reported usage, never from this.
func EstimateTokens(chars int) int64 {
	// ~3.5 chars/token on English prose; rounding down the divisor rounds the
	// estimate up.
	return int64(chars/3) + 1
}

// MapResult is one chunk's output.
type MapResult struct {
	Chunk Chunk
	Text  string
	Usage Usage
	Err   error
}

// MapReduce is the plan for summarizing a document that does not fit.
//
// It is a plan rather than an executor because the actor owns the budget: the
// actor asks how many chunks it can afford, runs that many, and records
// Truncated rather than silently dropping content or blowing the reservation
// (§4.1).
type MapReduce struct {
	Chunks []Chunk
	// Truncated is set when the sub-budget could not cover every chunk.
	Truncated bool
	// Skipped counts chunks dropped for budget.
	Skipped int
}

// Plan splits text and decides how much of it the sub-budget can cover.
//
// maxInputTokens is the sub-budget derived from the lead's reservation. When
// the document exceeds it, the highest-value chunks are kept — which here means
// the earliest, since article structure front-loads the substance and a
// truncation that kept the middle would read as incoherent.
func Plan(text string, maxInputTokens int64, opts ChunkOptions) MapReduce {
	chunks := Split(text, opts)
	if maxInputTokens <= 0 {
		return MapReduce{Chunks: chunks}
	}

	var (
		used int64
		kept []Chunk
	)
	for _, c := range chunks {
		cost := EstimateTokens(len(c.Text))
		if used+cost > maxInputTokens && len(kept) > 0 {
			break
		}
		used += cost
		kept = append(kept, c)
	}

	return MapReduce{
		Chunks:    kept,
		Truncated: len(kept) < len(chunks),
		Skipped:   len(chunks) - len(kept),
	}
}
