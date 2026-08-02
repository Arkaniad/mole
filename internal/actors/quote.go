package actors

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Quote verification.
//
// §11.5's cheaper half: the extracting actor must emit a verbatim span from the
// source supporting each claim, and a claim whose quote does not appear in that
// source is rejected here — before it reaches the store, the graph, or a
// report.
//
// This is the single most valuable check in the pipeline relative to its cost.
// It is deterministic, needs no model call, and catches the dominant failure
// mode: a fabricated citation attached to a plausible sentence. Consistency
// checking downstream cannot catch that, because a hallucinated claim is
// perfectly consistent with itself.

// QuoteMatch is where a quote was found.
type QuoteMatch struct {
	// Offset is the byte offset into the SOURCE text, not the chunk. Claims
	// must be locatable in the document as a whole, since the chunk that
	// produced them does not survive the actor run.
	Offset int
	// Text is the span exactly as it appears in the source, which may differ
	// from what the model returned in whitespace only.
	Text string
	// Exact is false when the match required whitespace normalization.
	Exact bool
}

// minQuoteLen rejects quotes too short to constitute evidence.
//
// A three-word quote will match somewhere in almost any document by chance,
// so accepting one would make verification theatre rather than a check.
const minQuoteLen = 24

// maxQuoteLen bounds what gets stored. A "quote" that is most of the page is
// not evidence for a specific claim, and §7 budgets a bounded span.
const maxQuoteLen = 1200

// FindQuote locates quote within source.
//
// Tries an exact match first. Falls back to a whitespace-insensitive search,
// because a model re-wrapping a line is a formatting difference rather than a
// fabrication — but the returned span is always the real text from the source,
// never the model's rendering of it.
func FindQuote(source, quote string) (QuoteMatch, bool) {
	quote = strings.TrimSpace(quote)
	if source == "" {
		return QuoteMatch{}, false
	}
	// Measure the quote with its whitespace collapsed. Padding is not evidence,
	// and checking the raw length let "It          achieves" clear a bar that
	// exists to require a substantial span.
	if len(collapseSpace(quote)) < minQuoteLen {
		return QuoteMatch{}, false
	}

	if i := strings.Index(source, quote); i >= 0 {
		return QuoteMatch{Offset: i, Text: quote, Exact: true}, true
	}

	return findNormalized(source, quote)
}

// findNormalized matches ignoring whitespace differences.
//
// Walks the source and the quote in parallel, treating any run of whitespace in
// either as equivalent. Returns the span of the SOURCE that matched, so the
// stored quote is what the document actually says.
func findNormalized(source, quote string) (QuoteMatch, bool) {
	qRunes := []rune(quote)
	qKey := make([]rune, 0, len(qRunes))
	for _, r := range qRunes {
		if unicode.IsSpace(r) {
			if len(qKey) > 0 && qKey[len(qKey)-1] != ' ' {
				qKey = append(qKey, ' ')
			}
			continue
		}
		qKey = append(qKey, unicode.ToLower(r))
	}
	qKey = trimSpaceRunes(qKey)
	if len(qKey) == 0 {
		return QuoteMatch{}, false
	}

	// Index the source once, recording where each normalized rune came from.
	var (
		sKey     = make([]rune, 0, len(source))
		startOff = make([]int, 0, len(source))
		endOff   = make([]int, 0, len(source))
	)
	// Decode explicitly rather than ranging. On invalid UTF-8, range yields
	// RuneError with a width of one byte, but the rune itself encodes to three
	// — so deriving the width from the decoded value walked endOff past the end
	// of the string and panicked on the final slice.
	for i := 0; i < len(source); {
		r, size := utf8.DecodeRuneInString(source[i:])
		switch {
		case unicode.IsSpace(r):
			if len(sKey) > 0 && sKey[len(sKey)-1] != ' ' {
				sKey = append(sKey, ' ')
				startOff = append(startOff, i)
				endOff = append(endOff, i+size)
			}
		default:
			sKey = append(sKey, unicode.ToLower(r))
			startOff = append(startOff, i)
			endOff = append(endOff, i+size)
		}
		i += size
	}

	idx := indexRunes(sKey, qKey)
	if idx < 0 {
		return QuoteMatch{}, false
	}

	start := startOff[idx]
	end := endOff[idx+len(qKey)-1]
	return QuoteMatch{Offset: start, Text: source[start:end], Exact: false}, true
}

func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func trimSpaceRunes(r []rune) []rune {
	for len(r) > 0 && r[0] == ' ' {
		r = r[1:]
	}
	for len(r) > 0 && r[len(r)-1] == ' ' {
		r = r[:len(r)-1]
	}
	return r
}

// collapseSpace reduces every run of whitespace to a single space, which is the
// form both the length check and findNormalized compare against.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// TruncateQuote bounds a quote to maxQuoteLen on a rune boundary.
func TruncateQuote(s string) string {
	if len(s) <= maxQuoteLen {
		return s
	}
	cut := s[:maxQuoteLen]
	for len(cut) > 0 && !isRuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	// Prefer cutting at a word boundary so the stored evidence reads.
	if i := strings.LastIndexByte(cut, ' '); i > maxQuoteLen/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
